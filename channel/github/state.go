package github

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// StateManager manages the persistent state of issue processing.
// Thread-safe via mutex. Uses atomic writes (tmp + rename).
type StateManager struct {
	mu        sync.RWMutex
	state     *GlobalState
	stateDir  string
	statePath string
	logger    *logger.Logger
}

// NewStateManager creates or loads the state from disk.
func NewStateManager(log *logger.Logger) (*StateManager, error) {
	stateDir := filepath.Join(config.BaseDir(), "github")
	statePath := filepath.Join(stateDir, "state.json")

	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return nil, fmt.Errorf("github: create state dir: %w", err)
	}

	sm := &StateManager{
		state:     &GlobalState{Repos: make(map[string]*RepoState)},
		stateDir:  stateDir,
		statePath: statePath,
		logger:    log,
	}

	// Try to load existing state
	if err := sm.load(); err != nil {
		log.Warn(context.Background(), "github: no existing state, starting fresh", "error", err)
	}

	return sm, nil
}

// load reads state from disk. Returns nil if file doesn't exist.
func (sm *StateManager) load() error {
	var state GlobalState
	if err := fileutil.ReadJSON(sm.statePath, &state); err != nil {
		return fmt.Errorf("github: load state: %w", err)
	}
	if state.Repos == nil {
		state.Repos = make(map[string]*RepoState)
	}
	sm.state = &state
	return nil
}

// SaveAtomic atomically writes the state to disk (tmp + rename).
func (sm *StateManager) SaveAtomic() error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if err := fileutil.AtomicWriteJSONShared(sm.statePath, sm.state); err != nil {
		return fmt.Errorf("github: save state: %w", err)
	}
	return nil
}

// Repo returns a copy of the RepoState for the given repo.
// Returns a newly allocated copy so callers cannot mutate internal state.
func (sm *StateManager) Repo(name string) RepoState {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	rs := sm.state.Repo(name)
	return *rs
}

// EnsureRepoSeeded marks a repo as seeded (first run handled).
func (sm *StateManager) EnsureRepoSeeded(name string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	rs := sm.state.Repo(name)
	rs.Seeded = true
}

// UpdateIssueState transitions an issue to a new state, with runtime
// validation against the state machine defined in IssueState.Transition.
// Invalid transitions are logged as warnings but still applied — the
// validation is a safety net, not a hard block, to avoid crashes from
// unexpected state paths during development.
func (sm *StateManager) UpdateIssueState(repo, issueNum string, newState IssueState) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	rs := sm.state.Repo(repo)
	ir := rs.Issue(issueNum)

	// Validate transition against the state machine.
	if !isValidTransition(ir.State, newState) {
		sm.logger.Warn(context.Background(), "github: invalid state transition",
			"repo", repo, "issue", issueNum,
			"from", ir.State, "to", newState)
	}

	ir.State = newState
}

// isValidTransition checks if a transition from old to new state is valid
// according to the state machine. Returns true if any event can produce the
// transition, or if old is empty (initial state).
func isValidTransition(old, new IssueState) bool {
	if old == "" {
		return true // initial state, any transition is valid
	}
	// Check all possible events from old state.
	for _, event := range []string{"discuss", "skip", "ready_for_pr", "wait_author", "implement", "retry", "pr_created"} {
		if next, ok := old.Transition(event); ok && next == new {
			return true
		}
	}
	return false
}

// UpdateIssueAfterPoll updates issue state after polling.
func (sm *StateManager) UpdateIssueAfterPoll(repo, issueNum string, lastCommentID int64, lastCommentAt time.Time) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	rs := sm.state.Repo(repo)
	ir := rs.Issue(issueNum)
	if ir.State == "" {
		ir.State = IssueStateNew
		ir.FirstSeenAt = time.Now()
	}
	ir.LastProcessedCommentID = lastCommentID
	ir.LastCommentAt = lastCommentAt
}

// GetIssueRecord returns a copy of the issue record.
func (sm *StateManager) GetIssueRecord(repo, issueNum string) *IssueRecord {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	rs := sm.state.Repo(repo)
	ir := rs.Issue(issueNum)
	cp := *ir
	return &cp
}

// SetLastPolledAt updates the last poll timestamp for a repo.
func (sm *StateManager) SetLastPolledAt(repo string, t time.Time) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	rs := sm.state.Repo(repo)
	rs.LastPolledAt = t
}

// IncrementRetryCount increments the retry count for an issue.
func (sm *StateManager) IncrementRetryCount(repo, issueNum string) int {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	rs := sm.state.Repo(repo)
	ir := rs.Issue(issueNum)
	ir.RetryCount++
	return ir.RetryCount
}

// MarkIssueProcessed advances the processed watermark after successful processing.
// Unlike UpdateIssueAfterPoll, this is called only after the worker successfully
// completes the turn — if the worker fails, the watermark stays, so the issue
// will be re-enqueued on the next poll.
func (sm *StateManager) MarkIssueProcessed(repo, issueNum string, commentID int64, commentAt time.Time) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	rs := sm.state.Repo(repo)
	ir := rs.Issue(issueNum)
	if ir.State == "" {
		ir.State = IssueStateNew
		ir.FirstSeenAt = time.Now()
	}
	ir.LastProcessedCommentID = commentID
	ir.LastCommentAt = commentAt
}

// ForEachPRCreatedIssue iterates over issues in PRCreated state under lock.
// Provides a safe way to iterate without exposing the internal map.
func (sm *StateManager) ForEachPRCreatedIssue(repo string, fn func(issueNum string, ir IssueRecord)) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	rs := sm.state.Repo(repo)
	for num, ir := range rs.Issues {
		if ir.State == IssueStatePRCreated && ir.PRNumber > 0 {
			fn(num, *ir)
		}
	}
}

// UpdateReviewCommentID updates the last processed PR review comment ID for an issue.
func (sm *StateManager) UpdateReviewCommentID(repo, issueNum string, commentID int64) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	rs := sm.state.Repo(repo)
	ir := rs.Issue(issueNum)
	ir.LastProcessedReviewCommentID = commentID
}

// SetPRNumber records the PR number for an issue.
func (sm *StateManager) SetPRNumber(repo, issueNum string, prNumber int) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	rs := sm.state.Repo(repo)
	ir := rs.Issue(issueNum)
	ir.PRNumber = prNumber
}

// SeedExistingIssues marks all current open issues as skipped on first run.
// This prevents the bot from replying to pre-existing issues.
//
// Only issues with no prior state (ir.State == "") are marked as skipped.
// Issues that already have a state (e.g., from a previous state file after
// restart) are preserved — this allows crash recovery without re-processing
// already-handled issues.
func (sm *StateManager) SeedExistingIssues(repo string, issueNumbers []string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	rs := sm.state.Repo(repo)
	for _, num := range issueNumbers {
		ir := rs.Issue(num)
		if ir.State == "" {
			ir.State = IssueStateSkipped
			ir.FirstSeenAt = time.Now()
		}
	}
	rs.Seeded = true
}

// AllSeeded returns true if all repos have been seeded.
func (sm *StateManager) AllSeeded() bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	for _, rs := range sm.state.Repos {
		if !rs.Seeded {
			return false
		}
	}
	return true
}

// StatePath returns the path to the state file.
func (sm *StateManager) StatePath() string {
	return sm.statePath
}
