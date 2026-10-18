// Package dream implements AutoDream — the background memory consolidation
// system that periodically reviews session history and distills knowledge
// into topic files.
//
// The package is designed to be invoked from any execution context (channel
// mode SystemScheduler, CLI command, TUI hook, etc.). It owns the gate logic,
// lock management, session grouping, and state persistence — but delegates
// the actual LLM sub-agent execution to a caller-provided RunFunc.
package dream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/agent/memory"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/container"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/pkg/syncx"
	"github.com/monsterxx03/tachi/session"
)

// MaxConcurrentDreams limits parallel dream sub-agents across all domains.
const MaxConcurrentDreams = 3

// DefaultMaxSessionsPerRun is the batch size when none is configured. Small on
// purpose: each session contributes up to MaxSummaryChars of conversation, and
// a prompt that overflows is a wasted run, not a partial one.
const DefaultMaxSessionsPerRun = 8

// State records the last successful dream execution for a memory domain.
type State struct {
	LastDreamAt     time.Time                    `json:"last_dream_at"`
	SessionsDreamed int                          `json:"sessions_dreamed"`
	TopicsCreated   int                          `json:"topics_created"`
	FactsAdded      int                          `json:"facts_added"`
	FactsSuperseded int                          `json:"facts_superseded"`
	FactsPruned     int                          `json:"facts_pruned"`
	Errors          []string                     `json:"errors,omitempty"`
	FactStates      map[string]*memory.FactState `json:"fact_states,omitempty"`

	// SessionWatermarks records, per session, how far a dream has read. It is
	// what lets ONE run take a bounded batch without skipping the rest: the
	// old single LastDreamAt could only move forward, so a run that could not
	// fit every active session (a months-long backlog, or a long outage) would
	// either overflow the prompt or mark the unprocessed sessions done forever.
	//
	// A missing entry falls back to LastDreamAt, which is exactly the old
	// behaviour for state written before this field existed.
	SessionWatermarks map[string]time.Time `json:"session_watermarks,omitempty"`

	// Backlog is true when the last run hit the per-run session cap and left
	// active sessions behind. The cooldown gate ignores it as long as it is
	// set, so a backlog drains at the caller's tick rate instead of at
	// min_interval.
	Backlog bool `json:"backlog,omitempty"`
}

// Status provides a snapshot of the orchestrator's current dream execution state.
type Status struct {
	Running int            // number of domains currently being dreamed
	Domains []DomainStatus // state per domain (in progress + completed)
}

// DomainStatus describes the dream state for a single memory domain.
type DomainStatus struct {
	Domain      string    // "project" or "global"
	Root        string    // git root or ""
	InProgress  bool      // currently being dreamed
	StartedAt   time.Time // when current run started (zero if not in progress)
	ActiveCount int       // number of sessions being processed
	LastState   State     // last completed dream state (from last_dream.json)
}

// inFlightInfo tracks a domain that is currently being dreamed.
type inFlightInfo struct {
	startedAt   time.Time
	domain      string
	root        string
	memoryRoot  string
	activeCount int
}

// SessionGroup groups sessions by their memory domain (project or global).
type SessionGroup struct {
	Domain     string             // "global" or "project"
	Root       string             // git root or ""
	MemoryRoot string             // memory directory path
	Sessions   []*session.Session // all sessions belonging to this domain
}

// Plan holds a validated group ready for dream execution.
type Plan struct {
	Group          SessionGroup       // domain metadata (Root, MemoryRoot, etc.)
	ActiveSessions []*session.Session // sessions with activity since last dream (capped)
	LastState      State
	// Backlog says this plan left active sessions behind because the per-run
	// cap was hit. It is written into the saved State so the NEXT run skips the
	// cooldown and continues draining.
	Backlog bool
}

// RunFunc is the callback invoked for each domain that passes the gates.
// Implementations should run the dream sub-agent pipeline (Orient → Gather →
// Consolidate → Prune) and return the resulting State to persist.
//
// Plan.ActiveSessions contains only sessions with new content since the last
// dream — the sub-agent should focus on these.
//
// The memoryRoot is guaranteed to exist (EnsureMemoryDir is called before).
type RunFunc func(ctx context.Context, plan Plan) (State, error)

// Config holds runtime parameters for dream execution.
type Config struct {
	Logger        *logger.Logger
	MaxConcurrent int // max parallel dream sub-agents (0 → use default)

	// MaxSessionsPerRun caps how many sessions one sub-agent is asked to read
	// (0 → DefaultMaxSessionsPerRun). One prompt has to hold them all: an
	// unbounded run over a backlog produces a request no model accepts, and a
	// failed run advances nothing. What is left over keeps its watermark and
	// goes into the next batch.
	MaxSessionsPerRun int

	// MinInterval is the least time that must have passed since a domain's last
	// successful dream before it is dreamed again (0 → no cooldown).
	//
	// It exists for TIMER-DRIVEN entry points — the desktop checks every half hour
	// while the app happens to be open. Gate 1 alone (new activity since last
	// dream) would let every tick consolidate the handful of messages typed since
	// the previous check, turning a background job into an LLM call per tick. An
	// hourly-or-daily cron entry does not need it: its schedule already IS the
	// interval.
	MinInterval time.Duration
}

