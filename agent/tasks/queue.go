// Package tasks carries the one kind of news a turn cannot wait for: a job the
// agent started has finished while it was busy doing something else (or while
// nobody was asking anything at all).
//
// A Queue is the waiting room between a task's exit and the model hearing about
// it. It exists because there are two ways to hear about a finished task and
// only one copy of the news:
//
//   - a turn is in flight: the reminder collector takes everything pending just
//     before the next request, so the model sees it in the same turn;
//   - the conversation is idle: the entry point takes everything pending and
//     starts a turn carrying it.
//
// Taking is destructive (Pending removes), so exactly one of the two reports an
// event. Both paths render it through systemreminder.TaskLines, so the wording is
// the same wherever it arrives.
package tasks

import (
	"sync"
	"time"
)

// Kind says what produced a task. It only decides wording: the queue treats
// every event the same.
type Kind string

const (
	KindBackground Kind = "background"
	KindSubagent   Kind = "subagent"
	KindRemote     Kind = "remote"
)

// Status is how a task ended. A task that was stopped on purpose is not posted
// at all — the caller asked for it, so there is nothing to report.
type Status string

const (
	StatusExited Status = "exited"
	StatusError  Status = "error"
	// StatusWaiting is not an ending: a remote task is parked on input the
	// conversation must supply. It is still news worth waking for — an idle
	// conversation cannot answer what it never heard.
	StatusWaiting Status = "waiting"
)

// Event is one task that has reached a terminal state. It is a snapshot: the
// producer copies everything the reader needs, because the process it describes
// is gone by the time anyone reads it.
type Event struct {
	// ID is unique within the conversation. Today background processes use their
	// managed name; the field is here so sub-agents and remote tasks can share the
	// queue later without one kind's naming leaking into another's.
	ID string

	// Conversation is the logical conversation the task belongs to. An entry point
	// that hosts ONE conversation per agent (desktop, channel) can ignore it; the
	// TUI keeps a single agent across sessions, and without this tag a background
	// process from one conversation would be reported in another.
	Conversation string

	Kind    Kind
	Name    string
	Command string

	Status   Status
	ExitCode int
	Error    string

	// Stdout/Stderr are the tails retained by the producer, already bounded. The
	// reader may trim further; it must not assume they are the whole output.
	Stdout string
	Stderr string

	// Node is the machine a remote task lives on, as a name a reader recognizes.
	// Empty for local tasks.
	Node string

	// QuestionID names the question a waiting remote task is parked on: an answer
	// must carry it (RemoteTask(action="reply")).
	QuestionID string

	At time.Time
}

// Queue is the pending set of one conversation. The zero value is not usable;
// call New.
//
// It is safe for concurrent use: producers post from process goroutines, while
// the reminder collector (turn goroutine) and the wake watcher (entry goroutine)
// both take from it.
type Queue struct {
	mu      sync.Mutex
	pending []Event

	// paused stops the IDLE consumer only (the entry point's watcher). The in-turn
	// reminder keeps taking events: a turn that is already running should still be
	// told, and it is how a paused conversation drains what it has. The user's Stop
	// sets this, and the next user message clears it (see desktop/taskwake.go).
	paused bool

	// signal is a one-deep nudge: posting never blocks, and a consumer that was
	// already awake finds the extra poke as an empty Pending.
	signal chan struct{}
}

// New returns an empty queue.
func New() *Queue {
	return &Queue{signal: make(chan struct{}, 1)}
}

// Post appends one finished task and nudges the consumer.
func (q *Queue) Post(e Event) {
	if q == nil {
		return
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	q.mu.Lock()
	q.pending = append(q.pending, e)
	q.mu.Unlock()
	q.poke()
}

// Pending removes and returns everything posted since the last take. It reports
// each event exactly once: the caller that takes it owns the telling.
func (q *Queue) Pending() []Event {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) == 0 {
		return nil
	}
	out := q.pending
	q.pending = nil
	return out
}

// PendingFor removes and returns the events that belong to one conversation,
// leaving every other conversation's events in place. It is how a reader that
// shares its queue with other conversations (the TUI's single agent) avoids
// reporting someone else's task.
func (q *Queue) PendingFor(conversation string) []Event {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []Event
	kept := q.pending[:0]
	for _, e := range q.pending {
		if e.Conversation == conversation {
			out = append(out, e)
			continue
		}
		kept = append(kept, e)
	}
	q.pending = kept
	return out
}

// Restore puts events back after a consumer took them but could not deliver
// them (the entry point found the session busy, or could not start a turn).
// Restored events go in front, because they finished first.
func (q *Queue) Restore(events []Event) {
	if q == nil || len(events) == 0 {
		return
	}
	q.mu.Lock()
	q.pending = append(append([]Event(nil), events...), q.pending...)
	q.mu.Unlock()
	q.poke()
}

// Signals reports one nudge per non-empty transition, so an idle consumer can
// sleep instead of polling. It never closes: the queue lives as long as the
// conversation, and its watcher is stopped by its own context.
func (q *Queue) Signals() <-chan struct{} {
	return q.signal
}

// Len reports how many events are pending.
func (q *Queue) Len() int {
	if q == nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending)
}

// Pause stops the idle consumer from starting a turn. Events keep arriving and
// keep being readable by the in-turn reminder; only the wake is held.
func (q *Queue) Pause() {
	if q == nil {
		return
	}
	q.mu.Lock()
	q.paused = true
	q.mu.Unlock()
}

// Resume lets the idle consumer wake again, and nudges it: events that piled up
// while paused must not wait for the next Post to be noticed.
func (q *Queue) Resume() {
	if q == nil {
		return
	}
	q.mu.Lock()
	q.paused = false
	q.mu.Unlock()
	q.poke()
}

// Paused reports whether the idle consumer is held.
func (q *Queue) Paused() bool {
	if q == nil {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.paused
}

func (q *Queue) poke() {
	select {
	case q.signal <- struct{}{}:
	default: // a nudge is already waiting; one is as good as two
	}
}
