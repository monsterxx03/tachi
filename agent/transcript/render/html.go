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
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent/transcript"
	"github.com/monsterxx03/tachi/pkg/fileutil"
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
	Events    []EventView `json:"events"` // full event stream (flat)
	UserInput string      `json:"user_input,omitempty"`
	Duration  string      `json:"duration,omitempty"`

	// APIRequests lists the LLM API calls of this turn, indexed by iteration.
	// RequestGroup entries reference them to render per-request groups.
	APIRequests []APIRequestView `json:"api_requests,omitempty"`
	// Items is the render structure: flat events (user/reminder/steer) and
	// per-request groups in original order.
	Items []TurnItem `json:"items,omitempty"`
	// HasRequestGroups reports whether Items contains request groups. Legacy
	// sessions (messages without iteration markers) have requests but no
	// groups; the report falls back to a flat request list for them.
	HasRequestGroups bool `json:"has_request_groups,omitempty"`
}

// TurnItem is a single render element of a turn: either a flat event
// (iteration-0: user / reminder / steer) or a request group (all events of
// one API call, with its system prompt and tools).
type TurnItem struct {
	Kind  string            `json:"kind"` // "event" | "group"
	Event *EventView        `json:"event,omitempty"`
	Group *RequestGroupView `json:"group,omitempty"`
}

// RequestGroupView groups every event produced by one API call (iteration):
// the request's system prompt + tools, plus its thinking/text/tool events.
type RequestGroupView struct {
	Iteration        int        `json:"iteration"`
	UserPrompt       string     `json:"user_prompt,omitempty"` // the user input this request answered
	SystemPrompt     string     `json:"system_prompt,omitempty"`
	SystemPromptSame bool       `json:"system_prompt_same,omitempty"`
	Tools            []ToolView `json:"tools,omitempty"`
	ToolsSame        bool       `json:"tools_same,omitempty"`
	AllSame          bool       `json:"all_same,omitempty"` // identical to previous request
	Model            string     `json:"model,omitempty"`    // model this request was sent to
	Provider         string     `json:"provider,omitempty"` // config provider name
	Thinking         string     `json:"thinking,omitempty"` // "none" | effort | "" = default
	// DurationMs is this request's wall-clock call duration; Duration the
	// preformatted display string. Enriched from the request's APIRequestView.
	DurationMs int64       `json:"duration_ms,omitempty"`
	Duration   string      `json:"duration,omitempty"`
	Events     []EventView `json:"events,omitempty"` // thinking/text/tool_call/tool_result of this request
}

// APIRequestView is a display-friendly API request. *Same flags mark content
// identical to the PREVIOUS request (in session order), so the report can
// collapse redundant iterations.
type APIRequestView struct {
	Iteration        int        `json:"iteration"` // 1-based API call number (session-wide)
	UserPrompt       string     `json:"user_prompt,omitempty"`
	SystemPrompt     string     `json:"system_prompt,omitempty"`
	SystemPromptSame bool       `json:"system_prompt_same,omitempty"`
	Tools            []ToolView `json:"tools,omitempty"`
	ToolsSame        bool       `json:"tools_same,omitempty"`
	AllSame          bool       `json:"all_same,omitempty"` // identical to previous request
	Model            string     `json:"model,omitempty"`    // model this request was sent to
	Provider         string     `json:"provider,omitempty"` // config provider name
	Thinking         string     `json:"thinking,omitempty"` // "none" | effort | "" = default
	// DurationMs is this API call's wall-clock duration; Duration the
	// preformatted display string ("850ms", "1.2s", ...).
	DurationMs int64  `json:"duration_ms,omitempty"`
	Duration   string `json:"duration,omitempty"`
}

// ToolView is a display-friendly tool definition from an API request.
type ToolView struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Schema      string `json:"schema,omitempty"` // pretty-printed JSON schema
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

	// Iteration marks the API call that produced a thinking/text/tool_call/
	// tool_result event, linking it to the request group that emitted it.
	// 0 = not request-bound (user / reminder / steer).
	Iteration int `json:"iteration,omitempty"`

	// DurationMs is the wall-clock execution duration, displayed-friendly.
	// Set on tool_result events (tool execution). Duration is the preformatted
	// string shown in the report ("850ms", "1.2s", ...).
	DurationMs int64  `json:"duration_ms,omitempty"`
	Duration   string `json:"duration,omitempty"`
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
	APICallCount   int    `json:"api_call_count"` // recorded LLM API requests
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
// No API request records are attached (see BuildReportDataFromMessagesWithRequests).
func BuildReportDataFromMessages(s *session.Session, msgs []session.Message, subagents map[string][]session.Message) *ReportData {
	return BuildReportDataFromMessagesWithRequests(s, msgs, subagents, nil)
}

