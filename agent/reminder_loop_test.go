package agent

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/agent/systemreminder"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// fixedReminderCollector reports the same piece on every collection: what a plan reminder
// looks like while an agent works through a turn.
type fixedReminderCollector struct {
	collects int
	lines    []string
}

func (c *fixedReminderCollector) Collect(ctx context.Context, rctx systemreminder.Context) string {
	return systemreminder.RenderPieces(c.CollectPieces(ctx, rctx))
}

func (c *fixedReminderCollector) CollectPieces(context.Context, systemreminder.Context) []systemreminder.Piece {
	c.collects++
	return []systemreminder.Piece{{Name: "fixed", Lines: c.lines}}
}

func (c *fixedReminderCollector) AddReminder(systemreminder.Reminder) {}

// changingReminderCollector reports a different piece every time — "there is something
// new to say", which the loop must keep delivering.
type changingReminderCollector struct{ collects int }

func (c *changingReminderCollector) Collect(ctx context.Context, rctx systemreminder.Context) string {
	return systemreminder.RenderPieces(c.CollectPieces(ctx, rctx))
}

func (c *changingReminderCollector) CollectPieces(context.Context, systemreminder.Context) []systemreminder.Piece {
	c.collects++
	return []systemreminder.Piece{{Name: "changing", Lines: []string{fmt.Sprintf("round %d", c.collects)}}}
}

func (c *changingReminderCollector) AddReminder(systemreminder.Reminder) {}

// recordedReminders returns the reminder messages the turn wrote to the session, in order.
func recordedReminders(t *testing.T, sm *fakeSessionManager) []string {
	t.Helper()
	msgs, err := sm.LoadMessages()
	require.NoError(t, err)
	var out []string
	for _, m := range msgs {
		if m.Type == session.MessageTypeReminder {
			out = append(out, m.Content)
		}
	}
	return out
}

// TestAgentLoop_UnchangedReminderNotRepeated: the loop collects reminders after EVERY
// tool round, and the same text must not be appended again — the model still has it in
// context, and a repeated block is paid for at the tail of a growing prompt, the one
// place a prompt cache does not help. (Before this, a three-round turn recorded three
// copies of an identical block; a plan reminder repeated its path and plan_id once per
// iteration.)
func TestAgentLoop_UnchangedReminderNotRepeated(t *testing.T) {
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			toolCallSeq("Bash", "call-1", `{"command":"echo one"}`),
			toolCallSeq("Bash", "call-2", `{"command":"echo two"}`),
			textSeq("done"),
		},
	}

	sm := &fakeSessionManager{}
	collector := &fixedReminderCollector{lines: []string{"Active plan: `/tmp/p.json` — 回写验证"}}
	a := newTestAgent(t, mp,
		func(a *AIAgent) { a.SetSessionManager(sm) },
		func(a *AIAgent) { a.SetReminderCollector(collector) },
	)
	a.RegisterTool(echoStub())

	result, _ := drainAgentEvents(a.RunConversationStream(t.Context(), nil, "go", "", llm.ChatOptions{MaxTokens: 4096}))
	require.NotNil(t, result)
	require.Equal(t, ExitReasonStop, result.ExitReason)

	// The collector still runs every round (it is how a CHANGED reminder would be
	// noticed) — it is the injection that is deduped.
	assert.Equal(t, 3, collector.collects)
	got := recordedReminders(t, sm)
	require.Len(t, got, 1, "the same reminder was injected more than once:\n%v", got)
	assert.Contains(t, got[0], "Active plan")
}

// TestAgentLoop_ChangedReminderStillInjected is the other half of the rule: deduping the
// unchanged must not silence a reminder that actually has something new to say.
func TestAgentLoop_ChangedReminderStillInjected(t *testing.T) {
	mp := &mockStreamProvider{
		name: "mock",
		sequences: [][]llm.StreamEvent{
			toolCallSeq("Bash", "call-1", `{"command":"echo one"}`),
			toolCallSeq("Bash", "call-2", `{"command":"echo two"}`),
			textSeq("done"),
		},
	}

	sm := &fakeSessionManager{}
	collector := &changingReminderCollector{}
	a := newTestAgent(t, mp,
		func(a *AIAgent) { a.SetSessionManager(sm) },
		func(a *AIAgent) { a.SetReminderCollector(collector) },
	)
	a.RegisterTool(echoStub())

	result, _ := drainAgentEvents(a.RunConversationStream(t.Context(), nil, "go", "", llm.ChatOptions{MaxTokens: 4096}))
	require.NotNil(t, result)

	got := recordedReminders(t, sm)
	require.Len(t, got, 3, "a reminder that changes every round must keep being injected:\n%v", got)
	assert.Contains(t, got[0], "round 1")
	assert.Contains(t, got[1], "round 2")
	assert.Contains(t, got[2], "round 3")
}
