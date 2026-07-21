package dream

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// mockProvider implements llm.Provider with a controllable stream function,
// letting tests simulate arbitrary behavior "during" the dream agent run.
type mockProvider struct {
	streamFn func(ctx context.Context) (<-chan llm.StreamEvent, error)
}

func (p *mockProvider) Name() string  { return "mock" }
func (p *mockProvider) Model() string { return "mock-model" }

func (p *mockProvider) CreateChat(ctx context.Context, messages []llm.Message, tools []llm.Tool, opts llm.ChatOptions) (*llm.Response, error) {
	return nil, fmt.Errorf("not implemented")
}

func (p *mockProvider) CreateChatStream(ctx context.Context, messages []llm.Message, tools []llm.Tool, opts llm.ChatOptions) (<-chan llm.StreamEvent, error) {
	return p.streamFn(ctx)
}

// stopStream returns a stream that emits a short text then finishes with stop.
func stopStream() <-chan llm.StreamEvent {
	ch := make(chan llm.StreamEvent, 2)
	ch <- llm.StreamEvent{Type: llm.StreamEventTextDelta, TextDelta: "done"}
	ch <- llm.StreamEvent{Type: llm.StreamEventDone, FinishReason: "stop", Usage: &llm.Usage{InputTokens: 10, OutputTokens: 5}}
	close(ch)
	return ch
}

func testPlan(memRoot string) Plan {
	return Plan{
		Group:          SessionGroup{Domain: "global", MemoryRoot: memRoot},
		ActiveSessions: []*session.Session{{ID: "s1", Title: "test session"}},
	}
}

func testLoadMessages(id string) ([]session.Message, error) {
	return []session.Message{
		{Type: session.MessageTypeUser, Content: "hello", Timestamp: time.Now()},
		{Type: session.MessageTypeAssistant, Content: "hi there", Timestamp: time.Now()},
	}, nil
}

// TestRunDream_WatermarkIsSnapshotTime verifies that State.LastDreamAt is the
// pre-run snapshot time, not the completion time. Messages arriving while the
// dream is running must remain eligible for the next dream — a completion-time
// watermark would silently skip them.
func TestRunDream_WatermarkIsSnapshotTime(t *testing.T) {
	memRoot := t.TempDir()

	provider := &mockProvider{
		streamFn: func(ctx context.Context) (<-chan llm.StreamEvent, error) {
			time.Sleep(300 * time.Millisecond) // simulate a slow dream run
			return stopStream(), nil
		},
	}

	state, err := RunDream(context.Background(), testPlan(memRoot), RunConfig{
		FallbackProvider: provider,
		MaxIter:          3,
	}, testLoadMessages)
	if err != nil {
		t.Fatalf("RunDream: %v", err)
	}

	// With a 300ms dream run, a completion-time watermark would be ~0ms old;
	// the snapshot watermark must be at least ~300ms old.
	if age := time.Since(state.LastDreamAt); age < 250*time.Millisecond {
		t.Errorf("LastDreamAt should be the pre-run snapshot (age ≥ ~300ms), got age %v", age)
	}
}

// TestRunDream_PreservesConcurrentReinforcements verifies that reinforcements
// recorded by TopicBackend while the dream is running are not clobbered by
// the post-dream state save. RunDream must reload the on-disk state before
// scanning topic facts instead of merging from the stale plan snapshot.
func TestRunDream_PreservesConcurrentReinforcements(t *testing.T) {
	memRoot := t.TempDir()
	topicsDir := filepath.Join(memRoot, "topics")
	if err := os.MkdirAll(topicsDir, 0755); err != nil {
		t.Fatal(err)
	}

	topicContent := `## Some Fact

状态: active
关键词: test

A fact worth remembering.

---
`
	if err := os.WriteFile(filepath.Join(topicsDir, "test.md"), []byte(topicContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Initial state as of plan-build time: 1 reinforcement.
	initial := ScanTopicFacts(memRoot, nil, nil)
	if len(initial) != 1 {
		t.Fatalf("expected 1 fact, got %d", len(initial))
	}
	var factID string
	for id, fs := range initial {
		factID = id
		fs.Reinforcements = 1
		fs.LastReinforced = time.Now().Add(-time.Hour)
	}
	if err := SaveState(memRoot, State{FactStates: initial}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	// During the dream, a concurrent recall reinforces the fact — mimics
	// TopicBackend.ReinforceFact writing last_dream.json mid-run.
	provider := &mockProvider{
		streamFn: func(ctx context.Context) (<-chan llm.StreamEvent, error) {
			latest := LoadState(memRoot)
			fs := latest.FactStates[factID]
			if fs == nil {
				t.Errorf("fact %s missing from reloaded state", factID)
				return stopStream(), nil
			}
			fs.Reinforcements = 5
			fs.LastReinforced = time.Now()
			if err := SaveState(memRoot, latest); err != nil {
				t.Errorf("concurrent SaveState: %v", err)
			}
			return stopStream(), nil
		},
	}

	plan := testPlan(memRoot)
	plan.LastState = LoadState(memRoot) // stale snapshot from plan-build time

	state, err := RunDream(context.Background(), plan, RunConfig{
		FallbackProvider: provider,
		MaxIter:          3,
	}, testLoadMessages)
	if err != nil {
		t.Fatalf("RunDream: %v", err)
	}

	fs := state.FactStates[factID]
	if fs == nil {
		t.Fatalf("fact %s missing from final state", factID)
	}
	if fs.Reinforcements != 5 {
		t.Errorf("reinforcements during dream were lost: expected 5, got %d", fs.Reinforcements)
	}
}
