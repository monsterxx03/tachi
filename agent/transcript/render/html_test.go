package render

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/agent/transcript"
	"github.com/monsterxx03/tachi/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatArgsJSON_Valid(t *testing.T) {
	result := formatArgsJSON(`{"key":"value","num":42}`)
	if !strings.Contains(result, `"key"`) || !strings.Contains(result, `"value"`) {
		t.Errorf("formatArgsJSON: unexpected output: %s", result)
	}
}

func TestFormatArgsJSON_Empty(t *testing.T) {
	result := formatArgsJSON("")
	if result != "" {
		t.Errorf("formatArgsJSON(''): want empty, got %q", result)
	}
}

func TestFormatArgsJSON_Invalid(t *testing.T) {
	raw := `not valid json at all`
	result := formatArgsJSON(raw)
	if result != raw {
		t.Errorf("formatArgsJSON(invalid): want raw, got %q", result)
	}
}

func TestFormatArgsJSON_Array(t *testing.T) {
	result := formatArgsJSON(`[1,2,3]`)
	if !strings.Contains(result, "1") {
		t.Errorf("formatArgsJSON(array): unexpected output: %s", result)
	}
}

func TestFormatTime(t *testing.T) {
	tm := time.Date(2026, 5, 21, 13, 30, 0, 0, time.UTC)
	result := formatTime(tm)
	expected := "2026-05-21 13:30:00"
	if result != expected {
		t.Errorf("formatTime = %q, want %q", result, expected)
	}
}

func TestFormatTime_Zero(t *testing.T) {
	result := formatTime(time.Time{})
	if result != "" {
		t.Errorf("formatTime(zero): want empty, got %q", result)
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		start, end time.Time
		want       string
	}{
		{time.Time{}, time.Now(), ""},                    // zero start
		{time.Now(), time.Time{}, ""},                    // zero end
		{time.Now(), time.Now().Add(-1 * time.Hour), ""}, // end before start
		{time.Date(2026, 5, 21, 13, 0, 0, 0, time.UTC),
			time.Date(2026, 5, 21, 13, 0, 30, 0, time.UTC), "30s"},
		{time.Date(2026, 5, 21, 13, 0, 0, 0, time.UTC),
			time.Date(2026, 5, 21, 13, 5, 30, 0, time.UTC), "5m 30s"},
		{time.Date(2026, 5, 21, 13, 0, 0, 0, time.UTC),
			time.Date(2026, 5, 21, 14, 30, 0, 0, time.UTC), "1h 30m"},
	}

	for _, tt := range tests {
		result := formatDuration(tt.start, tt.end)
		if result != tt.want {
			t.Errorf("formatDuration(%v, %v) = %q, want %q", tt.start, tt.end, result, tt.want)
		}
	}
}

func TestConvertArgsToString_String(t *testing.T) {
	result := convertArgsToString("direct string")
	if result != "direct string" {
		t.Errorf("convertArgsToString = %q, want %q", result, "direct string")
	}
}

func TestConvertArgsToString_Nil(t *testing.T) {
	result := convertArgsToString(nil)
	if result != "" {
		t.Errorf("convertArgsToString(nil) = %q, want empty", result)
	}
}

func TestConvertArgsToString_Map(t *testing.T) {
	result := convertArgsToString(map[string]any{"key": "value"})
	if !strings.Contains(result, `"key"`) || !strings.Contains(result, `"value"`) {
		t.Errorf("convertArgsToString(map): unexpected output: %s", result)
	}
}

func TestConvertArgsToString_Int(t *testing.T) {
	result := convertArgsToString(42)
	if result != "42" {
		t.Errorf("convertArgsToString(42) = %q, want %q", result, "42")
	}
}

func TestSortFreq(t *testing.T) {
	freq := map[string]int{"Bash": 5, "Read": 3, "Edit": 7}
	result := sortFreq(freq)
	if len(result) != 3 {
		t.Fatalf("sortFreq: got %d entries, want 3", len(result))
	}
	if result[0].Key != "Edit" || result[0].Value != 7 {
		t.Errorf("first = %s:%d, want Edit:7", result[0].Key, result[0].Value)
	}
	if result[1].Key != "Bash" || result[1].Value != 5 {
		t.Errorf("second = %s:%d, want Bash:5", result[1].Key, result[1].Value)
	}
	if result[2].Key != "Read" || result[2].Value != 3 {
		t.Errorf("third = %s:%d, want Read:3", result[2].Key, result[2].Value)
	}
}

func TestSortFreq_Empty(t *testing.T) {
	result := sortFreq(map[string]int{})
	if len(result) != 0 {
		t.Errorf("sortFreq(empty): want 0 entries, got %d", len(result))
	}
}

