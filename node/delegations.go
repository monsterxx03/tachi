package node

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

// This machine's own record of the work it delegated.
//
// It exists because the model cannot hold identities (design §8.6): a tool result carries a
// stable local id (`d_7f3a`), and everything the next turn needs — which node, which remote
// conversation, which remote task, what was asked, what the question was — is read back from
// here rather than remembered or re-derived. The remote side's ids are recorded too, but they
// are never what the model sees: a local id is what survives a peer being reconfigured, and it
// is what "continue that earlier work" refers to.
//
// The record is also the ONLY place the message ids this machine generated live. That is what
// makes a retry after a lost response a REPLAY of the request the peer already accepted instead
// of a second submission (design §9.2) — so the request text travels with the record, and a
// retry sends the same id and the same content or it is not a retry.
//
// Layout, inside the delegations dir:
//
//	<delegationsDir>/<id>.json   one delegated task
//
// One file per record, one writer (this process, guarded by the store's mutex), written through
// to disk before it is visible: a record that says a task was submitted describes something
// that really left this machine.
type delegationStore struct {
	dir string

	mu    sync.Mutex
	items map[string]*Delegation
}

const (
	delegationsDirPerm = 0o700
	// delegationIDPrefix makes the ids visibly local: the model copies them around, and
	// `d_` is what tells a reader (and the design's reminders) that this is THIS machine's
	// record rather than anything the peer named.
	delegationIDPrefix = "d_"
	// delegationIDLen is how many hex characters follow the prefix. Short enough to read
	// aloud, long enough that a conversation's records do not collide.
	delegationIDLen = 8
	// promptExcerptLen bounds the stored request text. A prompt is kept so a retry can send
	// the SAME request; it is not an archive, and the tool result never carries it.
	//
	// The budget counts CHARACTERS (strutil.TruncatePlain below applies it): a byte cap would
	// both cost a Chinese prompt two thirds of its budget and be able to cut a character in
	// half — invalid UTF-8 in the record, escaped on the way out as a replacement.
	promptExcerptLen = 4000
)

