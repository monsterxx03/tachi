package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent/transcript/render"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// ── helpers ────────────────────────────────────────────────────────────────

// sessionOneOffDir returns <sessions>/<id>/oneoff.
func (s *Server) sessionOneOffDir(id string) string {
	sessDir, err := config.SessionDir()
	if err != nil {
		return ""
	}
	return filepath.Join(sessDir, id, "oneoff")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// readSession loads a session by the {id} path value; 404 when missing.
func (s *Server) readSession(w http.ResponseWriter, id string) *session.Session {
	sess, err := s.Store.LoadMeta(id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeJSONError(w, http.StatusNotFound, "session not found")
			return nil
		}
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("load session: %v", err))
		return nil
	}
	return sess
}

// sessionCreatedFromID extracts the creation date from a time-prefixed
// session ID ("YYYY-MM-DD-HHMMSS-uuid"); zero time when unparseable.
// The usage ledger is sharded per day, and a session's rows can only exist
// from its creation day onward — so this date is a safe lower bound for
// ledger scans of that session.
func sessionCreatedFromID(id string) time.Time {
	if len(id) < 10 {
		return time.Time{}
	}
	t, err := time.Parse("2006-01-02", id[:10])
	if err != nil {
		return time.Time{}
	}
	return t
}

// sessionUsage aggregates the ledger rows belonging to one session,
// scanning only day files from the session's creation date onward.
// Used by single-session endpoints (detail view); the list endpoint uses
// sessionUsageBySession instead so it scans the ledger exactly once.
func (s *Server) sessionUsage(id string) *usageSummary {
	rows, err := s.Usage.Rows(id, sessionCreatedFromID(id))
	if err != nil {
		return &usageSummary{}
	}
	return summarizeUsage(rows)
}

// sessionRequestCosts returns the precise per-call cost for each API request,
// aligned to the order of apiReqs (chronological). The ledger's
// kind=conversation rows are the main-loop LLM calls — the same ones
// recorded as api_requests — and both are appended in call order, so pairing
// the conversation rows (sorted by TS) with apiReqs (already in file order)
// 1:1 yields the matching cost per request. Other kinds (title, subagent,
// review, ...) don't appear in api_requests and are filtered out. On any
// mismatch (e.g. legacy data) we return costs in order up to the shorter
// length; extras simply have no cost.
func (s *Server) sessionRequestCosts(id string, apiReqs []session.APIRequest) []float64 {
	rows, err := s.Usage.Rows(id, sessionCreatedFromID(id))
	if err != nil {
		return nil
	}
	// Keep only main-loop conversation calls, in chronological order —
	// ledger files stream oldest-first, but sort defensively anyway.
	conversation := make([]llm.UsageRow, 0, len(rows))
	for i := range rows {
		if rows[i].Kind == llm.UsageKindConversation {
			conversation = append(conversation, rows[i])
		}
	}
	slices.SortFunc(conversation, func(a, b llm.UsageRow) int { return a.TS.Compare(b.TS) })

	costs := make([]float64, len(apiReqs))
	for i := range costs {
		if i < len(conversation) {
			costs[i] = conversation[i].Cost()
		}
	}
	return costs
}

// sessionUsageBySession aggregates the ledger in one pass, grouped by
// session_id, scanning only day files from the OLDEST session's creation
// date onward (no session can have rows before it exists). The list
// endpoint needs per-session cost/calls for every row it returns — calling
// sessionUsage(id) per session would re-scan the day files once per session
// (O(sessions × rows)); grouping one scan is O(rows). Session-less rows
// (global oneoffs) are skipped.
func (s *Server) sessionUsageBySession(sessions []*session.Session) map[string]*usageSummary {
	from := time.Time{}
	for _, sess := range sessions {
		if c := sessionCreatedFromID(sess.ID); !c.IsZero() && (from.IsZero() || c.Before(from)) {
			from = c
		}
	}
	rows, err := s.Usage.RowsAll(from)
	if err != nil {
		return map[string]*usageSummary{}
	}
	groups := map[string][]llm.UsageRow{}
	for i := range rows {
		if rows[i].SessionID != "" {
			groups[rows[i].SessionID] = append(groups[rows[i].SessionID], rows[i])
		}
	}
	out := make(map[string]*usageSummary, len(groups))
	for id, group := range groups {
		out[id] = summarizeUsage(group)
	}
	return out
}

// ── GET /api/sessions ──────────────────────────────────────────────────────