// Orchestrator coordinates dream execution across memory domains.
type Orchestrator struct {
	cfg        Config
	logger     *logger.Logger
	mu         sync.RWMutex
	inProgress map[string]*inFlightInfo
}

// NewOrchestrator creates a dream Orchestrator.
func NewOrchestrator(cfg Config) *Orchestrator {
	l := cfg.Logger
	return &Orchestrator{
		cfg:        cfg,
		logger:     l.With("source", "dream"),
		inProgress: make(map[string]*inFlightInfo),
	}
}

// Status returns a snapshot of the orchestrator's current execution state.
// It includes both in-progress domains and their last completed state from disk.
// The returned domains are sorted by (domain, root) for deterministic output.
func (o *Orchestrator) Status() Status {
	o.mu.RLock()
	defer o.mu.RUnlock()

	domains := make([]DomainStatus, 0, len(o.inProgress))
	for _, info := range o.inProgress {
		domains = append(domains, DomainStatus{
			Domain:      info.domain,
			Root:        info.root,
			InProgress:  true,
			StartedAt:   info.startedAt,
			ActiveCount: info.activeCount,
			LastState:   LoadState(info.memoryRoot),
		})
	}

	sort.Slice(domains, func(i, j int) bool {
		return domains[i].Domain+":"+domains[i].Root <
			domains[j].Domain+":"+domains[j].Root
	})

	return Status{
		Running: len(domains),
		Domains: domains,
	}
}

// domainKey returns a unique key for a SessionGroup used for inProgress tracking.
func (o *Orchestrator) domainKey(g SessionGroup) string {
	if g.Domain == "global" {
		return "global"
	}
	return "project:" + g.Root
}

// Run is the main entry point. It lists sessions, groups by domain, checks
// gates, and invokes runFn for each qualifying domain.
//
// It reports how many domains were actually dreamed (0 = every domain was
// skipped by a gate). Callers that only care about the error ignore it; the
// desktop's manual trigger shows it, because "nothing ran" and "ran, but there
// was no new material" are different answers to a person who just asked.
func (o *Orchestrator) Run(ctx context.Context, sessions []*session.Session, runFn RunFunc) (int, error) {
	// Filter sessions marked as skip_dream.
	sessions = FilterSkippedSessions(sessions)

	// Group by memory domain (project vs global).
	groups := GroupSessionsByDomain(sessions)

	// Check gates for each domain and build execution plans.
	var plans []Plan
	for _, g := range groups {
		lastState := LoadState(g.MemoryRoot)

		// Gate 2: minimum interval since this domain's last dream (timer-driven
		// entry points only). Checked before Gate 1's session scan because it is
		// two fields — a tick that lands inside the cooldown must not do work.
		if o.cfg.MinInterval > 0 && !lastState.Backlog && !lastState.LastDreamAt.IsZero() &&
			time.Since(lastState.LastDreamAt) < o.cfg.MinInterval {
			o.logger.Info(ctx, "skipped — dreamed more recently than min interval",
				"domain", g.Domain, "root", g.Root,
				"last_dream", lastState.LastDreamAt, "min_interval", o.cfg.MinInterval)
			continue
		}

		// Gate 3: at least one session with activity since its own watermark.
		active := ActiveSessions(lastState, g.Sessions)
		if len(active) == 0 {
			o.logger.Info(ctx, "skipped — no sessions with activity since last dream",
				"domain", g.Domain, "root", g.Root)
			continue
		}

		// Newest activity first: recent conversations are what recall needs
		// most, and the batch cap must not spend itself on months-old ones.
		sort.SliceStable(active, func(i, j int) bool {
			return active[i].UpdatedAt.After(active[j].UpdatedAt)
		})
		backlog := false
		if maxSessions := o.maxSessionsPerRun(); len(active) > maxSessions {
			active = active[:maxSessions]
			backlog = true
			o.logger.Info(ctx, "session cap reached; remaining sessions wait for the next run",
				"domain", g.Domain, "root", g.Root, "cap", maxSessions)
		}

		// Gate 4: acquire domain lock (no concurrent dream on same domain).
		if !AcquireLock(g.MemoryRoot) {
			o.logger.Info(ctx, "skipped — lock held by another process", "domain", g.Domain, "root", g.Root)
			continue
		}

		plans = append(plans, Plan{Group: g, ActiveSessions: active, LastState: lastState, Backlog: backlog})
	}

	if len(plans) == 0 {
		o.logger.Info(ctx, "no domains passed gates")
		return 0, nil
	}

	return len(plans), o.executePlans(ctx, plans, runFn)
}