// BuildReportDataFromMessagesWithRequests builds report data from session
// messages plus the recorded API request payloads (system prompt + tool
// schemas). apiReqs may be nil — turns then render without the system/tools
// sections. apiReqs are attributed to turns by timestamp (each turn shows
// its first request).
func BuildReportDataFromMessagesWithRequests(s *session.Session, msgs []session.Message, subagents map[string][]session.Message, apiReqs []session.APIRequest) *ReportData {
	sv := buildSessionView(s)
	tv := buildTranscriptViewFromMessages(msgs, subagents, apiReqs)
	stats := buildStatsFromMessages(msgs, subagents, s.CreatedAt, s.UpdatedAt)
	stats.APICallCount = len(apiReqs)

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
// apiReqs (may be nil) are attributed to user events by timestamp — each user
// message carries the API requests it triggered (system prompt + tools).
func buildTranscriptViewFromMessages(msgs []session.Message, subagents map[string][]session.Message, apiReqs []session.APIRequest) *TranscriptView {
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
	var currentTimes []time.Time // event timestamps, aligned with currentEvents
	var turnStarts []time.Time   // first event timestamp per turn
	var turnTimes [][]time.Time  // per-turn event timestamps (for request attribution)
	turnStarted := false         // whether the current turn already has a recorded start
	type pendingReminder struct {
		ev EventView
		ts time.Time
	}
	var reminder *pendingReminder // buffered until the next non-reminder event

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
		// rather than creating a broken standalone turn. A leading reminder
		// still marks the turn's start (real turns open with <system-reminder>).
		if msg.Type == session.MessageTypeReminder {
			if !turnStarted {
				turnStarts = append(turnStarts, msg.Timestamp)
				turnStarted = true
			}
			reminder = &pendingReminder{ev: ev, ts: msg.Timestamp}
			continue
		}

		// User messages mark new turn boundaries
		if msg.Type == session.MessageTypeUser && len(currentEvents) > 0 {
			tv.Turns = append(tv.Turns, TurnView{ID: turnID, Events: currentEvents})
			turnTimes = append(turnTimes, currentTimes)
			turnID++
			currentEvents = nil
			currentTimes = nil
			turnStarted = false
		}

		// Insert buffered reminder before the current event
		if reminder != nil {
			currentEvents = append(currentEvents, reminder.ev)
			currentTimes = append(currentTimes, reminder.ts)
			reminder = nil
		}

		// Record this turn's start timestamp on its first event. A buffered
		// reminder may already have filled currentEvents, so track the start
		// with an explicit flag rather than a slice-length check.
		if !turnStarted {
			turnStarts = append(turnStarts, msg.Timestamp)
			turnStarted = true
		}

		currentEvents = append(currentEvents, ev)
		currentTimes = append(currentTimes, msg.Timestamp)
	}

	// Flush final turn (including any remaining buffered reminder).
	// Its start timestamp was already recorded when its first event arrived
	// in the loop; only a pure-reminder turn (no other events) needs a
	// fallback here.
	if len(currentEvents) > 0 {
		tv.Turns = append(tv.Turns, TurnView{ID: turnID, Events: currentEvents})
		turnTimes = append(turnTimes, currentTimes)
		if len(turnStarts) < len(tv.Turns) {
			turnStarts = append(turnStarts, msgs[len(msgs)-1].Timestamp)
		}
	}

	assignAPIRequests(tv.Turns, turnStarts, turnTimes, apiReqs)
	for i := range tv.Turns {
		tv.Turns[i].Items = buildTurnItems(&tv.Turns[i])
		for _, item := range tv.Turns[i].Items {
			if item.Kind == "group" {
				tv.Turns[i].HasRequestGroups = true
				break
			}
		}
	}

	return tv
}

