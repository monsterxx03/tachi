package tui

import (
	"context"
	"errors"
	"testing"

	"github.com/monsterxx03/tachi/agent"
)

// startTurn is the single place where a new agent stream is prepared. These
// tests pin the two invariants it exists to guarantee — every path that assigns
// m.eventCh goes through it, so a regression here reintroduces either the
// steer-point deadlock or stale-event processing.

// TestStartTurn_CancelsPreviousTurn is the regression test for the deadlock
// where starting a new turn while one was in flight left the old agent
// goroutine parked at a steer point, waiting on a steer channel the TUI no
// longer read, while the TUI waited on the replaced eventCh. Neither side ever
// woke up and the UI hung with no error.
func TestStartTurn_CancelsPreviousTurn(t *testing.T) {
	m := testModel()

	first := m.startTurn()
	if first.Err() != nil {
		t.Fatalf("first turn context should be live, got %v", first.Err())
	}

	second := m.startTurn()

	select {
	case <-first.Done():
		if !errors.Is(first.Err(), context.Canceled) {
			t.Errorf("first ctx err = %v, want context.Canceled", first.Err())
		}
	default:
		t.Fatal("startTurn did not cancel the in-flight turn — the old agent goroutine can deadlock at a steer point")
	}

	if second.Err() != nil {
		t.Errorf("second turn context should be live, got %v", second.Err())
	}
}

// TestStartTurn_BumpsStreamGen covers the other half of the invariant: events
// still in flight from the cancelled stream must be recognised as stale.
func TestStartTurn_BumpsStreamGen(t *testing.T) {
	m := testModel()

	before := m.streamGen
	m.startTurn()
	if m.streamGen != before+1 {
		t.Errorf("streamGen = %d, want %d", m.streamGen, before+1)
	}

	m.startTurn()
	if m.streamGen != before+2 {
		t.Errorf("streamGen after two turns = %d, want %d", m.streamGen, before+2)
	}
}

// TestStartTurn_StaleEventsIgnored ties the generation counter to the Update
// path: an event tagged with the previous generation must not be handled.
func TestStartTurn_StaleEventsIgnored(t *testing.T) {
	m := testModel()
	m.startTurn()
	staleGen := m.streamGen

	// A second turn supersedes the first.
	m.startTurn()

	m.chatview.ResetStreaming()
	_, cmd := m.Update(agentEventMsg{
		gen:   staleGen,
		event: agent.AgentEvent{Type: agent.AgentEventTextDelta, TextDelta: "from the cancelled stream"},
	})
	if cmd != nil {
		t.Error("stale event produced a follow-up command; it should be dropped outright")
	}

	// The current generation is still processed.
	_, cmd = m.Update(agentEventMsg{
		gen:   m.streamGen,
		event: agent.AgentEvent{Type: agent.AgentEventTextDelta, TextDelta: "live"},
	})
	if cmd == nil {
		t.Error("event for the current generation should be handled")
	}
}

// TestStartTurn_FirstCallWithNoPriorTurn guards the nil-cancelFunc path, which
// is what every fresh Model hits on its first message.
func TestStartTurn_FirstCallWithNoPriorTurn(t *testing.T) {
	m := testModel()
	if m.cancelFunc != nil {
		t.Fatal("fresh model should have no cancelFunc")
	}

	ctx := m.startTurn()
	if ctx.Err() != nil {
		t.Errorf("ctx should be live, got %v", ctx.Err())
	}
	if m.cancelFunc == nil {
		t.Error("startTurn must install a cancelFunc")
	}
}
