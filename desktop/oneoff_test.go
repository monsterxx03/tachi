package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/session"
)

// The fixtures below are built by marshalling the real types, never by hand-writing JSON:
// the whole point of this reader is that a record IS session.Message (see the file comment
// in oneoff.go), and a hand-rolled fixture would stop testing that the moment the recorder
// or the message shape changed.

// oneOffTestDir pins the process-global base dir at t.TempDir() (never the real ~/.tachi,
// same rule as fileservice_test.go) and returns this session's record directory.
func oneOffTestDir(t *testing.T, sessionID string) string {
	t.Helper()
	config.SetBaseDir(t.TempDir())
	dir, err := config.SessionDir()
	if err != nil {
		t.Fatalf("session dir: %v", err)
	}
	path := filepath.Join(dir, sessionID, "oneoff")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return path
}

func writeOneOffFile(t *testing.T, dir, name string, lines ...string) {
	t.Helper()
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// metaLine builds a record header exactly as the recorder writes it (oneoffMetaLine).
func metaLine(t *testing.T, kind, sessionID, report string, startedAt time.Time) string {
	t.Helper()
	m := oneOffMetaLine{Type: oneOffLineMeta, Kind: kind, SessionID: sessionID, StartedAt: startedAt,
		Provider: "anthropic", Model: "test-model", Extra: map[string]string{}}
	if report != "" {
		m.Extra[agent.OneOffKeyReport] = report
	}
	if len(m.Extra) == 0 {
		m.Extra = nil
	}
	return mustJSON(t, m)
}

// messageLine builds one recorded message line by marshalling session.Message — the shape
// the recorder produces verbatim.
func messageLine(t *testing.T, msg session.Message) string {
	t.Helper()
	return mustJSON(t, msg)
}

// requestLine builds one api_request line: the recorder embeds session.APIRequest under a
// "type" field, which flattens into the same JSON object.
func requestLine(t *testing.T, req session.APIRequest, toolNames ...string) string {
	t.Helper()
	for _, name := range toolNames {
		req.Tools = append(req.Tools, session.APITool{Name: name})
	}
	return mustJSON(t, struct {
		Type string `json:"type"`
		session.APIRequest
	}{Type: oneOffLineAPIRequest, APIRequest: req})
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return string(b)
}

const oneOffTestSession = "2026-09-12-193137-a8acf6c3"

// TestListOneOffs covers what the panel's switcher needs: newest first, the rounds of one
// review grouped under one run, and everything that must NOT show up (another session's
// record, a corrupt file, a non-record file).
func TestListOneOffs(t *testing.T) {
	dir := oneOffTestDir(t, oneOffTestSession)
	started := time.Date(2026, 9, 12, 19, 35, 35, 0, time.Local)
	// One review, two rounds: same orchestrator-owned report directory.
	reportDir := filepath.Join(t.TempDir(), ".tachi", "reviews", "20260912-193535")
	pending := filepath.Join(t.TempDir(), ".tachi", "reviews", "20260912-194000")

	writeOneOffFile(t, dir, "review-round-1-20260912-193500-aaaa.jsonl",
		metaLine(t, "review-round-1", oneOffTestSession, filepath.Join(reportDir, "round-1-senior.md"), started))
	writeOneOffFile(t, dir, "review-round-2-20260912-193505-bbbb.jsonl",
		metaLine(t, "review-round-2", oneOffTestSession, filepath.Join(reportDir, "round-2-judge.md"), started))
	// A second, newer review batch (single round).
	writeOneOffFile(t, dir, "review-20260912-194010-cccc.jsonl",
		metaLine(t, "review", oneOffTestSession, filepath.Join(pending, "round-1-senior.md"), started.Add(time.Minute)))
	// A commit: same directory, different kind, no report.
	writeOneOffFile(t, dir, "commit-20260912-194500-dddd.jsonl",
		metaLine(t, "commit", oneOffTestSession, "", started.Add(2*time.Minute)))
	// Noise that must be filtered out.
	writeOneOffFile(t, dir, "review-20260912-195000-eeee.jsonl",
		metaLine(t, "review", "another-session", "", started))
	writeOneOffFile(t, dir, "review-20260912-195500-ffff.jsonl", "not json at all")
	writeOneOffFile(t, dir, "notes.txt", "irrelevant")

	got := (&AgentService{}).ListOneOffs(oneOffTestSession)
	if got.Note != "" {
		t.Fatalf("ListOneOffs note = %q, want empty", got.Note)
	}
	var names []string
	for _, it := range got.Items {
		names = append(names, it.Name)
	}
	want := []string{
		"commit-20260912-194500-dddd.jsonl",
		"review-20260912-194010-cccc.jsonl",
		"review-round-2-20260912-193505-bbbb.jsonl",
		"review-round-1-20260912-193500-aaaa.jsonl",
	}
	if strings.Join(names, "|") != strings.Join(want, "|") {
		t.Errorf("order = %v, want %v (newest first, filtered)", names, want)
	}

	byName := map[string]OneOffVO{}
	for _, it := range got.Items {
		byName[it.Name] = it
	}
	r1, r2 := byName[want[3]], byName[want[2]]
	if r1.Run != r2.Run || r1.Run != reportDir {
		t.Errorf("rounds of one review must share the report dir as their run key: %q vs %q", r1.Run, r2.Run)
	}
	if r1.Round != 1 || r2.Round != 2 {
		t.Errorf("rounds = %d/%d, want 1/2", r1.Round, r2.Round)
	}
	if byName[want[1]].Run != pending {
		t.Errorf("a second batch must not merge into the first: run = %q", byName[want[1]].Run)
	}
	if c := byName[want[0]]; c.Kind != "commit" || c.Run != c.Name || c.Report != "" {
		t.Errorf("commit entry = %+v, want kind=commit, run=name, no report", c)
	}
	if r1.Provider != "anthropic" || r1.Model != "test-model" || r1.StartedAt == "" || r1.UpdatedAt == "" {
		t.Errorf("header fields not carried: %+v", r1)
	}
}

// TestListOneOffs carries the review origin: which turn asked for a run and which files it
// covered. That pairing is the durable half of 「已评审 N 条」 — the frontend's own memory of
// it dies with the window (P4).
func TestListOneOffsReviewOrigin(t *testing.T) {
	dir := oneOffTestDir(t, oneOffTestSession)
	started := time.Date(2026, 9, 12, 21, 0, 0, 0, time.Local)
	m := oneOffMetaLine{
		Type: oneOffLineMeta, Kind: "review", SessionID: oneOffTestSession, StartedAt: started,
		Extra: map[string]string{
			agent.OneOffKeyReport:      filepath.Join(t.TempDir(), "round-1-judge.md"),
			agent.OneOffKeyReviewedMsg: "a-1757680000",
			agent.OneOffKeyPaths:       `["NOTES.md","src/main.go"]`,
		},
	}
	writeOneOffFile(t, dir, "review-20260912-210000-aaaa.jsonl", mustJSON(t, m))
	// A whole-tree review (a typed command): no turn, no scope.
	writeOneOffFile(t, dir, "review-20260912-210500-bbbb.jsonl",
		metaLine(t, "review", oneOffTestSession, "", started.Add(time.Minute)))
	// A malformed scope must not sink the record: the findings still show, without a diff.
	writeOneOffFile(t, dir, "review-20260912-211000-cccc.jsonl", mustJSON(t, oneOffMetaLine{
		Type: oneOffLineMeta, Kind: "review", SessionID: oneOffTestSession, StartedAt: started.Add(2 * time.Minute),
		Extra: map[string]string{agent.OneOffKeyPaths: `["NOTES.md"`},
	}))

	items := (&AgentService{}).ListOneOffs(oneOffTestSession).Items
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3", len(items))
	}
	byName := map[string]OneOffVO{}
	for _, it := range items {
		byName[it.Name] = it
	}
	scoped := byName["review-20260912-210000-aaaa.jsonl"]
	if scoped.ReviewedMsg != "a-1757680000" {
		t.Errorf("reviewedMsg = %q, want the turn that asked for the review", scoped.ReviewedMsg)
	}
	if strings.Join(scoped.Paths, ",") != "NOTES.md,src/main.go" {
		t.Errorf("paths = %v, want the recorded scope", scoped.Paths)
	}
	if whole := byName["review-20260912-210500-bbbb.jsonl"]; whole.ReviewedMsg != "" || len(whole.Paths) != 0 {
		t.Errorf("a typed review must carry no origin: %+v", whole)
	}
	if bad := byName["review-20260912-211000-cccc.jsonl"]; len(bad.Paths) != 0 {
		t.Errorf("a malformed scope is dropped, not fatal: %+v", bad.Paths)
	}
}