func TestEventIconAndClass(t *testing.T) {
	tests := []struct {
		et        transcript.EventType
		name      string
		isError   bool
		wantIcon  string
		wantClass string
	}{
		{transcript.EventUser, "", false, "👤", "event-user"},
		{transcript.EventThinking, "", false, "💭", "event-thinking"},
		{transcript.EventText, "", false, "💬", "event-text"},
		{transcript.EventToolCall, "Bash", false, "🔧", "event-tool-call"},
		{transcript.EventToolCall, "SubAgent", false, "🔀", "event-subagent"},
		{transcript.EventToolResult, "", false, "📋", "event-tool-result"},
		{transcript.EventToolResult, "", true, "❌", "event-tool-result event-error"},
		{transcript.EventType("unknown"), "", false, "•", ""},
	}

	for _, tt := range tests {
		icon, cls := eventIconAndClass(tt.et, tt.name, tt.isError)
		if icon != tt.wantIcon {
			t.Errorf("eventIconAndClass(%q, %q, %v): icon = %q, want %q",
				tt.et, tt.name, tt.isError, icon, tt.wantIcon)
		}
		if cls != tt.wantClass {
			t.Errorf("eventIconAndClass(%q, %q, %v): class = %q, want %q",
				tt.et, tt.name, tt.isError, cls, tt.wantClass)
		}
	}
}

func TestSessionIconAndClass(t *testing.T) {
	tests := []struct {
		msg       session.Message
		wantIcon  string
		wantClass string
	}{
		{session.Message{Type: session.MessageTypeUser}, "👤", "event-user"},
		{session.Message{Type: session.MessageTypeAssistant}, "💬", "event-text"},
		{session.Message{Type: session.MessageTypeThinking}, "💭", "event-thinking"},
		{session.Message{Type: session.MessageTypeToolCall, Name: "Bash"}, "🔧", "event-tool-call"},
		{session.Message{Type: session.MessageTypeToolCall, Name: "SubAgent"}, "🔀", "event-subagent"},
		{session.Message{Type: session.MessageTypeToolResult}, "📋", "event-tool-result"},
		{session.Message{Type: session.MessageTypeToolResult, IsError: true}, "❌", "event-tool-result event-error"},
		{session.Message{Type: session.MessageTypeConfirm}, "•", ""},
	}

	for _, tt := range tests {
		icon, cls := sessionIconAndClass(tt.msg)
		if icon != tt.wantIcon {
			t.Errorf("sessionIconAndClass(%q): icon = %q, want %q",
				tt.msg.Type, icon, tt.wantIcon)
		}
		if cls != tt.wantClass {
			t.Errorf("sessionIconAndClass(%q): class = %q, want %q",
				tt.msg.Type, cls, tt.wantClass)
		}
	}
}

func TestCountTurnsFromMessages(t *testing.T) {
	msgs := []session.Message{
		{Type: session.MessageTypeUser},
		{Type: session.MessageTypeAssistant},
		{Type: session.MessageTypeUser},
		{Type: session.MessageTypeAssistant},
	}
	count := countTurnsFromMessages(msgs)
	if count != 2 {
		t.Errorf("countTurnsFromMessages = %d, want 2", count)
	}
}

func TestCountTurnsFromMessages_Empty(t *testing.T) {
	count := countTurnsFromMessages(nil)
	if count != 0 {
		t.Errorf("countTurnsFromMessages(nil) = %d, want 0", count)
	}
}

func TestBuildStatsFromMessages(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	msgs := []session.Message{
		{Type: session.MessageTypeUser},
		{Type: session.MessageTypeThinking},
		{Type: session.MessageTypeAssistant},
		{Type: session.MessageTypeToolCall, Name: "Bash"},
		{Type: session.MessageTypeToolCall, Name: "SubAgent"},
		{Type: session.MessageTypeToolResult},
		{Type: session.MessageTypeToolResult, IsError: true},
		{Type: session.MessageTypeUser},
		{Type: session.MessageTypeAssistant},
	}

	stats := buildStatsFromMessages(msgs, nil, start, end)

	if stats.UserMsgCount != 2 {
		t.Errorf("UserMsgCount = %d, want 2", stats.UserMsgCount)
	}
	if stats.ThinkingCount != 1 {
		t.Errorf("ThinkingCount = %d, want 1", stats.ThinkingCount)
	}
	if stats.TextCount != 2 {
		t.Errorf("TextCount = %d, want 2", stats.TextCount)
	}
	if stats.ToolCallCount != 2 {
		t.Errorf("ToolCallCount = %d, want 2", stats.ToolCallCount)
	}
	if stats.ToolErrorCount != 1 {
		t.Errorf("ToolErrorCount = %d, want 1", stats.ToolErrorCount)
	}
	if stats.SubAgentCount != 1 {
		t.Errorf("SubAgentCount = %d, want 1", stats.SubAgentCount)
	}
	if stats.TurnCount != 2 {
		t.Errorf("TurnCount = %d, want 2", stats.TurnCount)
	}
	if stats.TotalDuration != "1h 0m" {
		t.Errorf("TotalDuration = %q, want %q", stats.TotalDuration, "1h 0m")
	}
	if len(stats.ToolFreq) != 2 {
		t.Fatalf("ToolFreq length = %d, want 2", len(stats.ToolFreq))
	}
	// SubAgent should come first (alphabetically? No, by count. Both have count 1.
	// The sort is by value descending; same values have no guaranteed order.
	// Just verify both tools are present.
	names := map[string]bool{"Bash": true, "SubAgent": true}
	for _, kv := range stats.ToolFreq {
		if !names[kv.Key] {
			t.Errorf("unexpected tool in freq: %s", kv.Key)
		}
		if kv.Value != 1 {
			t.Errorf("freq for %s = %d, want 1", kv.Key, kv.Value)
		}
	}
}