// sessionsListItem is the compact row shown in the session lists.
type sessionsListItem struct {
	ID             string    `json:"id"`
	Title          string    `json:"title"`
	Provider       string    `json:"provider,omitempty"`
	Mode           string    `json:"mode,omitempty"`
	WorkingDir     string    `json:"working_dir,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	MessageCount   int       `json:"message_count"`
	ToolCalls      int       `json:"tool_calls"`
	OneOffCount    int       `json:"oneoff_count"`
	CompactedChild string    `json:"compacted_child_id,omitempty"`
	Cost           float64   `json:"cost"`
	Calls          int       `json:"calls"`
}

// listSessionsPageSize is the default page size for GET /api/sessions;
// callers pass ?limit= to override (capped at listSessionsPageSizeMax).
const (
	listSessionsPageSize    = 50
	listSessionsPageSizeMax = 200
)

// handleListSessions returns sessions sorted by creation time descending —
// session IDs embed their creation timestamp (YYYY-MM-DD-HHMMSS-uuid), so
// lexicographic ID order IS chronological order and the last returned ID
// works as a stable keyset cursor.
//
// Query params:
//   - limit: page size (default 50, max 200). Only the sessions on the
//     current page get their per-session stats computed (streaming message
//     count, oneoff glob, usage lookup) — lightweight callers (e.g. the
//     overview page's "recent sessions") pass a small limit instead of
//     paying for every session on disk.
//   - cursor: the ID of the last session of the previous page (exclusive);
//     omit for the first page.
//
// Response: {"sessions": [...], "total": N, "next_cursor": "<id>"} —
// next_cursor is empty when there are no more pages.
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	limit := listSessionsPageSize
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = min(n, listSessionsPageSizeMax)
		}
	}
	cursor := r.URL.Query().Get("cursor")

	sessions, err := s.Store.ListSessions()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("list sessions: %v", err))
		return
	}
	// Creation time descending = ID descending (IDs are time-prefixed).
	slices.SortFunc(sessions, func(a, b *session.Session) int {
		return strings.Compare(b.ID, a.ID)
	})

	start := 0
	if cursor != "" {
		for i, sess := range sessions {
			if sess.ID == cursor {
				start = i + 1
				break
			}
		}
	}
	end := min(start+limit, len(sessions))

	nextCursor := ""
	if end < len(sessions) && end > 0 {
		nextCursor = sessions[end-1].ID
	}

	// One ledger scan for the whole page, grouped per session (see
	// sessionUsageBySession) — never one full scan per session. The scan
	// starts at the oldest session on this page's creation date, so older
	// ledger day files (before any listed session existed) are skipped.
	usageBySession := s.sessionUsageBySession(sessions[start:end])
	writeJSON(w, http.StatusOK, map[string]any{
		"sessions":    s.buildSessionsListItems(sessions[start:end], usageBySession),
		"total":       len(sessions),
		"next_cursor": nextCursor,
	})
}

// buildSessionsListItems computes the compact list row for each session:
// message/tool counts from a streaming scan (never a full unmarshal), oneoff
// count from the dir glob, cost from the usageBySession map (precomputed in
// one ledger pass by the caller). Only called for the sessions on the
// current page.
func (s *Server) buildSessionsListItems(sessions []*session.Session, usageBySession map[string]*usageSummary) []sessionsListItem {
	items := make([]sessionsListItem, 0, len(sessions))
	for _, sess := range sessions {
		msgCount, toolCalls := s.Store.CountMessages(sess.ID)
		oneoffCount := 0
		if names, err := filepath.Glob(filepath.Join(s.sessionOneOffDir(sess.ID), "*.jsonl")); err == nil {
			oneoffCount = len(names)
		}
		u := usageBySession[sess.ID]
		if u == nil {
			u = &usageSummary{}
		}
		items = append(items, sessionsListItem{
			ID:             sess.ID,
			Title:          sess.Title,
			Provider:       sess.ProviderName,
			Mode:           sess.Mode,
			WorkingDir:     sess.WorkingDir,
			CreatedAt:      sess.CreatedAt,
			UpdatedAt:      sess.UpdatedAt,
			MessageCount:   msgCount,
			ToolCalls:      toolCalls,
			OneOffCount:    oneoffCount,
			CompactedChild: sess.CompactedChildID,
			Cost:           u.TotalCost,
			Calls:          u.TotalCalls,
		})
	}
	return items
}

// ── GET /api/sessions/{id} ─────────────────────────────────────────────────

type oneOffSummary struct {
	File         string            `json:"file"`
	Size         int64             `json:"size"`
	Kind         string            `json:"kind,omitempty"`
	Provider     string            `json:"provider,omitempty"`
	Model        string            `json:"model,omitempty"`
	StartedAt    *time.Time        `json:"started_at,omitempty"`
	SystemPrompt string            `json:"system_prompt,omitempty"`
	Extra        map[string]string `json:"extra,omitempty"`
	EventCount   int               `json:"event_count"`
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess := s.readSession(w, id)
	if sess == nil {
		return
	}

	msgs, err := s.Store.LoadMessages(id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("load messages: %v", err))
		return
	}
	apiReqs, _ := s.Store.LoadAPIRequests(id)
	subagents, _ := session.LoadSubagentMessages(id)
	oneoffs := s.scanOneOffDir(s.sessionOneOffDir(id))
	// Array fields must never serialize as null: the frontend calls
	// .map()/.length on them unconditionally. Nil slices become "[]".
	if msgs == nil {
		msgs = []session.Message{}
	}
	if apiReqs == nil {
		apiReqs = []session.APIRequest{}
	}
	if subagents == nil {
		subagents = map[string][]session.Message{}
	}
	// Attach the precise per-call cost (from the usage ledger's
	// conversation-kind rows, paired 1:1 in call order) to each request so
	// the inspector shows real numbers instead of estimating. The recorder
	// never sets Cost, so this is a display-only enrichment of this response.
	if costs := s.sessionRequestCosts(id, apiReqs); costs != nil {
		for i := range apiReqs {
			if i < len(costs) {
				apiReqs[i].Cost = costs[i]
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"meta":         sess,
		"messages":     msgs,
		"api_requests": apiReqs,
		"subagents":    subagents,
		"oneoffs":      oneoffs,
		"usage":        s.sessionUsage(id),
	})
}

// ── oneoff transcript listing ──────────────────────────────────────────────

// oneoffMetaLine is the first line of a one-off transcript file.
// Mirrors agent.oneoffMetaLine (kept private there) so the web layer can
// surface the header without depending on agent internals.
type oneoffMetaLine struct {
	Type         string            `json:"type"`
	Kind         string            `json:"kind,omitempty"`
	SessionID    string            `json:"session_id,omitempty"`
	CWD          string            `json:"cwd,omitempty"`
	Provider     string            `json:"provider,omitempty"`
	Model        string            `json:"model,omitempty"`
	StartedAt    time.Time         `json:"started_at"`
	SystemPrompt string            `json:"system_prompt,omitempty"`
	Extra        map[string]string `json:"extra,omitempty"`
}

// scanOneOffDir lists *.jsonl files under dir with their meta header.
// Always returns a non-nil slice (JSON "[]"), never null — the frontend
// types these fields as arrays and calls .map()/.length on them.
func (s *Server) scanOneOffDir(dir string) []oneOffSummary {
	out := make([]oneOffSummary, 0)
	names, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil || len(names) == 0 {
		return out
	}
	sort.Strings(names)
	for _, p := range names {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		sum := oneOffSummary{
			File: filepath.Base(p),
			Size: info.Size(),
		}
		hasMeta := false
		if meta, ok := readOneOffMeta(p); ok {
			hasMeta = true
			sum.Kind = meta.Kind
			sum.Provider = meta.Provider
			sum.Model = meta.Model
			sum.SystemPrompt = meta.SystemPrompt
			sum.Extra = meta.Extra
			t := meta.StartedAt
			sum.StartedAt = &t
		}
		// Count non-meta lines as events: every newline-delimited line is
		// one event, minus the meta header when one is present.
		data, _ := os.ReadFile(p)
		if len(data) > 0 {
			sum.EventCount = strings.Count(string(data), "\n")
			if hasMeta {
				sum.EventCount--
			}
		}
		out = append(out, sum)
	}
	return out
}

// readOneOffMeta parses the first "meta" line of a one-off transcript.
func readOneOffMeta(path string) (oneoffMetaLine, bool) {
	f, err := os.Open(path)
	if err != nil {
		return oneoffMetaLine{}, false
	}
	defer f.Close()

	var meta oneoffMetaLine
	dec := json.NewDecoder(f)
	if err := dec.Decode(&meta); err != nil {
		return oneoffMetaLine{}, false
	}
	if meta.Type != "meta" {
		return oneoffMetaLine{}, false
	}
	return meta, true
}

// GET /api/sessions/{id}/oneoff
func (s *Server) handleListSessionOneOffs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.readSession(w, id) == nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"oneoffs": s.scanOneOffDir(s.sessionOneOffDir(id)),
	})
}

// GET /api/sessions/{id}/oneoff/{file}
func (s *Server) handleGetOneOff(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	file := r.PathValue("file")
	if strings.Contains(file, "/") || strings.Contains(file, "..") {
		writeJSONError(w, http.StatusBadRequest, "invalid file name")
		return
	}
	if s.readSession(w, id) == nil {
		return
	}
	dir := s.sessionOneOffDir(id)
	data, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeJSONError(w, http.StatusNotFound, "oneoff file not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("read oneoff: %v", err))
		return
	}

	// Return the JSONL as an array of raw lines so the client can render
	// meta / messages / api_request lines uniformly.
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	events := make([]json.RawMessage, 0, len(lines))
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		events = append(events, json.RawMessage(ln))
	}
	writeJSON(w, http.StatusOK, map[string]any{"file": file, "events": events})
}

// GET /api/oneoff — global one-off transcripts grouped by kind.
func (s *Server) handleListGlobalOneOffs(w http.ResponseWriter, r *http.Request) {
	root := config.OneoffDir()
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusOK, map[string]any{"kinds": map[string]any{}})
			return
		}
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("read oneoff dir: %v", err))
		return
	}
	kinds := map[string]any{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		kinds[e.Name()] = s.scanOneOffDir(filepath.Join(root, e.Name()))
	}
	writeJSON(w, http.StatusOK, map[string]any{"kinds": kinds})
}

// ── GET /api/usage ─────────────────────────────────────────────────────────

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Usage.RowsAll(time.Time{})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("read usage ledger: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, summarizeUsage(rows))
}

// usageSummary aggregates ledger rows: totals + per-day + per-kind + per-model.
type usageSummary struct {
	TotalCost  float64                `json:"total_cost"`
	TotalCalls int                    `json:"total_calls"`
	Days       []usageDayStat         `json:"days"` // newest first
	ByKind     map[string]usageAmount `json:"by_kind"`
	ByModel    map[string]usageAmount `json:"by_model"`
}

type usageDayStat struct {
	Date      string  `json:"date"`
	Cost      float64 `json:"cost"`
	Calls     int     `json:"calls"`
	Unpriced  int     `json:"unpriced"`
	Input     int64   `json:"input"`
	Output    int64   `json:"output"`
	CacheRead int64   `json:"cache"`
}

type usageAmount struct {
	Cost  float64 `json:"cost"`
	Calls int     `json:"calls"`
}

func summarizeUsage(rows []llm.UsageRow) *usageSummary {
	sum := &usageSummary{
		ByKind:  map[string]usageAmount{},
		ByModel: map[string]usageAmount{},
	}
	dayMap := map[string]*usageDayStat{}
	for i := range rows {
		row := &rows[i]
		cost := row.Cost()
		sum.TotalCost += cost
		sum.TotalCalls++

		date := row.TS.Format("2006-01-02")
		day := dayMap[date]
		if day == nil {
			day = &usageDayStat{Date: date}
			dayMap[date] = day
		}
		day.Cost += cost
		day.Calls++
		day.Input += row.InputTokens
		day.Output += row.OutputTokens
		day.CacheRead += row.CacheReadInputTokens
		if row.Unpriced() {
			day.Unpriced++
		}

		kind := string(row.Kind)
		if kind == "" {
			kind = "unknown"
		}
		k := sum.ByKind[kind]
		k.Cost += cost
		k.Calls++
		sum.ByKind[kind] = k

		// Model is the finer-grained identity (provider may be empty).
		m := sum.ByModel[row.Model]
		m.Cost += cost
		m.Calls++
		sum.ByModel[row.Model] = m
	}
	for _, d := range dayMap {
		sum.Days = append(sum.Days, *d)
	}
	slices.SortFunc(sum.Days, func(a, b usageDayStat) int {
		return strings.Compare(b.Date, a.Date)
	})
	return sum
}

// ── GET /api/sessions/{id}/transcript ──────────────────────────────────────

// handleTranscript renders the session as the same HTML report that
// `tachi transcript show` produces, for in-browser export.
func (s *Server) handleTranscript(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess := s.readSession(w, id)
	if sess == nil {
		return
	}
	msgs, err := s.Store.LoadMessages(id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("load messages: %v", err))
		return
	}
	subagents, _ := session.LoadSubagentMessages(id)
	apiReqs, _ := s.Store.LoadAPIRequests(id)

	data := render.BuildReportDataFromMessagesWithRequests(sess, msgs, subagents, apiReqs)
	html, err := render.GenerateHTML(data)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("generate transcript: %v", err))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=tachi-transcript-%s.html", sess.ID[:8]))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(html))
}
