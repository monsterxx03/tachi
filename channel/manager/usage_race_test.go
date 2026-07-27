package manager

import (
	"sync"
	"testing"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
)

// TestGetAgentEstimateWithBreakdown_ConcurrentTurn exercises the path that
// motivated AIAgent's turnState: /usage reads token stats off a cached agent
// while a turn is running on that same thread.
//
// The read deliberately does NOT take the cachedAgent lock (a slash command
// must not block an in-flight turn), so before turnState this raced on both
// lastInputTokens and lastTokenBreakdown. Run with -race.
func TestGetAgentEstimateWithBreakdown_ConcurrentTurn(t *testing.T) {
	m := New(Config{Cfg: config.DefaultConfig()})

	const threadID = "thread-usage-race"
	ai := agent.NewAIAgent(nil, 10)
	m.agentCache[threadID] = &cachedAgent{agent: ai}

	msgs := []llm.Message{
		{Role: "system", Content: "system prompt padding"},
		{Role: "user", Content: "a user message long enough to register in the estimate"},
	}

	const iterations = 300
	var wg sync.WaitGroup

	wg.Add(1)
	go func() { // stands in for the turn goroutine
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			ai.EstimateAndUpdateTokens(msgs)
		}
	}()

	wg.Add(1)
	go func() { // stands in for repeated /usage invocations
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			total, bd := m.getAgentEstimateWithBreakdown(threadID)
			if total != 0 && bd.Total != 0 && total != bd.Total {
				t.Errorf("torn read: estimate=%d breakdown.Total=%d", total, bd.Total)
				return
			}
		}
	}()

	wg.Wait()

	total, bd := m.getAgentEstimateWithBreakdown(threadID)
	if total <= 0 {
		t.Errorf("expected a positive estimate, got %d", total)
	}
	if total != bd.Total {
		t.Errorf("final estimate %d disagrees with breakdown total %d", total, bd.Total)
	}
}

func TestGetAgentEstimateWithBreakdown_UnknownThread(t *testing.T) {
	m := New(Config{Cfg: config.DefaultConfig()})

	total, bd := m.getAgentEstimateWithBreakdown("no-such-thread")
	if total != 0 {
		t.Errorf("expected zero estimate for unknown thread, got %d", total)
	}
	if bd.Total != 0 {
		t.Errorf("expected zero breakdown for unknown thread, got %+v", bd)
	}
}