func TestBuildReportData(t *testing.T) {
	s := &session.Session{
		ID:           "test-session-1",
		Title:        "Test Session",
		ProviderName: "anthropic",
		CreatedAt:    time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC),
		UpdatedAt:    time.Date(2026, 5, 21, 11, 0, 0, 0, time.UTC),
	}

	tr := &transcript.Transcript{
		SessionID: "test-session-1",
		Turns: []transcript.Turn{
			{
				ID: 1,
				Events: []transcript.Event{
					{
						Type:      transcript.EventUser,
						Timestamp: time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC),
						Content:   "hello",
					},
					{
						Type:      transcript.EventText,
						Timestamp: time.Date(2026, 5, 21, 10, 0, 5, 0, time.UTC),
						Content:   "Hi! How can I help?",
					},
				},
			},
		},
	}

	data := BuildReportData(s, tr)

	if data.Session == nil {
		t.Fatal("Session is nil")
	}
	if data.Session.ID != "test-session-1" {
		t.Errorf("Session.ID = %q", data.Session.ID)
	}
	if data.Session.Title != "Test Session" {
		t.Errorf("Session.Title = %q", data.Session.Title)
	}
	if data.Session.Duration != "1h 0m" {
		t.Errorf("Session.Duration = %q", data.Session.Duration)
	}

	if data.Transcript == nil {
		t.Fatal("Transcript is nil")
	}
	if len(data.Transcript.Turns) != 1 {
		t.Fatalf("len(Turns) = %d, want 1", len(data.Transcript.Turns))
	}
	if data.Transcript.Turns[0].UserInput != "hello" {
		t.Errorf("UserInput = %q, want %q", data.Transcript.Turns[0].UserInput, "hello")
	}
	if data.Transcript.Turns[0].Duration != "5s" {
		t.Errorf("Turn Duration = %q, want %q", data.Transcript.Turns[0].Duration, "5s")
	}

	if data.Stats.UserMsgCount != 1 {
		t.Errorf("UserMsgCount = %d, want 1", data.Stats.UserMsgCount)
	}
	if data.Stats.TextCount != 1 {
		t.Errorf("TextCount = %d, want 1", data.Stats.TextCount)
	}
}

func TestBuildReportData_SubagentEvents(t *testing.T) {
	s := &session.Session{
		ID:           "sub-session",
		Title:        "Sub Session",
		ProviderName: "anthropic",
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}

	tr := &transcript.Transcript{
		Turns: []transcript.Turn{
			{
				ID: 1,
				Events: []transcript.Event{
					{
						Type:      transcript.EventToolCall,
						Name:      "SubAgent",
						Timestamp: time.Now(),
						Args:      `{"prompt":"test","allowed_tools":["Bash","Read"]}`,
						Children: []transcript.Event{
							{
								Type:      transcript.EventThinking,
								Timestamp: time.Now(),
								Content:   "child thinking",
							},
							{
								Type:      transcript.EventToolCall,
								Name:      "Bash",
								Timestamp: time.Now(),
								Args:      `{"command":"echo hello"}`,
							},
							{
								Type:      transcript.EventToolResult,
								Name:      "Bash",
								Timestamp: time.Now(),
								Content:   "hello",
							},
						},
					},
				},
			},
		},
	}

	data := BuildReportData(s, tr)

	if len(data.Transcript.Turns) != 1 {
		t.Fatalf("len(Turns) = %d, want 1", len(data.Transcript.Turns))
	}
	parentEvent := data.Transcript.Turns[0].Events[0]
	if !parentEvent.HasChildren {
		t.Error("parent SubAgent event should have children")
	}
	if len(parentEvent.Children) != 3 {
		t.Errorf("len(Children) = %d, want 3", len(parentEvent.Children))
	}
	// Check children icons/types
	if parentEvent.Children[0].Icon != "💭" {
		t.Errorf("child 0 icon = %q, want 💭", parentEvent.Children[0].Icon)
	}
	if parentEvent.Children[1].Icon != "🔧" {
		t.Errorf("child 1 icon = %q, want 🔧", parentEvent.Children[1].Icon)
	}
	if parentEvent.Children[2].Icon != "📋" {
		t.Errorf("child 2 icon = %q, want 📋", parentEvent.Children[2].Icon)
	}

	if data.Stats.SubAgentCount != 1 {
		t.Errorf("SubAgentCount = %d, want 1", data.Stats.SubAgentCount)
	}
}