func TestListOneOffsWithoutRecords(t *testing.T) {
	oneOffTestDir(t, oneOffTestSession)
	got := (&AgentService{}).ListOneOffs(oneOffTestSession)
	if len(got.Items) != 0 || got.Note == "" {
		t.Fatalf("empty list must explain itself: %+v", got)
	}
	if got := (&AgentService{}).ListOneOffs(""); got.Note == "" {
		t.Error("an empty session id must be refused, not silently listed")
	}
}

// TestLoadOneOffReplaysRecord walks the whole reader: a record replayed into the SAME
// SessionMessage shape a session's transcript uses, the findings counted, and the request
// split into a prompt-free summary plus an on-demand full fetch.
func TestLoadOneOffReplaysRecord(t *testing.T) {
	dir := oneOffTestDir(t, oneOffTestSession)
	name := "review-20260912-193535-bc05.jsonl"
	ts := time.Date(2026, 9, 12, 19, 35, 36, 0, time.Local)
	report := filepath.Join(t.TempDir(), ".tachi", "reviews", "20260912-193535", "round-1-judge.md")

	writeOneOffFile(t, dir, name,
		metaLine(t, "review", oneOffTestSession, report, ts),
		messageLine(t, session.Message{Type: session.MessageTypeUser, Content: "review these files", Timestamp: ts}),
		requestLine(t, session.APIRequest{
			Seq: 1, Iteration: 1, Timestamp: ts, Provider: "anthropic", Model: "test-model",
			SystemPrompt: "SYSTEM PROMPT TEXT", UserPrompt: "review these files", DurationMs: 42,
		}, "Bash", tools.ToolNameReportFinding),
		messageLine(t, session.Message{Type: session.MessageTypeThinking, Content: "let me look", Timestamp: ts}),
		messageLine(t, session.Message{Type: session.MessageTypeAssistant, Content: "starting", Seq: 1, Timestamp: ts}),
		messageLine(t, session.Message{Type: session.MessageTypeToolCall, Name: "Bash", ToolCallID: "c1",
			Args: `{"command":"git status"}`, Seq: 1, Timestamp: ts}),
		messageLine(t, session.Message{Type: session.MessageTypeToolResult, Name: "Bash", ToolCallID: "c1",
			Result: "nothing to commit", DurationMs: 7, Seq: 1, Timestamp: ts}),
		messageLine(t, session.Message{Type: session.MessageTypeToolCall, Name: tools.ToolNameReportFinding, ToolCallID: "c2",
			Args: `{"path":"desktop/oneoff.go","line":10,"severity":"warn","text":"watch out"}`, Seq: 1, Timestamp: ts}),
		messageLine(t, session.Message{Type: session.MessageTypeToolResult, Name: tools.ToolNameReportFinding, ToolCallID: "c2",
			Result: "已记录", Seq: 1, Timestamp: ts}),
		messageLine(t, session.Message{Type: session.MessageTypeAssistant, Content: "done", Seq: 1, Timestamp: ts}),
	)

	got := (&AgentService{}).LoadOneOff(oneOffTestSession, name)
	if got.Notice != "" {
		t.Fatalf("LoadOneOff notice = %q, want empty", got.Notice)
	}
	if got.Header.Name != name || got.Header.Kind != "review" || got.Header.Report != report {
		t.Errorf("header = %+v", got.Header)
	}
	if got.Header.Findings != 1 {
		t.Errorf("findings = %d, want 1 (only the ReportFinding call counts)", got.Header.Findings)
	}
	// The run's own findings, parsed out of the same traversal — the two recorded argument
	// shapes (a JSON string vs the object) both have to land here.
	if len(got.Findings) != 1 {
		t.Fatalf("findings payload = %d, want 1", len(got.Findings))
	}
	if f := got.Findings[0]; f.Path != "desktop/oneoff.go" || f.Line != 10 || f.Severity != "warn" || f.Text == "" {
		t.Errorf("finding not parsed from the recorded args: %+v", f)
	}

	var roles []string
	for _, m := range got.Messages {
		roles = append(roles, m.Role)
	}
	wantRoles := []string{"user", "assistant", "tool_call", "tool_result", "tool_call", "tool_result", "assistant"}
	if strings.Join(roles, "|") != strings.Join(wantRoles, "|") {
		t.Fatalf("roles = %v, want %v", roles, wantRoles)
	}
	// The thinking block belongs to the assistant message that follows it, exactly as it
	// does in the transcript (buildSessionMessages buckets it).
	if got.Messages[1].Thinking != "let me look" || got.Messages[1].Content != "starting" {
		t.Errorf("thinking not merged into the assistant message: %+v", got.Messages[1])
	}
	if tc := got.Messages[2]; tc.ToolName != "Bash" || tc.Title == "" || !strings.Contains(tc.Args, "git status") {
		t.Errorf("tool_call not mapped: %+v", tc)
	}
	if tr := got.Messages[3]; tr.ToolResult != "nothing to commit" || tr.IsError {
		t.Errorf("tool_result not mapped: %+v", tr)
	}

	if len(got.Requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(got.Requests))
	}
	sum := got.Requests[0]
	if sum.Seq != 1 || sum.DurationMs != 42 || strings.Join(sum.Tools, ",") != "Bash,"+tools.ToolNameReportFinding {
		t.Errorf("request summary = %+v", sum)
	}
	if sum.SystemPrompt != "" || sum.UserPrompt != "" {
		t.Error("the summary must not carry prompt text: it is over half of a record's bytes")
	}

	full := (&AgentService{}).LoadOneOffRequest(oneOffTestSession, name, 1)
	if full.SystemPrompt != "SYSTEM PROMPT TEXT" || full.UserPrompt != "review these files" {
		t.Errorf("lazy request fetch = %+v", full)
	}
	if miss := (&AgentService{}).LoadOneOffRequest(oneOffTestSession, name, 99); miss.Notice == "" {
		t.Error("a missing request must say so rather than return an empty one")
	}
}

