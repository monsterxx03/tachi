// Package render converts transcript data into visual formats.
package render

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent/transcript"
	"github.com/monsterxx03/tachi/pkg/strutil"
	"github.com/monsterxx03/tachi/session"
)

//go:embed templates/report.html
var reportTemplate string

// ReportData is the view model passed to the HTML template.
type ReportData struct {
	Session    *SessionView
	Transcript *TranscriptView
	Stats      StatsView
}

// SessionView is a display-friendly session summary.
type SessionView struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Provider  string `json:"provider"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	Duration  string `json:"duration"`
}

// TranscriptView wraps transcript.Transcript with display helpers.
type TranscriptView struct {
	Turns []TurnView `json:"turns"`
}

// TurnView is a display-friendly turn.
type TurnView struct {
	ID        int         `json:"id"`
	Events    []EventView `json:"events"`
	UserInput string      `json:"user_input,omitempty"`
	Duration  string      `json:"duration,omitempty"`
}

// EventView is a display-friendly event.
type EventView struct {
	Type        string      `json:"type"`
	Timestamp   string      `json:"ts"`
	Name        string      `json:"name,omitempty"`
	Content     string      `json:"content,omitempty"`
	ArgsJSON    string      `json:"args_json,omitempty"` // formatted JSON
	ArgsRaw     string      `json:"args_raw,omitempty"`  // raw string
	IsError     bool        `json:"is_error,omitempty"`
	HasChildren bool        `json:"has_children,omitempty"`
	Children    []EventView `json:"children,omitempty"`
	Icon        string      `json:"icon"`
	CSSClass    string      `json:"css_class"`
}

// StatsView holds aggregated statistics for the transcript.
type StatsView struct {
	TurnCount      int    `json:"turn_count"`
	UserMsgCount   int    `json:"user_msg_count"`
	ToolCallCount  int    `json:"tool_call_count"`
	ToolErrorCount int    `json:"tool_error_count"`
	ThinkingCount  int    `json:"thinking_count"`
	TextCount      int    `json:"text_count"`
	SubAgentCount  int    `json:"subagent_count"`
	ToolFreq       []KV   `json:"tool_freq"`
	TotalDuration  string `json:"total_duration"`
}

// KV is a key-value pair for sorted display.
type KV struct {
	Key   string `json:"key"`
	Value int    `json:"value"`
}

// BuildReportData converts a session and transcript into template-ready data.
func BuildReportData(s *session.Session, tr *transcript.Transcript) *ReportData {
	sv := buildSessionView(s)
	tv := buildTranscriptView(tr)
	stats := buildStats(tr, s.CreatedAt, s.UpdatedAt)

	return &ReportData{
		Session:    sv,
		Transcript: tv,
		Stats:      stats,
	}
}

// BuildReportDataFromMessages builds report data from session messages
// (replaces the transcript-based approach). subagents maps a sub-agent
// shortID to its sidecar messages (subagent/<shortID>.jsonl); it may be nil.
func BuildReportDataFromMessages(s *session.Session, msgs []session.Message, subagents map[string][]session.Message) *ReportData {
	sv := buildSessionView(s)
	tv := buildTranscriptViewFromMessages(msgs, subagents)
	stats := buildStatsFromMessages(msgs, subagents, s.CreatedAt, s.UpdatedAt)

	return &ReportData{
		Session:    sv,
		Transcript: tv,
		Stats:      stats,
	}
}

func buildSessionView(s *session.Session) *SessionView {
	return &SessionView{
		ID:        s.ID,
		Title:     s.Title,
		Provider:  s.ProviderName,
		CreatedAt: formatTime(s.CreatedAt),
		UpdatedAt: formatTime(s.UpdatedAt),
		Duration:  formatDuration(s.CreatedAt, s.UpdatedAt),
	}
}

func buildTranscriptView(tr *transcript.Transcript) *TranscriptView {
	tv := &TranscriptView{}
	for _, turn := range tr.Turns {
		tv.Turns = append(tv.Turns, buildTurnView(turn))
	}
	return tv
}

func buildTurnView(turn transcript.Turn) TurnView {
	tv := TurnView{ID: turn.ID}

	// Extract user input from the first user event (if present).
	// Also compute turn duration from first and last event timestamps.
	var firstTS, lastTS time.Time

	for _, ev := range turn.Events {
		eview := buildEventView(ev)

		if ev.Type == transcript.EventUser {
			tv.UserInput = ev.Content
		}

		if firstTS.IsZero() || ev.Timestamp.Before(firstTS) {
			firstTS = ev.Timestamp
		}
		if ev.Timestamp.After(lastTS) {
			lastTS = ev.Timestamp
		}

		tv.Events = append(tv.Events, eview)
	}

	if !firstTS.IsZero() && !lastTS.IsZero() {
		tv.Duration = formatDuration(firstTS, lastTS)
	}

	return tv
}

func buildEventView(ev transcript.Event) EventView {
	eview := EventView{
		Type:      string(ev.Type),
		Timestamp: formatTime(ev.Timestamp),
		Name:      ev.Name,
		Content:   ev.Content,
		ArgsRaw:   ev.Args,
		IsError:   ev.IsError,
	}

	// Format Args as pretty JSON for display.
	if ev.Args != "" {
		eview.ArgsJSON = formatArgsJSON(ev.Args)
	}

	// Set icon and CSS class.
	eview.Icon, eview.CSSClass = eventIconAndClass(ev.Type, ev.Name, ev.IsError)

	// Handle nested children (sub-agents).
	if len(ev.Children) > 0 {
		eview.HasChildren = true
		for _, child := range ev.Children {
			eview.Children = append(eview.Children, buildEventView(child))
		}
	}

	return eview
}

func eventIconAndClass(et transcript.EventType, name string, isError bool) (icon, cssClass string) {
	switch et {
	case transcript.EventUser:
		return "👤", "event-user"
	case transcript.EventThinking:
		return "💭", "event-thinking"
	case transcript.EventText:
		return "💬", "event-text"
	case transcript.EventToolCall:
		if name == "SubAgent" {
			return "🔀", "event-subagent"
		}
		return "🔧", "event-tool-call"
	case transcript.EventToolResult:
		if isError {
			return "❌", "event-tool-result event-error"
		}
		return "📋", "event-tool-result"
	default:
		return "•", ""
	}
}

func buildStats(tr *transcript.Transcript, created, updated time.Time) StatsView {
	stats := StatsView{
		TotalDuration: formatDuration(created, updated),
	}
	freq := map[string]int{}

	for _, turn := range tr.Turns {
		stats.TurnCount++
		countTurnStats(turn, &stats, freq)
	}

	stats.SubAgentCount = tr.SubagentCount()

	// Sort tool frequency descending.
	stats.ToolFreq = sortFreq(freq)

	return stats
}

func countTurnStats(turn transcript.Turn, stats *StatsView, freq map[string]int) {
	for _, ev := range turn.Events {
		countEventStats(ev, stats, freq)
	}
}

func countEventStats(ev transcript.Event, stats *StatsView, freq map[string]int) {
	switch ev.Type {
	case transcript.EventUser:
		stats.UserMsgCount++
	case transcript.EventThinking:
		stats.ThinkingCount++
	case transcript.EventText:
		stats.TextCount++
	case transcript.EventToolCall:
		stats.ToolCallCount++
		freq[ev.Name]++
	case transcript.EventToolResult:
		if ev.IsError {
			stats.ToolErrorCount++
		}
	}
	// Recurse into children.
	for _, child := range ev.Children {
		countEventStats(child, stats, freq)
	}
}

func sortFreq(freq map[string]int) []KV {
	var result []KV
	for k, v := range freq {
		result = append(result, KV{Key: k, Value: v})
	}
	// Sort by value descending.
	slices.SortFunc(result, func(a, b KV) int {
		return b.Value - a.Value
	})
	return result
}

// buildTranscriptViewFromMessages builds a turn view from flat session messages.
// SubAgent tool calls get their sidecar messages attached as children.
func buildTranscriptViewFromMessages(msgs []session.Message, subagents map[string][]session.Message) *TranscriptView {
	tv := &TranscriptView{}
	if len(msgs) == 0 {
		return tv
	}

	// Index SubAgent tool results: toolCallID → subagent shortID. The tool_call
	// precedes its result in the message stream, so this needs a first pass.
	subByCall := make(map[string]string)
	for _, msg := range msgs {
		if msg.Type == session.MessageTypeToolResult && msg.SubagentID != "" {
			subByCall[msg.ToolCallID] = msg.SubagentID
		}
	}

	// Group messages into turns: each user message starts a new turn.
	turnID := 1
	var currentEvents []EventView
	var pendingReminder *EventView // buffered until next user message

	for _, msg := range msgs {
		ev := sessionMessageToEventView(msg)

		// Attach sub-agent execution details to its SubAgent tool call.
		if msg.Type == session.MessageTypeToolCall && msg.Name == "SubAgent" {
			if subID, ok := subByCall[msg.ToolCallID]; ok && len(subagents[subID]) > 0 {
				ev.HasChildren = true
				ev.Children = buildSubagentEventViews(subagents[subID])
			}
		}

		// Buffer reminder to include with the next user message's turn,
		// rather than creating a broken standalone turn.
		if msg.Type == session.MessageTypeReminder {
			e := ev
			pendingReminder = &e
			continue
		}

		// User messages mark new turn boundaries
		if msg.Type == session.MessageTypeUser && len(currentEvents) > 0 {
			tv.Turns = append(tv.Turns, TurnView{ID: turnID, Events: currentEvents})
			turnID++
			currentEvents = nil
		}

		// Insert buffered reminder before the current event
		if pendingReminder != nil {
			currentEvents = append(currentEvents, *pendingReminder)
			pendingReminder = nil
		}

		currentEvents = append(currentEvents, ev)
	}

	// Flush final turn (including any remaining buffered reminder)
	if pendingReminder != nil {
		currentEvents = append(currentEvents, *pendingReminder)
	}
	if len(currentEvents) > 0 {
		tv.Turns = append(tv.Turns, TurnView{ID: turnID, Events: currentEvents})
	}

	return tv
}

// buildSubagentEventViews converts a sub-agent's sidecar messages into child
// event views. Sub-agents cannot spawn their own SubAgent, so no recursion
// is needed here.
func buildSubagentEventViews(msgs []session.Message) []EventView {
	views := make([]EventView, 0, len(msgs))
	for _, msg := range msgs {
		views = append(views, sessionMessageToEventView(msg))
	}
	return views
}

// sessionMessageToEventView converts a session.Message to an EventView.
func sessionMessageToEventView(msg session.Message) EventView {
	ev := EventView{
		Type:      string(msg.Type),
		Timestamp: formatTime(msg.Timestamp),
		Name:      msg.Name,
		Content:   msg.Content,
		IsError:   msg.IsError,
	}

	// Map session message types to transcript event types for display
	switch msg.Type {
	case session.MessageTypeUser:
		ev.Type = "user"
		ev.Content = msg.Content
	case session.MessageTypeAssistant:
		ev.Type = "text"
		ev.Content = msg.Content
	case session.MessageTypeThinking:
		ev.Type = "thinking"
		ev.Content = msg.Content
	case session.MessageTypeToolCall:
		ev.Type = "tool_call"
		ev.ArgsRaw = convertArgsToString(msg.Args)
		ev.ArgsJSON = formatArgsJSON(ev.ArgsRaw)
	case session.MessageTypeToolResult:
		ev.Type = "tool_result"
		ev.Content = msg.Result
	case session.MessageTypeConfirm:
		ev.Type = "confirm"
	case session.MessageTypeReminder:
		ev.Type = "reminder"
	default:
		ev.Type = string(msg.Type)
	}

	ev.Icon, ev.CSSClass = sessionIconAndClass(msg)
	return ev
}

// sessionIconAndClass returns icon and CSS class for a session message type.
func sessionIconAndClass(msg session.Message) (icon, cssClass string) {
	switch msg.Type {
	case session.MessageTypeUser:
		return "👤", "event-user"
	case session.MessageTypeAssistant:
		return "💬", "event-text"
	case session.MessageTypeThinking:
		return "💭", "event-thinking"
	case session.MessageTypeToolCall:
		if msg.Name == "SubAgent" {
			return "🔀", "event-subagent"
		}
		return "🔧", "event-tool-call"
	case session.MessageTypeToolResult:
		if msg.IsError {
			return "❌", "event-tool-result event-error"
		}
		return "📋", "event-tool-result"
	case session.MessageTypeReminder:
		return "ℹ️", "event-reminder"
	default:
		return "•", ""
	}
}

// convertArgsToString converts msg.Args (any) to string.
func convertArgsToString(args any) string {
	if args == nil {
		return ""
	}
	switch v := args.(type) {
	case string:
		return v
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(data)
	}
}

// buildStatsFromMessages builds statistics from session messages. Sub-agent
// sidecar messages are folded into the counts, except their synthetic user
// prompt, which is not a real user turn.
func buildStatsFromMessages(msgs []session.Message, subagents map[string][]session.Message, created, updated time.Time) StatsView {
	stats := StatsView{
		TotalDuration: formatDuration(created, updated),
	}
	freq := map[string]int{}

	countMsg := func(msg session.Message) {
		switch msg.Type {
		case session.MessageTypeUser:
			stats.UserMsgCount++
		case session.MessageTypeThinking:
			stats.ThinkingCount++
		case session.MessageTypeAssistant:
			stats.TextCount++
		case session.MessageTypeToolCall:
			stats.ToolCallCount++
			freq[msg.Name]++
			if msg.Name == "SubAgent" {
				stats.SubAgentCount++
			}
		case session.MessageTypeToolResult:
			if msg.IsError {
				stats.ToolErrorCount++
			}
		}
	}

	for _, msg := range msgs {
		countMsg(msg)
	}
	for _, subMsgs := range subagents {
		for _, msg := range subMsgs {
			if msg.Type == session.MessageTypeUser {
				continue // sub-agent task prompt, not a real user message
			}
			countMsg(msg)
		}
	}

	stats.TurnCount = countTurnsFromMessages(msgs)
	stats.ToolFreq = sortFreq(freq)
	return stats
}

// countTurnsFromMessages counts turns by user message boundaries.
func countTurnsFromMessages(msgs []session.Message) int {
	turns := 0
	for _, msg := range msgs {
		if msg.Type == session.MessageTypeUser {
			turns++
		}
	}
	return turns
}

func formatArgsJSON(raw string) string {
	if raw == "" {
		return ""
	}
	// Try to pretty-print JSON.
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return raw // Not valid JSON, return as-is.
	}
	pretty, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return raw
	}
	return string(pretty)
}

// GenerateHTML produces a self-contained HTML report and writes it to w.
// The caller should provide an io.Writer (typically *os.File).
func GenerateHTML(data *ReportData) (string, error) {
	tmpl, err := template.New("report").Funcs(template.FuncMap{
		"json": func(v any) template.JS {
			b, _ := json.Marshal(v)
			return template.JS(b)
		},
		"truncate": strutil.TruncateFitted,
	}).Parse(reportTemplate)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}

	var buf strings.Builder
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute template: %w", err)
	}
	return buf.String(), nil
}

// OpenInBrowser saves the HTML to a temp file and opens it in the default browser.
// Returns the path to the temp file (caller may delete after use).
func OpenInBrowser(html string, sessionID string) (string, error) {
	dir := os.TempDir()
	filename := filepath.Join(dir, fmt.Sprintf("tachi-transcript-%s.html", sessionID[:8]))

	if err := os.WriteFile(filename, []byte(html), 0644); err != nil {
		return "", fmt.Errorf("write temp file: %w", err)
	}

	if err := openBrowser(filename); err != nil {
		return filename, fmt.Errorf("open browser: %w", err)
	}

	return filename, nil
}

func openBrowser(url string) error {
	var cmd string
	var args []string

	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
		args = []string{url}
	case "linux":
		cmd = "xdg-open"
		args = []string{url}
	case "windows":
		cmd = "cmd"
		args = []string{"/c", "start", url}
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}

	return exec.Command(cmd, args...).Start()
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02 15:04:05")
}

func formatDuration(start, end time.Time) string {
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return ""
	}
	d := end.Sub(start)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
}