func TestBuildReportDataFromMessages(t *testing.T) {
	s := &session.Session{
		ID:           "msg-session",
		Title:        "Message Session",
		ProviderName: "openai",
		CreatedAt:    time.Date(2026, 5, 21, 9, 0, 0, 0, time.UTC),
		UpdatedAt:    time.Date(2026, 5, 21, 9, 30, 0, 0, time.UTC),
	}

	msgs := []session.Message{
		{
			Type:      session.MessageTypeUser,
			Content:   "What is Go?",
			Timestamp: time.Date(2026, 5, 21, 9, 0, 0, 0, time.UTC),
		},
		{
			Type:      session.MessageTypeAssistant,
			Content:   "Go is a statically typed, compiled programming language.",
			Timestamp: time.Date(2026, 5, 21, 9, 30, 0, 0, time.UTC),
		},
	}

	data := BuildReportDataFromMessages(s, msgs, nil)

	if data.Session.ID != "msg-session" {
		t.Errorf("Session.ID = %q", data.Session.ID)
	}
	if data.Session.Duration != "30m 0s" {
		t.Errorf("Session.Duration = %q", data.Session.Duration)
	}
	if len(data.Transcript.Turns) != 1 {
		t.Fatalf("len(Turns) = %d, want 1", len(data.Transcript.Turns))
	}
	if data.Transcript.Turns[0].Events[0].Icon != "👤" {
		t.Errorf("first event icon = %q", data.Transcript.Turns[0].Events[0].Icon)
	}
	if data.Transcript.Turns[0].Events[1].Icon != "💬" {
		t.Errorf("second event icon = %q", data.Transcript.Turns[0].Events[1].Icon)
	}
	if data.Stats.UserMsgCount != 1 {
		t.Errorf("UserMsgCount = %d, want 1", data.Stats.UserMsgCount)
	}
	if data.Stats.TextCount != 1 {
		t.Errorf("TextCount = %d, want 1", data.Stats.TextCount)
	}
}

