package agent

import (
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/session"
)

// TestConvertSessionToLLMMessages_TrailingArtifactReminder guards the J1
// disk-reload path end-to-end: an artifact registered via
// session.Manager.AppendArtifact (which lands in messages.jsonl as the last
// message) must survive ConvertSessionToLLMMessages as a trailing user
// message — otherwise a follow-up turn after agent eviction/restart would
// never see the artifact reminder.
func TestConvertSessionToLLMMessages_TrailingArtifactReminder(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	mgr := session.NewManagerWithStore(store, nil)
	if _, err := mgr.New("openai", "."); err != nil {
		t.Fatalf("New: %v", err)
	}

	// Normal exchange, then an artifact appended as the trailing message.
	if err := mgr.AppendMessage(&session.Message{Type: session.MessageTypeUser, Content: "帮我 review"}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if err := mgr.AppendMessage(&session.Message{Type: session.MessageTypeAssistant, Content: "审查完成"}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if err := mgr.AppendArtifact(session.ArtifactRef{
		Kind:  session.ArtifactKindReview,
		Title: "代码审查（3 轮）",
		Path:  "/work/.tachi/reviews/20260802-210636/round-3-judge-x.md",
	}); err != nil {
		t.Fatalf("AppendArtifact: %v", err)
	}

	msgs, err := mgr.LoadMessages()
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	result, err := ConvertSessionToLLMMessages(msgs, "anthropic")
	if err != nil {
		t.Fatalf("ConvertSessionToLLMMessages: %v", err)
	}

	// The artifact reminder must appear in the output (as a trailing user
	// message carrying the <system-reminder> block).
	var last string
	for _, m := range result {
		if m.Role == "user" {
			last = m.Content
		}
	}
	if !strings.Contains(last, "代码审查（3 轮）") || !strings.Contains(last, "round-3-judge-x.md") {
		t.Errorf("artifact reminder lost in conversion; last user msg = %q", last)
	}
}

// TestConvertSessionToLLMMessages_ConsecutiveRemindersNotOverwritten guards
// the J1 cumulative-buffer fix: an artifact reminder followed by a per-turn
// reminder (e.g. the date block recorded by recordUserTurn) and THEN a real
// user message must keep BOTH reminders in the reloaded history. With a
// single-value buffer the artifact would be overwritten by the date reminder
// and lost forever — breaking "follow up after a restart" for any artifact
// that isn't the very last message.
func TestConvertSessionToLLMMessages_ConsecutiveRemindersNotOverwritten(t *testing.T) {
	artifactBlock := session.FormatArtifactReminder([]session.ArtifactRef{
		{Kind: session.ArtifactKindResearch, Title: "AI Agent 产品对比", Path: "/tmp/r.html"},
	})
	dateBlock := "<system-reminder>\nCurrent date: Sunday, August 2, 2026\n</system-reminder>"

	sessionMsgs := []session.Message{
		{Type: session.MessageTypeUser, Content: "帮我研究"},
		{Type: session.MessageTypeAssistant, Content: "研究完成"},
		// Artifact reminder, then this turn's date reminder, then a real
		// follow-up message — the layout recordUserTurn produces.
		{Type: session.MessageTypeReminder, Content: artifactBlock},
		{Type: session.MessageTypeReminder, Content: dateBlock},
		{Type: session.MessageTypeUser, Content: "追问"},
	}

	result, err := ConvertSessionToLLMMessages(sessionMsgs, "anthropic")
	if err != nil {
		t.Fatalf("ConvertSessionToLLMMessages: %v", err)
	}

	// The follow-up user message must carry BOTH reminders.
	found := false
	for _, m := range result {
		if m.Role == "user" && strings.Contains(m.Content, "追问") {
			found = true
			if !strings.Contains(m.Content, "AI Agent 产品对比") {
				t.Errorf("artifact reminder lost (overwritten by date reminder): %q", m.Content)
			}
			if !strings.Contains(m.Content, "Current date: Sunday") {
				t.Errorf("date reminder lost: %q", m.Content)
			}
		}
	}
	if !found {
		t.Fatalf("follow-up user message not in output: %+v", result)
	}
}
