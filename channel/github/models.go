package github

import (
	"context"
	"time"

	"github.com/google/go-github/v69/github"
)

// IssueState represents the current state of an issue being processed.
type IssueState string

const (
	IssueStateNew           IssueState = "new"
	IssueStateDiscussing    IssueState = "discussing"
	IssueStateReadyForPR    IssueState = "ready_for_pr"
	IssueStateImplementing  IssueState = "implementing"
	IssueStatePRCreated     IssueState = "pr_created"
	IssueStateWaitingAuthor IssueState = "waiting_author"
	IssueStateSkipped       IssueState = "skipped"
)

// IssueRecord tracks the processing state of a single GitHub issue.
type IssueRecord struct {
	State                        IssueState `json:"state"`
	FirstSeenAt                  time.Time  `json:"first_seen_at"`
	LastCommentAt                time.Time  `json:"last_comment_at,omitempty"`
	LastProcessedCommentID       int64      `json:"last_processed_comment_id,omitempty"`
	LastProcessedReviewCommentID int64      `json:"last_processed_review_comment_id,omitempty"`
	PRNumber                     int        `json:"pr_number,omitempty"`
	RetryCount                   int        `json:"retry_count,omitempty"`
}

// RepoState holds the state for a single repository.
type RepoState struct {
	Seeded       bool                    `json:"seeded"`
	LastPolledAt time.Time               `json:"last_polled_at"`
	Issues       map[string]*IssueRecord `json:"issues"` // keyed by issue number
}

// GlobalState is the top-level state persisted to disk.
type GlobalState struct {
	Repos map[string]*RepoState `json:"repos"` // keyed by "owner/repo"
}

// Repo returns the RepoState for the given repo, creating it if needed.
func (s *GlobalState) Repo(name string) *RepoState {
	if s.Repos == nil {
		s.Repos = make(map[string]*RepoState)
	}
	rs, ok := s.Repos[name]
	if !ok {
		rs = &RepoState{
			Issues: make(map[string]*IssueRecord),
		}
		s.Repos[name] = rs
	}
	return rs
}

// Issue returns the IssueRecord for the given issue number, creating it if needed.
func (rs *RepoState) Issue(number string) *IssueRecord {
	if rs.Issues == nil {
		rs.Issues = make(map[string]*IssueRecord)
	}
	ir, ok := rs.Issues[number]
	if !ok {
		ir = &IssueRecord{State: IssueStateNew}
		rs.Issues[number] = ir
	}
	return ir
}

// WorkItem represents a unit of work to be processed by the worker.
type WorkItem struct {
	RepoName      string
	RepoPath      string // local clone path
	Issue         *github.Issue
	IssueNum      string
	IssueRec      *IssueRecord
	Comments      []*github.IssueComment
	BotLogin      string
	LastCommentID int64     // latest human comment ID (for advancing watermark on success)
	LastCommentAt time.Time // latest human comment timestamp
	DefaultBranch string    // base branch for PR creation
	Ctx           context.Context
}

// Transition maps old state to new state based on transition rules.
// Used by UpdateIssueState for runtime validation of state transitions.
func (s IssueState) Transition(event string) (IssueState, bool) {
	transitions := map[IssueState]map[string]IssueState{
		IssueStateNew: {
			"discuss": IssueStateDiscussing,
			"skip":    IssueStateSkipped,
		},
		IssueStateDiscussing: {
			"ready_for_pr": IssueStateReadyForPR,
			"wait_author":  IssueStateWaitingAuthor,
			"skip":         IssueStateSkipped,
		},
		IssueStateReadyForPR: {
			"implement": IssueStateImplementing,
			"discuss":   IssueStateDiscussing, // gate failed, back to discuss
			"skip":      IssueStateSkipped,
		},
		IssueStateImplementing: {
			"pr_created": IssueStatePRCreated,
			"retry":      IssueStateReadyForPR, // crash recovery
			"skip":       IssueStateSkipped,
		},
		IssueStateWaitingAuthor: {
			"discuss": IssueStateDiscussing, // author replied
			"skip":    IssueStateSkipped,    // timeout
		},
		IssueStatePRCreated: {
			// terminal state, no transitions
		},
		IssueStateSkipped: {
			// terminal state, no transitions
		},
	}

	if m, ok := transitions[s]; ok {
		if next, ok := m[event]; ok {
			return next, true
		}
	}
	return s, false
}
