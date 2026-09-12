package main

// Side-channel (one-off) run records: turning <SessionDir>/<sid>/oneoff/*.jsonl into the
// payload the side panel renders.
//
// The recorder writes session.Message values verbatim (agent/oneoff_recorder.go), so a
// record line's "type" IS one of session.MessageType* — which is what lets this file hand
// raw messages to buildSessionMessages and the frontend render them with buildTurns: the
// panel and the transcript are one renderer over one shape, and cannot drift apart. Only
// two line types are unique to a record: the "meta" header and the "api_request" payload.
//
// Design: docs/2026-09-12-desktop-oneoff-panel-design.md §6/§7.1

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/session"
)

const (
	// oneOffLineMeta / oneOffLineAPIRequest are the two line types only a one-off record
	// has (a session's messages.jsonl has no request payload and no header).
	oneOffLineMeta       = "meta"
	oneOffLineAPIRequest = "api_request"

	// maxOneOffLineBytes is the per-line ceiling for the scanner. bufio.Scanner defaults
	// to 64KB, which a long thinking block alone can exceed (the longest line in a real
	// record measured 23KB); the headroom is there so one large tool result can never
	// truncate a record silently.
	maxOneOffLineBytes = 8 << 20

	// oneOffRunningWindow decides the "in flight" badge from the file's mtime. A record
	// has no end marker — the run appends to it as it goes — so "written to seconds ago"
	// is the honest approximation available without a lock or a side channel.
	oneOffRunningWindow = 15 * time.Second

	// oneOffRoundPrefix is the kind multi-round reviews use ("review-round-2"); the
	// orchestrator builds it from spec.Kind (agent/commands/review.go), the same string
	// the usage ledger records.
	oneOffRoundPrefix = "review-round-"
)

// OneOffListVO is the panel's list payload.
type OneOffListVO struct {
	// Items are this session's side-channel runs, newest first.
	Items []OneOffVO `json:"items"`
	// Note says in the user's words why the list is empty: no such session, no record
	// directory, or nothing recorded yet. "Nothing here" and "could not look" are
	// different facts, and a silent empty list tells the reader neither.
	Note string `json:"note,omitempty"`
}

