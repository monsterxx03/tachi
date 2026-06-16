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
	"github.com/monsterxx03/tachi/pkg/debuglog"
	"github.com/monsterxx03/tachi/session"
)

// MaxConcurrentDreams limits parallel dream sub-agents across all domains.
const MaxConcurrentDreams = 3

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
	ActiveSessions []*session.Session // sessions with activity since last dream
	LastState      State
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
	Logger         *debuglog.Logger
	MaxConcurrent  int // max parallel dream sub-agents (0 → use default)
}

// Orchestrator coordinates dream execution across memory domains.
type Orchestrator struct {
	cfg        Config
	logger     *debuglog.Logger
	mu         sync.RWMutex
	inProgress map[string]*inFlightInfo
}

// NewOrchestrator creates a dream Orchestrator.
func NewOrchestrator(cfg Config) *Orchestrator {
	logger := cfg.Logger
	if logger == nil {
		logger = debuglog.DefaultLogger
	}
	return &Orchestrator{
		cfg:        cfg,
		logger:     logger.WithSource("dream"),
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
func (o *Orchestrator) Run(ctx context.Context, sessions []*session.Session, runFn RunFunc) error {
	// Filter sessions marked as skip_dream.
	sessions = FilterSkippedSessions(sessions)

	// Group by memory domain (project vs global).
	groups := GroupSessionsByDomain(sessions)

	// Check gates for each domain and build execution plans.
	var plans []Plan
	for _, g := range groups {
		lastState := LoadState(g.MemoryRoot)

		// Gate 1: at least one session with activity since last dream.
		active := ActiveSessionsSince(lastState.LastDreamAt, g.Sessions)
		if len(active) == 0 {
			o.logger.Log("[%s:%s] skipped — no sessions with activity since last dream",
				g.Domain, g.Root)
			continue
		}

		// Gate 3: acquire domain lock (no concurrent dream on same domain).
		if !AcquireLock(g.MemoryRoot) {
			o.logger.Log("[%s:%s] skipped — lock held by another process", g.Domain, g.Root)
			continue
		}

		plans = append(plans, Plan{Group: g, ActiveSessions: active, LastState: lastState})
	}

	if len(plans) == 0 {
		o.logger.Log("no domains passed gates")
		return nil
	}

	return o.executePlans(ctx, plans, runFn)
}

// executePlans runs dream sub-agents for each plan, limited by concurrency.
func (o *Orchestrator) executePlans(ctx context.Context, plans []Plan, runFn RunFunc) error {
	maxConcurrent := o.cfg.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = MaxConcurrentDreams
	}
	sem := make(chan struct{}, maxConcurrent)
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
		sem <- struct{}{}
		wg.Add(1)
		go func(p Plan) {
			defer wg.Done()
			defer func() { <-sem }()
			defer ReleaseLock(p.Group.MemoryRoot)
			defer func() {
				o.mu.Lock()
				delete(o.inProgress, o.domainKey(p.Group))
				o.mu.Unlock()
			}()

			// Ensure memory directory structure exists.
			if err := EnsureMemoryDir(p.Group.MemoryRoot); err != nil {
				o.logger.Log("[%s:%s] failed to create memory dir: %v", p.Group.Domain, p.Group.Root, err)
				return
			}

			state, err := runFn(ctx, p)
			if err != nil {
				o.logger.Log("[%s:%s] failed: %v", p.Group.Domain, p.Group.Root, err)
				return
			}

			if err := SaveState(p.Group.MemoryRoot, state); err != nil {
				o.logger.Log("[%s:%s] failed to save state: %v", p.Group.Domain, p.Group.Root, err)
			} else {
				o.logger.Log("[%s:%s] completed", p.Group.Domain, p.Group.Root)
			}
		}(plan)
	}

	wg.Wait()
	return nil
}

// --- Session filtering ---

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
		os.Remove(lockPath)
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
	os.Remove(lockPath)
	return AcquireLock(memoryDir)
}

// ReleaseLock removes the dream.lock file for the given memory dir.
func ReleaseLock(memoryDir string) {
	os.Remove(filepath.Join(memoryDir, "dream.lock"))
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
func SaveState(memoryRoot string, state State) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(memoryRoot, memory.DreamStateFile), data, 0644)
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
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
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
