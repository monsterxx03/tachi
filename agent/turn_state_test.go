package agent

import (
	"sync"
	"testing"

	"github.com/monsterxx03/tachi/llm"
)

// TestTurnState_ConcurrentEstimateAndRead guards the data race that motivated
// turnState. In channel mode one cached AIAgent is shared between the turn
// goroutine (writing the estimate via EstimateAndUpdateTokens) and slash-command
// handlers reading it: handleUsageCommand reaches the agent through
// getAgentEstimate / getAgentBreakdown, which release agentCacheMu after the
// map lookup and then read agent fields without holding the cachedAgent lock.
//
// Before turnState, both lastInputTokens and lastTokenBreakdown raced here.
// Run with -race for this test to be meaningful.
func TestTurnState_ConcurrentEstimateAndRead(t *testing.T) {
	a := NewAIAgent(&mockStreamProvider{name: "anthropic"}, 10)

	msgs := []llm.Message{
		{Role: "system", Content: "you are a helpful agent"},
		{Role: "user", Content: "hello world, padding to make the estimate nonzero"},
	}

	const iterations = 500
	var wg sync.WaitGroup

	wg.Add(1)
	go func() { // turn goroutine
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			a.EstimateAndUpdateTokens(msgs)
		}
	}()

	wg.Add(1)
	go func() { // slash-command reader
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_ = a.LastInputEstimate()
			_ = a.LastTokenBreakdown()
		}
	}()

	wg.Wait()

	if got := a.LastInputEstimate(); got <= 0 {
		t.Errorf("expected a positive estimate after concurrent updates, got %d", got)
	}
}

// TestTurnState_EstimateAndBreakdownStayConsistent verifies the invariant that
// motivated pairing the two fields under one lock: a reader must never observe
// a token total that disagrees with the breakdown it was computed from.
func TestTurnState_EstimateAndBreakdownStayConsistent(t *testing.T) {
	a := NewAIAgent(&mockStreamProvider{name: "anthropic"}, 10)

	short := []llm.Message{{Role: "user", Content: "hi"}}
	long := []llm.Message{{Role: "user", Content: string(make([]byte, 40_000))}}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			if i%2 == 0 {
				a.EstimateAndUpdateTokens(short)
			} else {
				a.EstimateAndUpdateTokens(long)
			}
		}
		close(stop)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			total, tb := a.LastInputEstimateWithBreakdown()
			// Only meaningful once the first estimate has landed.
			if total != 0 && tb.Total != 0 && total != tb.Total {
				t.Errorf("torn read: estimate=%d but breakdown.Total=%d", total, tb.Total)
				return
			}
		}
	}()

	wg.Wait()
}

// TestTurnState_ConcurrentMessagesSnapshot covers GetLastMessages, which channel
// mode calls to refresh its in-memory history cache while a turn may still be
// running. The returned slice is a copy, so appends by the writer must not be
// visible through it.
func TestTurnState_ConcurrentMessagesSnapshot(t *testing.T) {
	ts := newTurnState()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		msgs := make([]llm.Message, 0, 100)
		for i := 0; i < 100; i++ {
			msgs = append(msgs, llm.Message{Role: "user", Content: "m"})
			ts.setMessages(msgs)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			snap := ts.snapshotMessages()
			// Mutating the snapshot must not affect the stored slice.
			for j := range snap {
				snap[j].Content = "mutated"
			}
		}
	}()

	wg.Wait()

	for i, m := range ts.snapshotMessages() {
		if m.Content != "m" {
			t.Fatalf("snapshot mutation leaked into turn state at %d: %q", i, m.Content)
		}
	}
}

func TestTurnState_TakePendingImagesClears(t *testing.T) {
	ts := newTurnState()
	ts.setPendingImages([]llm.ContentPart{{Type: llm.ContentPartText, Text: "img"}})

	if got := ts.takePendingImages(); len(got) != 1 {
		t.Fatalf("expected 1 pending image, got %d", len(got))
	}
	if got := ts.takePendingImages(); got != nil {
		t.Errorf("expected pending images to be cleared, got %+v", got)
	}
}

func TestTurnState_CompactCooldown(t *testing.T) {
	ts := newTurnState()

	if ts.compactCooldown() {
		t.Error("no compaction recorded yet — cooldown must be false")
	}

	ts.setEstimate(1000, ts.snapshotBreakdown())
	ts.setCompactEstimate(1000)

	if !ts.compactCooldown() {
		t.Error("estimate unchanged since compaction — expected cooldown")
	}

	ts.setTokens(1150) // +15%
	if !ts.compactCooldown() {
		t.Error("growth below 20% — expected cooldown to still hold")
	}

	ts.setTokens(1300) // +30%
	if ts.compactCooldown() {
		t.Error("growth above 20% — expected cooldown to lift")
	}
}