func TestBuildReportDataFromMessages_MultipleTurns(t *testing.T) {
	s := &session.Session{
		ID:        "multi-turn",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	msgs := []session.Message{
		{Type: session.MessageTypeUser, Content: "Q1", Timestamp: time.Now()},
		{Type: session.MessageTypeAssistant, Content: "A1", Timestamp: time.Now()},
		{Type: session.MessageTypeUser, Content: "Q2", Timestamp: time.Now()},
		{Type: session.MessageTypeAssistant, Content: "A2", Timestamp: time.Now()},
	}

	data := BuildReportDataFromMessages(s, msgs, nil)

	if len(data.Transcript.Turns) != 2 {
		t.Errorf("len(Turns) = %d, want 2", len(data.Transcript.Turns))
	}
	if data.Stats.TurnCount != 2 {
		t.Errorf("TurnCount = %d, want 2", data.Stats.TurnCount)
	}
	if data.Stats.UserMsgCount != 2 {
		t.Errorf("UserMsgCount = %d, want 2", data.Stats.UserMsgCount)
	}
}

func TestBuildReportDataFromMessages_SubagentChildren(t *testing.T) {
	s := &session.Session{
		ID:        "msg-sub",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	msgs := []session.Message{
		{Type: session.MessageTypeUser, Content: "research this", Timestamp: time.Now()},
		{
			Type:       session.MessageTypeToolCall,
			Name:       "SubAgent",
			Args:       `{"prompt":"search for files"}`,
			ToolCallID: "call-1",
			Timestamp:  time.Now(),
		},
		{
			Type:       session.MessageTypeToolResult,
			Name:       "SubAgent",
			Result:     "final answer",
			ToolCallID: "call-1",
			SubagentID: "sub-abc",
			Timestamp:  time.Now(),
		},
		// A SubAgent call whose sidecar file is missing must not break anything.
		{
			Type:       session.MessageTypeToolCall,
			Name:       "SubAgent",
			Args:       `{"prompt":"lost"}`,
			ToolCallID: "call-2",
			Timestamp:  time.Now(),
		},
		{
			Type:       session.MessageTypeToolResult,
			Name:       "SubAgent",
			Result:     "gone",
			ToolCallID: "call-2",
			SubagentID: "sub-missing",
			Timestamp:  time.Now(),
		},
		{Type: session.MessageTypeAssistant, Content: "done", Timestamp: time.Now()},
	}

	subagents := map[string][]session.Message{
		"sub-abc": {
			{Type: session.MessageTypeUser, Content: "search for files", Timestamp: time.Now()},
			{Type: session.MessageTypeThinking, Content: "child thinking", Timestamp: time.Now()},
			{Type: session.MessageTypeToolCall, Name: "Bash", Args: `{"command":"ls"}`, ToolCallID: "sub-call-1", Timestamp: time.Now()},
			{Type: session.MessageTypeToolResult, Name: "Bash", Result: "file.go", ToolCallID: "sub-call-1", Timestamp: time.Now()},
			{Type: session.MessageTypeAssistant, Content: "final answer", Timestamp: time.Now()},
		},
	}

	data := BuildReportDataFromMessages(s, msgs, subagents)

	if len(data.Transcript.Turns) != 1 {
		t.Fatalf("len(Turns) = %d, want 1", len(data.Transcript.Turns))
	}
	events := data.Transcript.Turns[0].Events

	// events: user, SubAgent call (children), SubAgent result, SubAgent call (no children), SubAgent result, text
	var withKids, withoutKids *EventView
	for i := range events {
		ev := &events[i]
		if ev.Type == "tool_call" && ev.Name == "SubAgent" {
			if ev.HasChildren {
				withKids = ev
			} else {
				withoutKids = ev
			}
		}
	}

	if withKids == nil {
		t.Fatal("SubAgent call with sidecar messages should have children")
	}
	if withKids.Icon != "🔀" || withKids.CSSClass != "event-subagent" {
		t.Errorf("subagent parent icon/class = %q/%q", withKids.Icon, withKids.CSSClass)
	}
	if len(withKids.Children) != 5 {
		t.Fatalf("len(Children) = %d, want 5", len(withKids.Children))
	}
	wantIcons := []string{"👤", "💭", "🔧", "📋", "💬"}
	for i, want := range wantIcons {
		if withKids.Children[i].Icon != want {
			t.Errorf("child %d icon = %q, want %q", i, withKids.Children[i].Icon, want)
		}
	}
	if withKids.Children[0].Content != "search for files" {
		t.Errorf("child prompt = %q", withKids.Children[0].Content)
	}

	if withoutKids == nil {
		t.Fatal("SubAgent call without sidecar messages should still render")
	}
	if len(withoutKids.Children) != 0 {
		t.Errorf("orphan SubAgent call should have no children, got %d", len(withoutKids.Children))
	}

	// Stats: sub-agent messages fold in, but its user prompt is not counted.
	if data.Stats.SubAgentCount != 2 {
		t.Errorf("SubAgentCount = %d, want 2", data.Stats.SubAgentCount)
	}
	if data.Stats.UserMsgCount != 1 {
		t.Errorf("UserMsgCount = %d, want 1 (sub prompt excluded)", data.Stats.UserMsgCount)
	}
	if data.Stats.ToolCallCount != 3 {
		t.Errorf("ToolCallCount = %d, want 3 (2 SubAgent + 1 Bash)", data.Stats.ToolCallCount)
	}
	if data.Stats.ThinkingCount != 1 {
		t.Errorf("ThinkingCount = %d, want 1", data.Stats.ThinkingCount)
	}
	if data.Stats.TextCount != 2 {
		t.Errorf("TextCount = %d, want 2", data.Stats.TextCount)
	}
	var bashFreq int
	for _, kv := range data.Stats.ToolFreq {
		if kv.Key == "Bash" {
			bashFreq = kv.Value
		}
	}
	if bashFreq != 1 {
		t.Errorf("Bash freq = %d, want 1", bashFreq)
	}
}

func TestGenerateHTML(t *testing.T) {
	s := &session.Session{
		ID:           "html-test",
		Title:        "HTML Report Test",
		ProviderName: "anthropic",
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}

	tr := &transcript.Transcript{
		Turns: []transcript.Turn{
			{
				ID: 1,
				Events: []transcript.Event{
					{
						Type:      transcript.EventUser,
						Timestamp: time.Now(),
						Content:   "help me",
					},
				},
			},
		},
	}

	data := BuildReportData(s, tr)
	html, err := GenerateHTML(data)
	if err != nil {
		t.Fatalf("GenerateHTML: unexpected error: %v", err)
	}
	if !strings.Contains(html, "<!DOCTYPE html>") && !strings.Contains(html, "<html") {
		t.Error("GenerateHTML output should contain HTML markup")
	}
	if !strings.Contains(html, "HTML Report Test") {
		t.Error("HTML should contain session title")
	}
	if !strings.Contains(html, "help me") {
		t.Error("HTML should contain user input content")
	}
}

func TestGenerateHTML_Empty(t *testing.T) {
	s := &session.Session{
		ID:        "empty",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	tr := &transcript.Transcript{}

	data := BuildReportData(s, tr)
	html, err := GenerateHTML(data)
	if err != nil {
		t.Fatalf("GenerateHTML(empty): unexpected error: %v", err)
	}
	if html == "" {
		t.Error("GenerateHTML should not return empty string")
	}
}

func TestSessionMessageToEventView_ToolCall(t *testing.T) {
	msg := session.Message{
		Type:      session.MessageTypeToolCall,
		Name:      "Bash",
		Args:      map[string]any{"command": "ls -la"},
		Timestamp: time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC),
	}

	ev := sessionMessageToEventView(msg)

	if ev.Type != "tool_call" {
		t.Errorf("Type = %q, want tool_call", ev.Type)
	}
	if ev.Name != "Bash" {
		t.Errorf("Name = %q, want Bash", ev.Name)
	}
	if !strings.Contains(ev.ArgsRaw, "ls -la") {
		t.Errorf("ArgsRaw doesn't contain command: %s", ev.ArgsRaw)
	}
	if ev.Icon != "🔧" {
		t.Errorf("Icon = %q, want 🔧", ev.Icon)
	}
}

func TestSessionMessageToEventView_ToolResult(t *testing.T) {
	msg := session.Message{
		Type:      session.MessageTypeToolResult,
		Name:      "Bash",
		Result:    "stdout output",
		IsError:   false,
		Timestamp: time.Now(),
	}

	ev := sessionMessageToEventView(msg)

	if ev.Type != "tool_result" {
		t.Errorf("Type = %q, want tool_result", ev.Type)
	}
	if ev.Content != "stdout output" {
		t.Errorf("Content = %q, want %q", ev.Content, "stdout output")
	}
	if ev.IsError {
		t.Error("IsError should be false")
	}
	if ev.Icon != "📋" {
		t.Errorf("Icon = %q, want 📋", ev.Icon)
	}
}

// findGroups returns the request groups of a turn in order.
func findGroups(turn TurnView) []*RequestGroupView {
	var groups []*RequestGroupView
	for i := range turn.Items {
		if turn.Items[i].Kind == "group" {
			groups = append(groups, turn.Items[i].Group)
		}
	}
	return groups
}

func TestBuildReportDataFromMessagesWithRequests(t *testing.T) {
	s := &session.Session{ID: "api-reqs", CreatedAt: time.Now(), UpdatedAt: time.Now()}

	t0 := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 8, 15, 10, 1, 0, 0, time.UTC)

	msgs := []session.Message{
		{Type: session.MessageTypeUser, Content: "Q1", Timestamp: t0},
		{Type: session.MessageTypeAssistant, Content: "A1", Timestamp: t0.Add(30 * time.Second), Iteration: 1},
		{Type: session.MessageTypeUser, Content: "Q2", Timestamp: t1},
		{Type: session.MessageTypeAssistant, Content: "A2", Timestamp: t1.Add(30 * time.Second), Iteration: 2},
	}

	reqs := []session.APIRequest{
		{Timestamp: t0.Add(5 * time.Second), Iteration: 1, UserPrompt: "Q1", SystemPrompt: "SP1", Model: "mock-model", Provider: "deepseek-v4-flash", Thinking: "high", Tools: []session.APITool{{Name: "ReadFile", Parameters: json.RawMessage(`{"type":"object"}`)}}},
		{Timestamp: t1.Add(5 * time.Second), Iteration: 2, UserPrompt: "Q2", SystemPrompt: "SP1", Model: "mock-model", Provider: "deepseek-v4-flash", Thinking: "high", Tools: []session.APITool{{Name: "Bash", Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`)}}},
	}

	data := BuildReportDataFromMessagesWithRequests(s, msgs, nil, reqs)

	if data.Stats.APICallCount != 2 {
		t.Errorf("APICallCount = %d, want 2", data.Stats.APICallCount)
	}
	if len(data.Transcript.Turns) != 2 {
		t.Fatalf("len(Turns) = %d, want 2", len(data.Transcript.Turns))
	}

	// Turn 1: user event + one request group (iteration 1).
	turn1 := data.Transcript.Turns[0]
	if len(turn1.APIRequests) != 1 || turn1.APIRequests[0].Iteration != 1 {
		t.Fatalf("turn1 APIRequests = %+v, want iteration 1", turn1.APIRequests)
	}
	groups1 := findGroups(turn1)
	if len(groups1) != 1 {
		t.Fatalf("turn1 groups = %d, want 1", len(groups1))
	}
	g1 := groups1[0]
	if g1.Iteration != 1 || g1.UserPrompt != "Q1" || g1.SystemPrompt != "SP1" {
		t.Errorf("group1 = iter %d / user %q / sp %q, want 1/Q1/SP1", g1.Iteration, g1.UserPrompt, g1.SystemPrompt)
	}
	if g1.Model != "mock-model" || g1.Provider != "deepseek-v4-flash" || g1.Thinking != "high" {
		t.Errorf("group1 = model %q / provider %q / thinking %q, want mock-model/deepseek-v4-flash/high", g1.Model, g1.Provider, g1.Thinking)
	}
	if g1.SystemPromptSame || g1.AllSame {
		t.Error("group1 marked same — nothing precedes it")
	}
	if len(g1.Tools) != 1 || g1.Tools[0].Name != "ReadFile" {
		t.Errorf("group1 Tools = %+v, want [ReadFile]", g1.Tools)
	}
	if g1.Tools[0].Schema == "" {
		t.Error("group1 tool schema empty, want pretty JSON")
	}
	// The iteration-1 assistant event lives inside the group.
	if len(g1.Events) != 1 || g1.Events[0].Content != "A1" {
		t.Errorf("group1 Events = %+v, want [A1]", g1.Events)
	}

	// Turn 2: user event + group (iteration 2), same prompt, different tools.
	turn2 := data.Transcript.Turns[1]
	groups2 := findGroups(turn2)
	if len(groups2) != 1 {
		t.Fatalf("turn2 groups = %d, want 1", len(groups2))
	}
	g2 := groups2[0]
	if g2.Iteration != 2 || !g2.SystemPromptSame {
		t.Errorf("group2 = iter %d same=%v, want iter 2 / same prompt", g2.Iteration, g2.SystemPromptSame)
	}
	if len(g2.Tools) != 1 || g2.Tools[0].Name != "Bash" {
		t.Errorf("group2 Tools = %+v, want [Bash]", g2.Tools)
	}
	if g2.ToolsSame || g2.AllSame {
		t.Error("group2 marked fully same — tools differ")
	}
}

func TestRequestGroup_MultipleToolCallsInOneRequest(t *testing.T) {
	// Core requirement: several tool calls issued by the SAME API request are
	// grouped together in one request group.
	s := &session.Session{ID: "multi-tool-group", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	t0 := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

	msgs := []session.Message{
		{Type: session.MessageTypeUser, Content: "Q1", Timestamp: t0},
		{Type: session.MessageTypeToolCall, Name: "Bash", ToolCallID: "c1", Timestamp: t0.Add(5 * time.Second), Iteration: 1},
		{Type: session.MessageTypeToolCall, Name: "ReadFile", ToolCallID: "c2", Timestamp: t0.Add(6 * time.Second), Iteration: 1},
		{Type: session.MessageTypeToolResult, Name: "Bash", ToolCallID: "c1", Timestamp: t0.Add(7 * time.Second), Iteration: 1},
		{Type: session.MessageTypeToolResult, Name: "ReadFile", ToolCallID: "c2", Timestamp: t0.Add(8 * time.Second), Iteration: 1},
		{Type: session.MessageTypeAssistant, Content: "A1", Timestamp: t0.Add(9 * time.Second), Iteration: 1},
	}
	reqs := []session.APIRequest{
		{Timestamp: t0.Add(2 * time.Second), Iteration: 1, UserPrompt: "Q1", SystemPrompt: "SP", Tools: []session.APITool{{Name: "Bash"}, {Name: "ReadFile"}}},
	}

	data := BuildReportDataFromMessagesWithRequests(s, msgs, nil, reqs)
	groups := findGroups(data.Transcript.Turns[0])
	require.Len(t, groups, 1, "one request group")

	// Both tool calls (and their results + final text) live in the group.
	var toolCalls []string
	for _, ev := range groups[0].Events {
		if ev.Type == "tool_call" {
			toolCalls = append(toolCalls, ev.Name)
		}
	}
	assert.Equal(t, []string{"Bash", "ReadFile"}, toolCalls, "tool calls grouped under their request")
	if len(groups[0].Events) != 5 {
		t.Errorf("group events = %d, want 5 (2 calls + 2 results + text)", len(groups[0].Events))
	}
}

func TestBuildReportDataFromMessagesWithRequests_NoRequests(t *testing.T) {
	s := &session.Session{ID: "no-api-reqs", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	msgs := []session.Message{
		{Type: session.MessageTypeUser, Content: "Q1", Timestamp: time.Now()},
		{Type: session.MessageTypeAssistant, Content: "A1", Timestamp: time.Now()},
	}

	// nil requests → no groups, all events flat, no crash.
	data := BuildReportDataFromMessagesWithRequests(s, msgs, nil, nil)
	if len(data.Transcript.Turns) != 1 {
		t.Fatalf("len(Turns) = %d, want 1", len(data.Transcript.Turns))
	}
	turn := data.Transcript.Turns[0]
	if len(turn.APIRequests) != 0 {
		t.Errorf("APIRequests = %d, want 0", len(turn.APIRequests))
	}
	if groups := findGroups(turn); len(groups) != 0 {
		t.Errorf("groups = %d, want 0 (no requests recorded)", len(groups))
	}
	if data.Stats.APICallCount != 0 {
		t.Errorf("APICallCount = %d, want 0", data.Stats.APICallCount)
	}
}

func TestAssignAPIRequests_EmptyTurns(t *testing.T) {
	// Guards: no turns / mismatched starts must not panic or modify.
	turns := []TurnView{}
	assignAPIRequests(turns, nil, nil, []session.APIRequest{{Timestamp: time.Now(), SystemPrompt: "SP"}})

	turns = []TurnView{{ID: 1}}
	assignAPIRequests(turns, []time.Time{}, [][]time.Time{}, []session.APIRequest{{Timestamp: time.Now(), SystemPrompt: "SP"}})
	if len(turns[0].APIRequests) != 0 {
		t.Error("request leaked into turn despite start mismatch")
	}
}

func TestGenerateHTMLWithAPIRequests(t *testing.T) {
	s := &session.Session{
		ID:        "html-api-reqs",
		Title:     "HTML API Req Test",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	msgs := []session.Message{
		{Type: session.MessageTypeUser, Content: "Q1", Timestamp: time.Now()},
		{Type: session.MessageTypeAssistant, Content: "A1", Timestamp: time.Now(), Iteration: 1},
	}
	reqs := []session.APIRequest{{
		Timestamp:    time.Now(),
		Iteration:    1,
		UserPrompt:   "Q1",
		SystemPrompt: "You are Tachi — the test system prompt.",
		Tools: []session.APITool{{
			Name:        "ReadFile",
			Description: "Read a file from disk",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"file path"}},"required":["path"]}`),
		}},
	}}

	data := BuildReportDataFromMessagesWithRequests(s, msgs, nil, reqs)
	html, err := GenerateHTML(data)
	if err != nil {
		t.Fatalf("GenerateHTML: %v", err)
	}

	for _, want := range []string{
		"API 请求 #1",
		"System Prompt",
		"You are Tachi — the test system prompt.",
		"Tools（1）",
		"Q1", // request group bound to the user prompt (rg-prompt)
		"ReadFile",
		"Read a file from disk",
		"file path", // schema survives into the report (quotes are HTML-escaped)
		"API 调用",    // sidebar stat
	} {
		if !strings.Contains(html, want) {
			t.Errorf("generated HTML missing %q", want)
		}
	}
}