// Delegation is one remote task this machine asked for.
type Delegation struct {
	ID string `json:"id"`

	// Peer is the configured alias, and PeerNodeID is the identity the peer proved when it
	// answered (its node id). Both are kept because they answer different questions: the alias
	// is what the user and the model use, the node id is what says the record still points at
	// the SAME machine — an alias reconfigured to another host must not silently adopt the
	// tasks of the old one.
	Peer       string `json:"peer"`
	PeerNodeID string `json:"peer_node_id,omitempty"`

	// LogicalID is the local conversation this delegation belongs to. It is the authority for
	// "which of my conversations asked for this", and it is what survives local compaction
	// (the session's own id does not) — see session.Session.LogicalID.
	LogicalID string `json:"logical_id"`

	// RemoteContextID and RemoteTaskID are the receiving side's names for what it accepted.
	// Empty until a submission has been accepted: "we did not get an answer" is a state of this
	// record, not a reason to submit again (§9.2).
	RemoteContextID string `json:"remote_context_id,omitempty"`
	RemoteTaskID    string `json:"remote_task_id,omitempty"`

	// Title is what the model shows for this work ("检查昨天的备份"), and Prompt is the request
	// as it was sent — kept for the retry above, never returned in a tool result.
	Title  string `json:"title"`
	Prompt string `json:"prompt,omitempty"`

	// MessageID is the id this machine generated for the submission. It is the dedupe key the
	// peer replayed against, so it is part of the record rather than regenerated.
	MessageID string `json:"message_id"`
	// QuestionID is the tool call that asked the question the peer is waiting on, when it is
	// waiting on one. It is what an answer must name (§7.4).
	QuestionID string `json:"question_id,omitempty"`

	// LastKnown is the last thing we saw this task be, with when we saw it.
	LastKnown lastKnown `json:"last_known"`

	// DirectAnswer is set when the peer answered the submission with a MESSAGE rather than
	// creating a task (§6.1 — the spec's SendMessageResponse is a oneOf, and "a direct Message
	// response for simple interactions" is a legitimate answer). The answer itself is in
	// LastKnown.Text, and RemoteTaskID stays empty: there is no handle to query, wait on or
	// cancel.
	//
	// It is recorded rather than inferred from "completed with no remote task id" because the
	// two reasons for an empty RemoteTaskID are DIFFERENT FACTS and every refusal that mentions
	// one must not read as the other: a submission whose fate is unknown (nothing came back) is
	// not a peer that answered. Empty for every record written before this field existed, which
	// is correct — none of them were answered that way.
	DirectAnswer bool `json:"direct_answer,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// lastKnown is a snapshot of the remote task as this machine last observed it.
//
// It is deliberately a CACHE with a time on it: every reader (the reminder, a `get`, a tool
// result) has to be able to say "as of then", because the peer may be unreachable now and its
// task may have moved on (§8.2: 区分缓存状态和连接失败).
type lastKnown struct {
	State a2a.TaskState `json:"state"`
	// Text is what travels with that state: the question when the task waits for input, the
	// final answer when it finished. It is a snippet for the model's benefit, not a record.
	Text string    `json:"text,omitempty"`
	At   time.Time `json:"at"`
}

// openDelegationStore loads this machine's delegations, creating the directory on first use.
//
// A record that cannot be read stops the load: these are ids the model has already been given,
// and starting with a hole would make some of them fail for no visible reason.
func openDelegationStore(dir string) (*delegationStore, error) {
	s := &delegationStore{dir: dir, items: make(map[string]*Delegation)}
	if err := os.MkdirAll(dir, delegationsDirPerm); err != nil {
		return nil, fmt.Errorf("node: create delegations dir %s: %w", dir, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("node: read delegations dir %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		var d Delegation
		path := filepath.Join(dir, e.Name())
		if err := fileutil.ReadJSON(path, &d); err != nil {
			return nil, fmt.Errorf("node: read delegation %s: %w", path, err)
		}
		if d.ID == "" {
			return nil, fmt.Errorf("node: delegation record %s has no id", path)
		}
		s.items[d.ID] = &d
	}
	return s, nil
}

// newDelegation builds a record for a submission that is about to happen.
func newDelegation(peer, logicalID, title, prompt, messageID string) *Delegation {
	now := time.Now().UTC()
	prompt = strutil.TruncatePlain(prompt, promptExcerptLen)
	return &Delegation{
		ID:        delegationIDPrefix + strutil.ShortUUID(delegationIDLen),
		Peer:      peer,
		LogicalID: logicalID,
		Title:     title,
		Prompt:    prompt,
		MessageID: messageID,
		// The state is left UNSPECIFIED on purpose: at this moment the request may not have left
		// yet, so the honest answer to "what is this task doing" is "we do not know" — and that is
		// what a view of this record reports until a peer says otherwise (§9.2). Recording
		// SUBMITTED here would claim the peer accepted something it may never have seen.
		LastKnown: lastKnown{At: now},
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// put writes a record through to disk before it becomes visible here.
func (s *delegationStore) put(d *Delegation) error {
	if d == nil || d.ID == "" {
		return errors.New("node: refusing to store a delegation without an id")
	}
	if err := checkRecordID("delegation", d.ID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	d.UpdatedAt = time.Now().UTC()
	path := filepath.Join(s.dir, d.ID+".json")
	if err := fileutil.AtomicWriteJSONPrivate(path, d); err != nil {
		return fmt.Errorf("node: write delegation %s: %w", d.ID, err)
	}
	stored := *d
	s.items[d.ID] = &stored
	return nil
}

// get returns a snapshot: a caller must never hold a pointer into the store's state.
func (s *delegationStore) get(id string) (*Delegation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.items[id]
	if !ok {
		return nil, false
	}
	out := *d
	return &out, true
}

// list returns the records of ONE conversation, newest first.
//
// The filter always applies, and an empty logical id is a scope rather than a wildcard: it is
// what a run WITHOUT a conversation files under (the pipe entry has no session by design), so a
// script's one-off delegation is findable by that script's later runs and invisible to — and
// from — every real conversation. "No filter" would let a sessionless run read a private
// conversation's tasks, which is the leak §8.5's rule is about.
func (s *delegationStore) list(logicalID string) []*Delegation {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Delegation, 0, len(s.items))
	for _, d := range s.items {
		if d.LogicalID != logicalID {
			continue
		}
		snapshot := *d
		out = append(out, &snapshot)
	}
	// A TOTAL order, not merely "newest first". The records come out of a MAP, so a comparator
	// that can report two entries as equal leaves their relative order to map iteration — which
	// differs between calls, and between one load and the next. Two delegations made in the same
	// instant would then trade places in the reminder's rendering, and a reminder whose text
	// changes for no reason is one the collector re-appends as if it were news (the prompt prefix
	// moves with it). The id is the tie-break because it is the record's own identity and never
	// changes once written.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// find locates a record the model referred to. It answers only what was asked: an unknown id is
// an error the caller reports, never a guess at a nearby record (§8.5: 先查询或澄清，不猜).
func (s *delegationStore) find(id string) (*Delegation, error) {
	if id == "" {
		return nil, errors.New("node: a delegation id is required")
	}
	d, ok := s.get(id)
	if !ok {
		return nil, &UnknownDelegationError{ID: id}
	}
	return d, nil
}

// UnknownDelegationError is the refusal an unknown local id gets.
type UnknownDelegationError struct{ ID string }

func (e *UnknownDelegationError) Error() string {
	return fmt.Sprintf("node: no local delegation record %q; use RemoteTask(action=\"list\") to find the right id", e.ID)
}