// TestOneOffPathsAreUntrusted: both the session id and the record name arrive from the
// webview, so neither may escape the session's directory.
func TestOneOffPathsAreUntrusted(t *testing.T) {
	dir := oneOffTestDir(t, oneOffTestSession)
	// A record that exists, to prove the refusals are about the NAME, not about absence.
	writeOneOffFile(t, dir, "review-20260912-193535-bc05.jsonl",
		metaLine(t, "review", oneOffTestSession, "", time.Now()))

	for _, tc := range []struct{ session, name string }{
		{"../2026-09-12-193137-a8acf6c3", "review-20260912-193535-bc05.jsonl"},
		{"..", "review-20260912-193535-bc05.jsonl"},
		{"", "review-20260912-193535-bc05.jsonl"},
		{oneOffTestSession, "../../../etc/passwd"},
		{oneOffTestSession, "sub/review-20260912-193535-bc05.jsonl"},
		{oneOffTestSession, "review-20260912-193535-bc05.txt"},
		{oneOffTestSession, ""},
	} {
		if got := (&AgentService{}).LoadOneOff(tc.session, tc.name); got.Notice == "" {
			t.Errorf("LoadOneOff(%q, %q) was accepted; want a refusal", tc.session, tc.name)
		}
		if got := (&AgentService{}).ListOneOffs(tc.session); tc.session != oneOffTestSession && got.Note == "" {
			t.Errorf("ListOneOffs(%q) was accepted; want a refusal", tc.session)
		}
	}
	if _, err := oneOffPath(oneOffTestSession, "../x.jsonl"); err == nil {
		t.Error("oneOffPath accepted a traversal")
	}
}

func TestOneOffRoundFromKind(t *testing.T) {
	for _, tc := range []struct {
		kind string
		want int
	}{
		{"review", 0}, {"commit", 0}, {"review-round-1", 1}, {"review-round-10", 10},
		{"review-round-x", 0}, {"review-round-", 0}, {"review-round1", 0},
	} {
		if got := oneOffRound(tc.kind); got != tc.want {
			t.Errorf("oneOffRound(%q) = %d, want %d", tc.kind, got, tc.want)
		}
	}
}