// OneOffVO is one run (one record file) as the list and the panel header show it.
type OneOffVO struct {
	// Name is the record file name — the list's key and what LoadOneOff takes.
	Name string `json:"name"`
	// Run groups the records of one batch: a multi-round review writes one file per
	// round, all sharing the orchestrator-owned report directory, which is therefore the
	// only reliable key (the file name's second-precision timestamp can straddle a
	// second). Falls back to Name when the record carries no report.
	Run string `json:"run"`
	// Kind is the raw recorded kind: review / commit / review-round-N.
	Kind string `json:"kind"`
	// Round is the round number inside a multi-round review (0 = single-round).
	Round     int    `json:"round,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
	// StartedAt is the meta header's start time (RFC3339, "" when unrecorded).
	StartedAt string `json:"startedAt,omitempty"`
	// UpdatedAt is the record file's last write — for a finished run, effectively its end.
	UpdatedAt string `json:"updatedAt,omitempty"`
	// Running is the mtime heuristic: the file was written to within
	// oneOffRunningWindow.
	Running bool `json:"running,omitempty"`
	// Report is where the run's human-readable report landed (agent.OneOffKeyReport).
	Report string `json:"report,omitempty"`
	// ReviewedMsg is the conversation message this run was started for (a review's entry
	// records it), which is what lets a turn's chip say 已评审 after a restart.
	ReviewedMsg string `json:"reviewedMsg,omitempty"`
	// Paths is the file set a scoped review was limited to (nil for a whole-tree review).
	// The diff pane needs it to show anything, and it is a property of the turn that asked,
	// so the record is where it has to live.
	Paths []string `json:"paths,omitempty"`
	// Findings counts the run's ReportFinding calls. Only LoadOneOff fills it: counting
	// means reading the whole file, and paying that for every record in the list is not
	// worth it (the panel shows the count once a run is selected).
	Findings int `json:"findings,omitempty"`
}

// OneOffRequestVO is one LLM call inside a run. The summary (LoadOneOff) carries the
// metadata and the tool NAMES; the full version (LoadOneOffRequest) adds the prompts.
//
// The prompts are the reason this is separate: a record averages ~16KB of request payload
// per call (the system prompt and tool schemas are resent every iteration), which is over
// half the file. The panel should not pay for it to open a run, and a reader who asks
// "what was the reviewer actually told" should get it on demand.
type OneOffRequestVO struct {
	Seq          int      `json:"seq"`
	Iteration    int      `json:"iteration,omitempty"`
	Timestamp    string   `json:"timestamp,omitempty"`
	Provider     string   `json:"provider,omitempty"`
	Model        string   `json:"model,omitempty"`
	Thinking     string   `json:"thinking,omitempty"`
	DurationMs   int64    `json:"durationMs,omitempty"`
	Tools        []string `json:"tools,omitempty"`
	SystemPrompt string   `json:"systemPrompt,omitempty"`
	UserPrompt   string   `json:"userPrompt,omitempty"`
	// Notice explains a failure of the lazy fetch (the summaries never carry one).
	Notice string `json:"notice,omitempty"`
}

// OneOffDetailVO is one run's replayable content.
type OneOffDetailVO struct {
	Header OneOffVO `json:"header"`
	// Messages are the run's raw messages in the same shape the transcript uses, so the
	// frontend renders them with the same bubbles and tool cards.
	Messages []SessionMessage `json:"messages"`
	// Requests are the API-call summaries (no prompt text — see OneOffRequestVO).
	Requests []OneOffRequestVO `json:"requests,omitempty"`
	// Findings are the ReportFinding calls THIS run made — the run's own, not "the newest
	// review in the session" (see GetReviewFindings for that other question). It is what the
	// panel's findings pane shows for whichever run the reader selected.
	Findings []FindingVO `json:"findings,omitempty"`
	// Notice explains a failure: bad name, unreadable file, unreadable record.
	Notice string `json:"notice,omitempty"`
}

// oneOffMetaLine is the header a record starts with. Only the fields the panel needs are
// declared: system_prompt is in there too, but it runs to kilobytes and the panel gets it
// per request instead.
type oneOffMetaLine struct {
	Type      string            `json:"type"`
	Kind      string            `json:"kind"`
	SessionID string            `json:"session_id"`
	Provider  string            `json:"provider"`
	Model     string            `json:"model"`
	StartedAt time.Time         `json:"started_at"`
	Extra     map[string]string `json:"extra"`
}

// ListOneOffs lists this session's side-channel runs, newest first.
//
// It reads each record's header line and its file metadata only. The full parse is
// LoadOneOff's job, for the one run the reader is actually looking at.
func (s *AgentService) ListOneOffs(sessionID string) OneOffListVO {
	dir, err := oneOffDir(sessionID)
	if err != nil {
		return OneOffListVO{Note: err.Error()}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return OneOffListVO{Note: "这个会话还没有旁路运行（评审、提交）"}
		}
		return OneOffListVO{Note: "读取旁路记录目录失败：" + err.Error()}
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		names = append(names, e.Name())
	}

	items := make([]oneOffEntry, 0, len(names))
	for _, name := range names {
		vo, started, err := readOneOffHeader(filepath.Join(dir, name), name)
		if err != nil {
			// One damaged record must not close the whole panel; the list is a menu, and
			// a record that cannot even be identified is not a candidate.
			continue
		}
		// The directory is per session, but the header names its session: a record copied
		// or left behind by another run must not show up as this conversation's.
		if vo.SessionID != "" && vo.SessionID != sessionID {
			continue
		}
		items = append(items, oneOffEntry{vo: vo, at: started})
	}
	if len(items) == 0 {
		return OneOffListVO{Note: "这个会话还没有旁路运行（评审、提交）"}
	}

	// Newest first by the TIME the record states, not by its name: a name is
	// <kind>-<timestamp>-<rand>, so sorting by name would sort by kind first and put every
	// review-round-* ahead of every plain review-* (and every commit last), whatever the
	// dates say.
	sort.SliceStable(items, func(i, j int) bool {
		if !items[i].at.Equal(items[j].at) {
			return items[i].at.After(items[j].at)
		}
		return items[i].vo.Name > items[j].vo.Name // deterministic tie-break
	})
	out := make([]OneOffVO, 0, len(items))
	for _, it := range items {
		out = append(out, it.vo)
	}
	return OneOffListVO{Items: out}
}

// oneOffEntry is a record plus the time it is ordered by. Kept out of OneOffVO so the
// binding payload carries no ordering key the frontend would have to know about.
type oneOffEntry struct {
	vo OneOffVO
	at time.Time
}

// LoadOneOff reads one run: its messages (in the transcript's own shape) plus the API-call
// summaries. Prompt text is left out — see OneOffRequestVO.
func (s *AgentService) LoadOneOff(sessionID, name string) OneOffDetailVO {
	path, err := oneOffPath(sessionID, name)
	if err != nil {
		return OneOffDetailVO{Notice: err.Error()}
	}
	header, _, err := readOneOffHeader(path, name)
	if err != nil {
		return OneOffDetailVO{Notice: "读取旁路记录失败：" + err.Error()}
	}

	f, err := os.Open(path)
	if err != nil {
		return OneOffDetailVO{Notice: "读取旁路记录失败：" + err.Error()}
	}
	defer f.Close()

	var (
		msgs     []session.Message
		requests []OneOffRequestVO
		findings []FindingVO
	)
	sc := newOneOffScanner(f)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		lineType, err := oneOffLineTypeOf(line)
		if err != nil {
			continue // a malformed line is not worth failing the panel over
		}
		switch lineType {
		case oneOffLineMeta:
			continue // already read as the header
		case oneOffLineAPIRequest:
			var req session.APIRequest
			if err := json.Unmarshal(line, &req); err != nil {
				continue
			}
			requests = append(requests, oneOffRequest(req, false))
			continue
		}
		var msg session.Message
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		// The run's own findings, collected on the way past: they belong to THIS record, which
		// is what lets the panel show the findings of the run the reader selected rather than
		// "the newest review in the session". Counting them is the same traversal, so the
		// header's tally costs nothing extra.
		if msg.Type == session.MessageTypeToolCall && msg.Name == tools.ToolNameReportFinding {
			if f, ok := findingFromToolCall(msg.Args); ok {
				findings = append(findings, f)
			}
		}
		msgs = append(msgs, msg)
	}
	if err := sc.Err(); err != nil {
		return OneOffDetailVO{Header: header, Notice: "读取旁路记录中断：" + err.Error()}
	}

	header.Findings = len(findings)
	return OneOffDetailVO{
		Header: header, Messages: buildSessionMessages(msgs),
		Requests: requests, Findings: findings,
	}
}

// LoadOneOffRequest returns one LLM call in full, prompts included, by its Seq. It is a
// separate call because the prompts are over half of a record's bytes and only a reader
// who asked for them should pay for them.
func (s *AgentService) LoadOneOffRequest(sessionID, name string, seq int) OneOffRequestVO {
	path, err := oneOffPath(sessionID, name)
	if err != nil {
		return OneOffRequestVO{Notice: err.Error()}
	}
	f, err := os.Open(path)
	if err != nil {
		return OneOffRequestVO{Notice: "读取旁路记录失败：" + err.Error()}
	}
	defer f.Close()

	sc := newOneOffScanner(f)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if t, err := oneOffLineTypeOf(line); err != nil || t != oneOffLineAPIRequest {
			continue
		}
		var req session.APIRequest
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		if req.Seq == seq {
			return oneOffRequest(req, true)
		}
	}
	if err := sc.Err(); err != nil {
		return OneOffRequestVO{Notice: "读取旁路记录中断：" + err.Error()}
	}
	return OneOffRequestVO{Seq: seq, Notice: fmt.Sprintf("记录里没有第 %d 次请求", seq)}
}

// oneOffRequest maps a recorded API call. withPrompts decides whether the prompt text is
// carried — the list of summaries deliberately leaves it out.
func oneOffRequest(req session.APIRequest, withPrompts bool) OneOffRequestVO {
	vo := OneOffRequestVO{
		Seq:        req.Seq,
		Iteration:  req.Iteration,
		Provider:   req.Provider,
		Model:      req.Model,
		Thinking:   req.Thinking,
		DurationMs: req.DurationMs,
	}
	if !req.Timestamp.IsZero() {
		vo.Timestamp = req.Timestamp.Format(time.RFC3339)
	}
	// Tool NAMES only: the schema is boilerplate that repeats in every request, and the
	// question "what was this run actually told to do" is answered by the prompts.
	for _, t := range req.Tools {
		vo.Tools = append(vo.Tools, t.Name)
	}
	if withPrompts {
		vo.SystemPrompt = req.SystemPrompt
		vo.UserPrompt = req.UserPrompt
	}
	return vo
}

// readOneOffHeader reads a record's meta line and file metadata: what the list needs, and
// nothing that costs a full read of a file that can run to hundreds of kilobytes.
//
// The returned time is the ordering key: the header's started_at, falling back to the
// file's mtime for a record that predates the field.
func readOneOffHeader(path, name string) (OneOffVO, time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return OneOffVO{}, time.Time{}, err
	}
	defer f.Close()

	line, err := bufio.NewReaderSize(f, 64<<10).ReadBytes('\n')
	if err != nil && len(bytes.TrimSpace(line)) == 0 {
		return OneOffVO{}, time.Time{}, err
	}
	var m oneOffMetaLine
	if err := json.Unmarshal(bytes.TrimSpace(line), &m); err != nil {
		return OneOffVO{}, time.Time{}, fmt.Errorf("%s: 首行不是 meta：%w", name, err)
	}
	if m.Type != oneOffLineMeta {
		return OneOffVO{}, time.Time{}, fmt.Errorf("%s: 首行不是 meta（type=%q）", name, m.Type)
	}

	vo := OneOffVO{
		Name: name, Run: name, Kind: m.Kind, Round: oneOffRound(m.Kind),
		SessionID: m.SessionID, Provider: m.Provider, Model: m.Model,
	}
	if !m.StartedAt.IsZero() {
		vo.StartedAt = m.StartedAt.Format(time.RFC3339)
	}
	if m.Extra != nil {
		if report := m.Extra[agent.OneOffKeyReport]; report != "" {
			vo.Report = report
			// The batch key: every round of one review shares the orchestrator-owned
			// report directory (agent/commands/review.go), which the file names cannot say.
			vo.Run = filepath.Dir(report)
		}
		vo.ReviewedMsg = m.Extra[agent.OneOffKeyReviewedMsg]
		if raw := m.Extra[agent.OneOffKeyPaths]; raw != "" {
			// A malformed list is not worth failing the record over: the panel then shows
			// the findings without a diff, which is what an unscoped review looks like too.
			_ = json.Unmarshal([]byte(raw), &vo.Paths)
		}
	}
	started := m.StartedAt
	if st, err := f.Stat(); err == nil {
		vo.UpdatedAt = st.ModTime().Format(time.RFC3339)
		vo.Running = time.Since(st.ModTime()) < oneOffRunningWindow
		if started.IsZero() {
			started = st.ModTime()
		}
	}
	return vo, started, nil
}

// oneOffRound extracts the round number from a kind ("review-round-2" → 2). A single-round
// review records the plain kind, and so does every other command: 0 means "not a round".
func oneOffRound(kind string) int {
	if !strings.HasPrefix(kind, oneOffRoundPrefix) {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimPrefix(kind, oneOffRoundPrefix))
	if err != nil {
		return 0
	}
	return n
}

// oneOffLineTypeOf reads only the line's "type", which is all the dispatcher needs before
// deciding whether to unmarshal it as a message, a request, or nothing at all.
func oneOffLineTypeOf(line []byte) (string, error) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		return "", err
	}
	return probe.Type, nil
}

// newOneOffScanner is a line scanner sized for record lines rather than bufio's default.
func newOneOffScanner(f *os.File) *bufio.Scanner {
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxOneOffLineBytes)
	return sc
}

// oneOffDir locates a session's record directory.
//
// sessionID arrives from the webview, so it is treated as untrusted: only a single path
// element is accepted. Without that check a name like "../../other-session" would move the
// read somewhere else entirely, and these methods take a file NAME from the frontend too.
func oneOffDir(sessionID string) (string, error) {
	if sessionID == "" {
		return "", errors.New("没有会话")
	}
	if !isSinglePathElement(sessionID) {
		return "", fmt.Errorf("非法会话名：%q", sessionID)
	}
	dir, err := config.SessionDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, sessionID, "oneoff"), nil
}

// oneOffPath resolves a record file name inside the session's directory. Same rule as
// oneOffDir, plus the extension: the name only ever comes from ListOneOffs.
func oneOffPath(sessionID, name string) (string, error) {
	dir, err := oneOffDir(sessionID)
	if err != nil {
		return "", err
	}
	if !isSinglePathElement(name) || filepath.Ext(name) != ".jsonl" {
		return "", fmt.Errorf("非法记录名：%q", name)
	}
	return filepath.Join(dir, name), nil
}

// isSinglePathElement reports whether s names one entry in one directory: no separator, no
// traversal, and not the current/parent directory itself.
func isSinglePathElement(s string) bool {
	return s != "" && s != "." && s != ".." &&
		s == filepath.Base(s) && !strings.ContainsAny(s, `/\`)
}

// GetUIState returns the desktop-only UI preferences the frontend owns.
//
// The frontend keeps its own copy (React state drives the frame) and reads this once at
// mount: the file is the durable half, not the source of truth for what is on screen.
type UIStateVO struct {
	// OneOffPanelOpen is whether the side-channel panel was open last time.
	OneOffPanelOpen bool `json:"oneOffPanelOpen"`
	// OneOffPanelWidth is that panel's width in CSS pixels; 0 means "never dragged".
	OneOffPanelWidth int `json:"oneOffPanelWidth,omitempty"`
}

func (s *AgentService) GetUIState() UIStateVO {
	st := loadUIState()
	return UIStateVO{OneOffPanelOpen: st.OneOffPanelOpen, OneOffPanelWidth: st.OneOffPanelWidth}
}

// SetOneOffPanelOpen records whether the side-channel panel is open. Written on every toggle
// and best effort, like the rest of uiState: a failure costs the next launch the panel's
// position and nothing else.
func (s *AgentService) SetOneOffPanelOpen(open bool) {
	st := loadUIState()
	if st.OneOffPanelOpen == open {
		return
	}
	st.OneOffPanelOpen = open
	saveUIState(st)
}

// SetOneOffPanelWidth records how wide the reader made the panel. The value is refused when it
// is outside the panel's bounds: this is a hand-editable file, and a width the layout cannot
// honour is worse than the default it falls back to.
func (s *AgentService) SetOneOffPanelWidth(px int) {
	if !validOneOffPanelWidth(px) {
		return
	}
	st := loadUIState()
	if st.OneOffPanelWidth == px {
		return
	}
	st.OneOffPanelWidth = px
	saveUIState(st)
}