// executePlans runs dream sub-agents for each plan, limited by concurrency.
func (o *Orchestrator) executePlans(ctx context.Context, plans []Plan, runFn RunFunc) error {
	maxConcurrent := o.cfg.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = MaxConcurrentDreams
	}
	sem := syncx.NewSemaphore(maxConcurrent)
	var wg sync.WaitGroup

	// Register all domains as in-progress before starting any of them.
	o.mu.Lock()
	for _, p := range plans {
		key := o.domainKey(p.Group)
		o.inProgress[key] = &inFlightInfo{
			startedAt:   time.Now(),
			domain:      p.Group.Domain,
			root:        p.Group.Root,
			memoryRoot:  p.Group.MemoryRoot,
			activeCount: len(p.ActiveSessions),
		}
	}
	o.mu.Unlock()

	for _, plan := range plans {
		if err := sem.Acquire(ctx); err != nil {
			// Context canceled while waiting for a slot — let the running
			// sub-agents finish before returning.
			wg.Wait()
			return err
		}
		wg.Add(1)
		go func(p Plan) {
			defer wg.Done()
			defer sem.Release()
			defer ReleaseLock(p.Group.MemoryRoot)
			defer func() {
				o.mu.Lock()
				delete(o.inProgress, o.domainKey(p.Group))
				o.mu.Unlock()
			}()

			// Ensure memory directory structure exists.
			if err := EnsureMemoryDir(p.Group.MemoryRoot); err != nil {
				o.logger.Error(ctx, "failed to create memory dir", err, "domain", p.Group.Domain, "root", p.Group.Root)
				return
			}

			state, err := runFn(ctx, p)
			if err != nil {
				o.logger.Error(ctx, "failed", err, "domain", p.Group.Domain, "root", p.Group.Root)
				return
			}

			state.Backlog = p.Backlog
			state.SessionWatermarks = pruneWatermarks(state.SessionWatermarks, p.Group.Sessions)
			if err := SaveState(p.Group.MemoryRoot, state); err != nil {
				o.logger.Error(ctx, "failed to save state", err, "domain", p.Group.Domain, "root", p.Group.Root)
			} else {
				o.logger.Info(ctx, "completed", "domain", p.Group.Domain, "root", p.Group.Root)
			}
		}(plan)
	}

	wg.Wait()
	return nil
}

// --- Session filtering ---

// maxSessionsPerRun resolves the batch cap.
func (o *Orchestrator) maxSessionsPerRun() int {
	if o.cfg.MaxSessionsPerRun > 0 {
		return o.cfg.MaxSessionsPerRun
	}
	return DefaultMaxSessionsPerRun
}

// ActiveSessions returns the sessions with activity past their own watermark.
//
// A session without an entry falls back to LastDreamAt — state written before
// per-session watermarks existed behaves exactly as it did. LastDreamAt is
// deliberately NOT used as a floor for a session that HAS a watermark: after a
// run, LastDreamAt is later than an unprocessed session's last update, and
// taking the max would put that session to sleep forever.
func ActiveSessions(lastState State, sessions []*session.Session) []*session.Session {
	var active []*session.Session
	for _, s := range sessions {
		cutoff, ok := lastState.SessionWatermarks[s.ID]
		if !ok {
			cutoff = lastState.LastDreamAt
		}
		if cutoff.IsZero() || s.UpdatedAt.After(cutoff) {
			active = append(active, s)
		}
	}
	return active
}

// pruneWatermarks drops entries for sessions that no longer exist, so the
// state file does not grow with every session ever dreamed.
func pruneWatermarks(marks map[string]time.Time, sessions []*session.Session) map[string]time.Time {
	if len(marks) == 0 {
		return nil
	}
	known := make(container.Set[string])
	for _, s := range sessions {
		known.Add(s.ID)
	}
	out := make(map[string]time.Time, len(marks))
	for id, at := range marks {
		if known.Has(id) {
			out[id] = at
		}
	}
	return out
}

// ActiveSessionsSince returns sessions that have been updated after the given
// time. If since is zero (first dream), all sessions are considered active.
//
// This correctly handles the channel-mode case where a single long-lived
// session accumulates messages over days — as long as UpdatedAt advances,
// it will be picked up by the next dream.
func ActiveSessionsSince(since time.Time, sessions []*session.Session) []*session.Session {
	if since.IsZero() {
		return sessions
	}

	var active []*session.Session
	for _, s := range sessions {
		if s.UpdatedAt.After(since) {
			active = append(active, s)
		}
	}
	return active
}