// assignAPIRequests attributes API request records to their turn by
// timestamp and stores them on the turn's APIRequests (indexed by iteration,
// session-wide). *Same flags compare each request against the PREVIOUS
// recorded request in session order.
func assignAPIRequests(turns []TurnView, turnStarts []time.Time, turnTimes [][]time.Time, reqs []session.APIRequest) {
	if len(reqs) == 0 || len(turns) == 0 || len(turnStarts) != len(turns) || len(turnTimes) != len(turns) {
		return
	}

	var prevReq *APIRequestView

	for _, req := range reqs {
		// Latest turn start ≤ req.Timestamp.
		turnIdx := sort.Search(len(turnStarts), func(i int) bool {
			return turnStarts[i].After(req.Timestamp)
		}) - 1
		if turnIdx < 0 {
			continue
		}

		view := APIRequestView{
			Iteration:    req.Iteration,
			UserPrompt:   req.UserPrompt,
			SystemPrompt: req.SystemPrompt,
			Model:        req.Model,
			Provider:     req.Provider,
			Thinking:     req.Thinking,
			DurationMs:   req.DurationMs,
			Duration:     strutil.FormatMs(req.DurationMs),
			Tools:        buildToolViews(req.Tools),
		}
		if prevReq != nil {
			view.SystemPromptSame = view.SystemPrompt == prevReq.SystemPrompt
			view.ToolsSame = reflect.DeepEqual(view.Tools, prevReq.Tools)
			view.AllSame = view.SystemPromptSame && view.ToolsSame
		}
		turns[turnIdx].APIRequests = append(turns[turnIdx].APIRequests, view)
		prevReq = &turns[turnIdx].APIRequests[len(turns[turnIdx].APIRequests)-1]
	}
}

// buildTurnItems assembles the render structure of a turn: events with
// iteration 0 (user / reminder / steer) stay flat; every other event is
// grouped under the API request (iteration) that produced it. Groups are
// emitted at the position of their first event, so the original event order
// is preserved. Each group carries its request's system prompt + tools from
// turn.APIRequests.
func buildTurnItems(turn *TurnView) []TurnItem {
	if len(turn.Events) == 0 {
		return nil
	}

	// Index requests by iteration for group enrichment.
	reqByIter := make(map[int]*APIRequestView, len(turn.APIRequests))
	for i := range turn.APIRequests {
		reqByIter[turn.APIRequests[i].Iteration] = &turn.APIRequests[i]
	}

	var items []TurnItem
	var cur *RequestGroupView

	for i := range turn.Events {
		ev := &turn.Events[i]
		if ev.Iteration <= 0 {
			// Flat event (user / reminder / steer).
			cur = nil
			items = append(items, TurnItem{Kind: "event", Event: ev})
			continue
		}
		// Request-bound event: open a new group when the iteration changes.
		if cur == nil || cur.Iteration != ev.Iteration {
			g := &RequestGroupView{Iteration: ev.Iteration}
			if req, ok := reqByIter[ev.Iteration]; ok {
				g.UserPrompt = req.UserPrompt
				g.SystemPrompt = req.SystemPrompt
				g.SystemPromptSame = req.SystemPromptSame
				g.Tools = req.Tools
				g.ToolsSame = req.ToolsSame
				g.AllSame = req.AllSame
				g.Model = req.Model
				g.Provider = req.Provider
				g.Thinking = req.Thinking
				g.DurationMs = req.DurationMs
				g.Duration = req.Duration
			}
			cur = g
			items = append(items, TurnItem{Kind: "group", Group: g})
		}
		cur.Events = append(cur.Events, *ev)
	}

	return items
}

// buildToolViews converts stored API tool definitions into display views,
// pretty-printing the schema JSON.
func buildToolViews(tools []session.APITool) []ToolView {
	if len(tools) == 0 {
		return nil
	}
	out := make([]ToolView, 0, len(tools))
	for _, t := range tools {
		out = append(out, ToolView{
			Name:        t.Name,
			Description: t.Description,
			Schema:      formatArgsJSON(string(t.Parameters)),
		})
	}
	return out
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
		Iteration: msg.Iteration,
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

	// Tool execution duration (present on tool_result messages).
	if msg.DurationMs > 0 {
		ev.DurationMs = msg.DurationMs
		ev.Duration = strutil.FormatMs(msg.DurationMs)
	}

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
	pretty, err := fileutil.MarshalJSON(v)
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

	if err := fileutil.WriteFileShared(filename, []byte(html)); err != nil {
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