func TestBuildReportDataWithReminderLeadingTurn(t *testing.T) {
	// Real sessions open each turn with a <system-reminder> block buffered
	// ahead of the user message. Regression test: turn starts must still be
	// attributed so API requests land in their groups.
	s := &session.Session{ID: "reminder-turn", CreatedAt: time.Now(), UpdatedAt: time.Now()}

	t0 := time.Date(2026, 8, 15, 11, 0, 0, 0, time.UTC)
	msgs := []session.Message{
		{Type: session.MessageTypeReminder, Content: "<system-reminder>context</system-reminder>", Timestamp: t0},
		{Type: session.MessageTypeUser, Content: "Q1", Timestamp: t0.Add(time.Second)},
		{Type: session.MessageTypeAssistant, Content: "A1", Timestamp: t0.Add(30 * time.Second), Iteration: 1},
		{Type: session.MessageTypeReminder, Content: "<system-reminder>more</system-reminder>", Timestamp: t0.Add(time.Minute)},
		{Type: session.MessageTypeUser, Content: "Q2", Timestamp: t0.Add(61 * time.Second)},
		{Type: session.MessageTypeAssistant, Content: "A2", Timestamp: t0.Add(90 * time.Second), Iteration: 2},
	}
	reqs := []session.APIRequest{
		{Timestamp: t0.Add(5 * time.Second), Iteration: 1, UserPrompt: "Q1", SystemPrompt: "SP", Tools: []session.APITool{{Name: "Grep"}}},
		{Timestamp: t0.Add(65 * time.Second), Iteration: 2, UserPrompt: "Q2", SystemPrompt: "SP", Tools: []session.APITool{{Name: "Bash"}}},
	}

	data := BuildReportDataFromMessagesWithRequests(s, msgs, nil, reqs)
	if len(data.Transcript.Turns) != 2 {
		t.Fatalf("len(Turns) = %d, want 2", len(data.Transcript.Turns))
	}
	groups1 := findGroups(data.Transcript.Turns[0])
	groups2 := findGroups(data.Transcript.Turns[1])
	if len(groups1) != 1 {
		t.Errorf("turn1 groups = %d, want 1 (reminder-led turn)", len(groups1))
	}
	if len(groups2) != 1 {
		t.Errorf("turn2 groups = %d, want 1", len(groups2))
	}
	if got := groups1[0].Tools[0].Name; got != "Grep" {
		t.Errorf("turn1 tool = %q, want Grep", got)
	}
	if got := groups2[0].Tools[0].Name; got != "Bash" {
		t.Errorf("turn2 tool = %q, want Bash", got)
	}
	if got := groups1[0].UserPrompt; got != "Q1" {
		t.Errorf("group1 user prompt = %q, want Q1", got)
	}
}
