package tasks

import (
	"testing"
)

func event(name string) Event {
	return Event{ID: name, Name: name, Kind: KindBackground, Status: StatusExited}
}

func names(events []Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Name
	}
	return out
}

// Pending is the queue's whole contract: an event is handed out exactly once,
// because the two consumers (the in-turn reminder and the idle wake) must not
// both tell the model the same thing.
func TestQueue_TakeIsOnce(t *testing.T) {
	q := New()
	q.Post(event("build"))

	first := q.Pending()
	if len(first) != 1 || first[0].Name != "build" {
		t.Fatalf("first Pending = %v, want [build]", names(first))
	}
	if second := q.Pending(); len(second) != 0 {
		t.Fatalf("second Pending = %v, want nothing", names(second))
	}
}

// A consumer that took events but could not deliver them puts them back; they
// must go in FRONT of anything that arrived in the meantime, because they
// finished first.
func TestQueue_RestoreKeepsOrder(t *testing.T) {
	q := New()
	q.Post(event("a"))
	took := q.Pending()

	q.Post(event("b"))
	q.Restore(took)

	got := names(q.Pending())
	want := []string{"a", "b"}
	if len(got) != len(want) {
		t.Fatalf("Pending = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Pending = %v, want %v", got, want)
		}
	}
}

func TestQueue_RestoreEmptyIsANoop(t *testing.T) {
	q := New()
	q.Restore(nil)
	if got := q.Pending(); len(got) != 0 {
		t.Fatalf("Pending = %v, want nothing", names(got))
	}
}

// Posting nudges an idle consumer. The signal is one deep: a burst of posts
// coalesces into "there is something to look at", which is all the watcher
// needs — it drains everything pending.
func TestQueue_PostSignals(t *testing.T) {
	q := New()
	for range 3 {
		q.Post(event("x"))
	}
	select {
	case <-q.Signals():
	default:
		t.Fatal("Post did not signal the consumer")
	}
	if got := len(q.Pending()); got != 3 {
		t.Fatalf("Pending kept %d events, want 3", got)
	}
}

// Restoring also signals: the consumer that put them back did so because it
// could not start a turn, and the next consumer (or the same one, later) must
// still be nudged.
func TestQueue_RestoreSignals(t *testing.T) {
	q := New()
	q.Post(event("x"))
	took := q.Pending()
	<-q.Signals() // consume the post's nudge

	q.Restore(took)
	select {
	case <-q.Signals():
	default:
		t.Fatal("Restore did not signal")
	}
}

func TestQueue_NilIsSafe(t *testing.T) {
	var q *Queue
	q.Post(event("x"))
	if got := q.Pending(); got != nil {
		t.Fatalf("nil queue Pending = %v, want nil", got)
	}
	q.Restore([]Event{event("x")})
	if got := q.Len(); got != 0 {
		t.Fatalf("nil queue Len = %d, want 0", got)
	}
}

// Pause holds the IDLE consumer only: the in-turn reminder keeps taking events,
// which is how a paused conversation drains what it has. Resume pokes the
// watcher so events that piled up while paused are noticed without a new post.
func TestQueue_PauseHoldsTheWakeButNotTheReminder(t *testing.T) {
	q := New()
	q.Pause()
	if !q.Paused() {
		t.Fatal("Pause did not hold the queue")
	}

	q.Post(event("build"))
	if got := q.Pending(); len(got) != 1 {
		t.Fatalf("the in-turn reminder must still read while paused, got %v", names(got))
	}

	q.Resume()
	if q.Paused() {
		t.Fatal("Resume did not release the queue")
	}
	select {
	case <-q.Signals():
	default:
		t.Fatal("Resume must nudge the watcher")
	}
}