// FilterSkippedSessions removes sessions with SkipDream=true.
func FilterSkippedSessions(sessions []*session.Session) []*session.Session {
	filtered := make([]*session.Session, 0, len(sessions))
	for _, s := range sessions {
		if !s.SkipDream {
			filtered = append(filtered, s)
		}
	}
	return filtered
}

// --- Session grouping ---

// GroupSessionsByDomain groups sessions by git root (project domain) or global.
func GroupSessionsByDomain(sessions []*session.Session) []SessionGroup {
	groups := make(map[string]*SessionGroup)

	for _, s := range sessions {
		workingDir := s.WorkingDir
		projRoot := FindGitRoot(workingDir)

		var key string
		var group *SessionGroup

		if projRoot != "" {
			key = "project:" + projRoot
			group = &SessionGroup{
				Domain:     "project",
				Root:       projRoot,
				MemoryRoot: filepath.Join(projRoot, ".tachi", "memory"),
			}
		} else {
			key = "global"
			group = &SessionGroup{
				Domain:     "global",
				Root:       "",
				MemoryRoot: filepath.Join(config.BaseDir(), "memory"),
			}
		}

		if _, ok := groups[key]; !ok {
			groups[key] = group
		}
		groups[key].Sessions = append(groups[key].Sessions, s)
	}

	result := make([]SessionGroup, 0, len(groups))
	for _, g := range groups {
		result = append(result, *g)
	}
	return result
}

// --- Lock management ---

// AcquireLock attempts to atomically create dream.lock in the memory dir.
// Returns true if the lock was acquired.
func AcquireLock(memoryDir string) bool {
	// Ensure directory exists before attempting lock.
	_ = os.MkdirAll(memoryDir, 0700)

	lockPath := filepath.Join(memoryDir, "dream.lock")

	// Try atomic create.
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0644)
	if err == nil {
		defer f.Close()
		fmt.Fprintf(f, "%d:%s", os.Getpid(), time.Now().Format(time.RFC3339))
		return true
	}

	if !errors.Is(err, os.ErrExist) {
		return false
	}

	// Lock file exists — check if stale.
	data, readErr := os.ReadFile(lockPath)
	if readErr != nil {
		return false
	}

	parts := strings.SplitN(strings.TrimSpace(string(data)), ":", 2)
	if len(parts) != 2 {
		// Corrupt lock file — take over.
		_ = fileutil.RemoveIgnoreNotExist(lockPath)
		return AcquireLock(memoryDir)
	}

	pid, pidErr := strconv.Atoi(parts[0])
	timestamp, timeErr := time.Parse(time.RFC3339, parts[1])

	// Check PID liveness.
	if pidErr == nil {
		if proc, findErr := os.FindProcess(pid); findErr == nil {
			if proc.Signal(nil) == nil {
				// Process still alive — lock is valid.
				return false
			}
		}
	}

	// Check timestamp freshness (defense in depth).
	if timeErr == nil && time.Since(timestamp) < 5*time.Minute {
		return false
	}

	// Lock is stale — remove and retry.
	_ = fileutil.RemoveIgnoreNotExist(lockPath)
	return AcquireLock(memoryDir)
}

// ReleaseLock removes the dream.lock file for the given memory dir.
func ReleaseLock(memoryDir string) {
	_ = fileutil.RemoveIgnoreNotExist(filepath.Join(memoryDir, "dream.lock"))
}

// --- State persistence ---

// LoadState reads last_dream.json from the memory directory.
func LoadState(memoryRoot string) State {
	data, err := os.ReadFile(filepath.Join(memoryRoot, memory.DreamStateFile))
	if err != nil {
		return State{} // First run or missing.
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}
	}
	return state
}

// SaveState writes last_dream.json to the memory directory.
// Uses a temp-file + rename to avoid corruption on crash (atomic write) —
// a torn file would silently reset LastDreamAt and trigger a full re-dream.
func SaveState(memoryRoot string, state State) error {
	statePath := filepath.Join(memoryRoot, memory.DreamStateFile)
	return fileutil.AtomicWriteJSONShared(statePath, &state)
}

// --- Helpers ---

// FindGitRoot returns the git root for dir, or "" if not in a git repo.
// Pure Go implementation — does not fork a process.
func FindGitRoot(dir string) string {
	if dir == "" {
		return ""
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for {
		if fileutil.Exists(filepath.Join(dir, ".git")) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// EnsureMemoryDir creates the memory directory structure if it doesn't exist.
func EnsureMemoryDir(memoryRoot string) error {
	return os.MkdirAll(filepath.Join(memoryRoot, "topics"), 0700)
}
