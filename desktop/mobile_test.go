package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/cron"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// 移动接口的测试钉住的是设计里那几条不变量（docs/2026-10-18-mobile-client-design.md §11.1），
// 不是端点逐个跑一遍：哪个端点返回 200 由实现决定，哪条规则不能被踩由测试决定。

// newMobileApp builds an app with a temp session store. config.SetBaseDir is a process
// global, so these tests never run in parallel.
func newMobileApp(t *testing.T) (*desktopApp, *session.Manager) {
	t.Helper()
	config.SetBaseDir(t.TempDir())
	sm, err := session.NewManager(nil)
	if err != nil {
		t.Fatalf("session manager: %v", err)
	}
	d := newTestApp()
	d.sm = sm
	d.cfg = &config.Config{Language: "en", Mobile: config.MobileConfig{Enabled: true}}
	return d, sm
}

// writeRecords writes a session's raw record file directly, so a test can control timestamps
// (Manager.AppendMessage stamps time.Now, which makes a same-instant page boundary impossible
// to build).
func writeRecords(t *testing.T, id string, msgs ...string) {
	t.Helper()
	dir, err := config.SessionDir()
	if err != nil {
		t.Fatalf("session dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, id, "messages.jsonl"), []byte(strings.Join(msgs, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write records: %v", err)
	}
}

func mobileTestServer(t *testing.T, d *desktopApp) (*mobileServer, *httptest.Server) {
	t.Helper()
	m := newMobileServer(d, "secret")
	ts := httptest.NewServer(m.handler())
	// Register it on the app, the way startMobile does: the event paths find it through
	// desktopApp.mobileServer(), and a test that only kept the *mobileServer would silently
	// broadcast nowhere.
	d.setMobileServer(m)
	t.Cleanup(func() {
		ts.Close()
		d.setMobileServer(nil)
		m.close()
	})
	return m, ts
}

func mobileCall(t *testing.T, ts *httptest.Server, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	var rdr *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = strings.NewReader(string(b))
	} else {
		rdr = strings.NewReader("")
	}
	req, err := http.NewRequest(method, ts.URL+path, rdr)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

// TestMobileAuth: ping answers without a token (the pairing page has to reach something
// first); everything else refuses a missing or wrong one.
func TestMobileAuth(t *testing.T) {
	d, _ := newMobileApp(t)
	_, ts := mobileTestServer(t, d)

	code, ping := mobileCall(t, ts, "GET", "/v1/ping", "", nil)
	if code != http.StatusOK {
		t.Fatalf("ping without token = %d, want 200", code)
	}
	// hostname is what a phone with several saved computers labels this one with, so an empty one
	// silently leaves every row saying "tachi-desktop · <address>".
	if h, _ := ping["hostname"].(string); h == "" {
		t.Fatalf("ping carries no hostname: %v", ping)
	}
	if name, _ := ping["name"].(string); name == "" {
		t.Fatalf("ping carries no name: %v", ping)
	}
	// urls is the phone's way to learn the addresses it was never paired with (a laptop that
	// changed network, a tailnet that came up). It has to be on every ping, and it has to be an
	// array even when this machine has nothing to offer — a missing key makes the phone think the
	// server is old, which is a different thing from "this machine has no address".
	if _, ok := ping["urls"].([]any); !ok {
		t.Fatalf("ping carries no urls array: %v", ping["urls"])
	}
	if code, _ := mobileCall(t, ts, "GET", "/v1/sessions", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("sessions without token = %d, want 401", code)
	}
	if code, _ := mobileCall(t, ts, "GET", "/v1/sessions", "wrong", nil); code != http.StatusUnauthorized {
		t.Fatalf("sessions with a wrong token = %d, want 401", code)
	}
	if code, _ := mobileCall(t, ts, "GET", "/v1/sessions", "secret", nil); code != http.StatusOK {
		t.Fatalf("sessions with the token = %d, want 200", code)
	}
}

// TestMobileListFoldsTheCompactionChain: one conversation is one row — the parent of a chain
// must not appear next to the child that continued it. Folding happens on the server, so the
// phone never builds a second definition of "which row is this conversation".
func TestMobileListFoldsTheCompactionChain(t *testing.T) {
	d, sm := newMobileApp(t)
	parent, err := sm.New("p", t.TempDir())
	if err != nil {
		t.Fatalf("new parent: %v", err)
	}
	child, err := sm.New("p", t.TempDir())
	if err != nil {
		t.Fatalf("new child: %v", err)
	}
	child.CompactedParentID = parent.ID
	child.Title = "child"
	if err := sm.UpdateMeta(child); err != nil {
		t.Fatalf("update child: %v", err)
	}

	rows, err := d.mobileSessions()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	if len(rows) != 1 || rows[0].ID != child.ID {
		t.Fatalf("rows = %v, want only the leaf %s", ids, child.ID)
	}
}

// TestMobileRowCarriesTheCronCount: a conversation a timed job writes into is marked on its
// row. The number comes from the same scan the sidebar's badge uses (per logical id, so a
// compaction chain counts as one conversation), and the phone only draws it.
func TestMobileRowCarriesTheCronCount(t *testing.T) {
	d, sm := newMobileApp(t)
	s, err := sm.New("p", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	other, err := sm.New("p", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	path := filepath.Join(t.TempDir(), "crons.json")
	store := cron.NewStore(path)
	for _, id := range []string{"c1", "c2"} {
		if err := store.Create(&cron.Job{
			ID: id, Name: id, Schedule: "@every 1h", Prompt: "p",
			TargetType: cron.TargetTypeDesktop,
			Desktop:    &cron.DesktopTarget{Mode: cron.DesktopModeSession, SessionID: s.LogicalConversationID()},
		}); err != nil {
			t.Fatalf("create job: %v", err)
		}
	}
	d.cfg = &config.Config{Language: "en", Cron: config.CronConfig{StorePath: path}}

	rows, err := d.mobileSessions()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byID := map[string]mobileSessionSummary{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	if got := byID[s.ID].Cron; got != 2 {
		t.Fatalf("cron on the bound row = %d, want 2", got)
	}
	if got := byID[other.ID].Cron; got != 0 {
		t.Fatalf("cron on an unbound row = %d, want 0", got)
	}
}

// TestMobileRowCarriesTheNextRun: the row says when the conversation comes alive again — the
// EARLIEST of its active jobs, since that is the next thing that will actually happen (the last
// one would name a moment by which everything already had). A paused job still COUNTS — something
// is bound to this conversation and a delete would take it with it — but it schedules nothing, so
// a conversation whose jobs are all paused keeps the number and carries no time.
func TestMobileRowCarriesTheNextRun(t *testing.T) {
	d, sm := newMobileApp(t)
	soon, err := sm.New("p", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	pausedOnly, err := sm.New("p", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	plain, err := sm.New("p", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	path := filepath.Join(t.TempDir(), "crons.json")
	store := cron.NewStore(path)
	job := func(id, schedule string, status cron.JobStatus, target string) {
		t.Helper()
		if err := store.Create(&cron.Job{
			ID: id, Name: id, Schedule: schedule, Prompt: "p", Status: status,
			TargetType: cron.TargetTypeDesktop,
			Desktop:    &cron.DesktopTarget{Mode: cron.DesktopModeSession, SessionID: target},
		}); err != nil {
			t.Fatalf("create job %s: %v", id, err)
		}
	}
	// Two active jobs far apart in time, so "earliest" and "latest" are different answers.
	job("c-soon", "@every 1m", cron.JobStatusActive, soon.LogicalConversationID())
	job("c-later", "@every 5h", cron.JobStatusActive, soon.LogicalConversationID())
	job("c-paused", "@every 1m", cron.JobStatusPaused, pausedOnly.LogicalConversationID())
	d.cfg = &config.Config{Language: "en", Cron: config.CronConfig{StorePath: path}}

	rows, err := d.mobileSessions()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byID := map[string]mobileSessionSummary{}
	for _, r := range rows {
		byID[r.ID] = r
	}

	got := byID[soon.ID]
	if got.Cron != 2 {
		t.Fatalf("cron on the bound row = %d, want 2", got.Cron)
	}
	if got.CronNext == "" {
		t.Fatal("an active job says nothing about when it runs")
	}
	next, err := time.Parse(time.RFC3339, got.CronNext)
	if err != nil {
		t.Fatalf("cronNext %q is not RFC3339: %v", got.CronNext, err)
	}
	if d := time.Until(next); d < 30*time.Second || d > 90*time.Second {
		t.Fatalf("next run in %v, want the 1m job's (~60s) — the 5h one must not win", d)
	}

	if p := byID[pausedOnly.ID]; p.Cron != 1 || p.CronNext != "" {
		t.Fatalf("all-paused row = (%d, %q), want (1, \"\"): it counts, it schedules nothing", p.Cron, p.CronNext)
	}
	if p := byID[plain.ID]; p.Cron != 0 || p.CronNext != "" {
		t.Fatalf("no-job row = (%d, %q), want (0, \"\")", p.Cron, p.CronNext)
	}
}

// TestMobilePageUsesTheRecordIndex is the cursor rule: `before` is a record index, so two
// records written in the same instant still page one at a time. A timestamp cursor would have
// dropped or duplicated them.
func TestMobilePageUsesTheRecordIndex(t *testing.T) {
	d, sm := newMobileApp(t)
	s, err := sm.New("p", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	same := `"timestamp":"2026-10-19T10:00:00Z"`
	writeRecords(t, s.ID,
		`{"type":"user","content":"one",`+same+`}`,
		`{"type":"user","content":"two",`+same+`}`,
		`{"type":"user","content":"three",`+same+`}`,
	)

	// Newest page of two: indexes 1,2 — not "everything with this timestamp".
	page, hasMore, _, err := d.mobilePage(s.ID, 0, 2)
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if got := indexesOf(page); !equalInts(got, []int{1, 2}) {
		t.Fatalf("first page indexes = %v, want [1 2]", got)
	}
	if !hasMore {
		t.Fatal("hasMore = false, want true (index 0 is older)")
	}

	// The cursor is the oldest index already held: strictly older than 1 → only index 0.
	page, hasMore, _, err = d.mobilePage(s.ID, 1, 2)
	if err != nil {
		t.Fatalf("page before 1: %v", err)
	}
	if got := indexesOf(page); !equalInts(got, []int{0}) {
		t.Fatalf("second page indexes = %v, want [0]", got)
	}
	if hasMore {
		t.Fatal("hasMore = true, want false at the head of the record")
	}
}

func indexesOf(msgs []mobileMessage) []int {
	out := make([]int, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Index)
	}
	return out
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestMobileReadDoesNotChangeTheDeskView is invariant 1: opening, paging and snapshotting from
// the phone leave d.activeID exactly where the window left it.
func TestMobileReadDoesNotChangeTheDeskView(t *testing.T) {
	d, sm := newMobileApp(t)
	shown, err := sm.New("p", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	other, err := sm.New("p", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	writeRecords(t, other.ID, `{"type":"user","content":"hello","timestamp":"2026-10-19T10:00:00Z"}`)
	d.mu.Lock()
	d.activeID = shown.ID
	d.mu.Unlock()

	if _, _, _, err := d.mobilePage(other.ID, 0, 10); err != nil {
		t.Fatalf("page: %v", err)
	}
	if _, err := d.mobileSnapshot(other.ID); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	d.mu.Lock()
	got := d.activeID
	d.mu.Unlock()
	if got != shown.ID {
		t.Fatalf("activeID = %q, want the displayed session %q untouched", got, shown.ID)
	}
}

// TestMobileSkipApprovalIsAddressedBySession is invariant 8 for one of the two switches: the
// phone writes the conversation it named, and the window's session keeps its own mode.
func TestMobileSkipApprovalIsAddressedBySession(t *testing.T) {
	d, sm := newMobileApp(t)
	a, err := sm.New("p", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	b, err := sm.New("p", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	d.mu.Lock()
	d.activeID = a.ID
	d.mu.Unlock()

	if err := d.setSkipApprovalFor(b.ID, true); err != nil {
		t.Fatalf("set on b: %v", err)
	}
	if !d.skipApprovalFor(b.ID) {
		t.Fatal("b's mode was not written")
	}
	if d.skipApprovalFor(a.ID) {
		t.Fatal("setting b's mode changed the displayed session a")
	}
}

// TestMobileSendRefusals: each refusal keeps its own code, because the phone shows a different
// sentence (and a different way out) for each.
func TestMobileSendRefusals(t *testing.T) {
	d, sm := newMobileApp(t)
	s, err := sm.New("p", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	if code, got := d.mobileSend("nope", "hi", "send", nil); code != http.StatusNotFound || got != "no_session" {
		t.Fatalf("unknown session = (%d,%s), want (404,no_session)", code, got)
	}
	if code, got := d.mobileSend(s.ID, "hi", "steer", nil); code != http.StatusConflict || got != "not_running" {
		t.Fatalf("steer while idle = (%d,%s), want (409,not_running)", code, got)
	}

	// A turn in flight with no steer channel is a SCHEDULED turn: it says so rather than
	// claiming nothing is running.
	d.mu.Lock()
	r := d.getRun(s.ID)
	r.running = true
	d.mu.Unlock()
	if code, got := d.mobileSend(s.ID, "hi", "steer", nil); code != http.StatusConflict || got != "steer_unsupported" {
		t.Fatalf("steer on a scheduled turn = (%d,%s), want (409,steer_unsupported)", code, got)
	}
	if code, got := d.mobileSend(s.ID, "hi", "send", nil); code != http.StatusConflict || got != "turn_running" {
		t.Fatalf("send while running = (%d,%s), want (409,turn_running)", code, got)
	}
}

// TestMobilePermissionAnswerRefusals: a stale tool id is an explicit 409, never a silent
// success (an old card must not be able to approve something).
func TestMobilePermissionAnswerRefusals(t *testing.T) {
	d, sm := newMobileApp(t)
	s, err := sm.New("p", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if code, got := d.mobileAnswerPermission(s.ID, "gone", permAllowOnce); code != http.StatusConflict || got != "no_such_pending_request" {
		t.Fatalf("stale id = (%d,%s), want (409,no_such_pending_request)", code, got)
	}
	// V1 has no session-wide grant on the phone.
	if code, got := d.mobileAnswerPermission(s.ID, "x", permAllowSession); code != http.StatusBadRequest {
		t.Fatalf("allow_session = (%d,%s), want 400", code, got)
	}
}

// TestMobileAnswersBroadcast tells the other screens to drop the card, whichever screen
// answered (invariant 2 / 5).
func TestMobileAnswersBroadcast(t *testing.T) {
	d, sm := newMobileApp(t)
	s, err := sm.New("p", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	m, _ := mobileTestServer(t, d)
	c := newMobileConn(m, nil)
	c.out = make(chan []byte, 4)
	m.mu.Lock()
	m.conns[c] = struct{}{}
	m.mu.Unlock()

	d.notifyAskAnswered(s.ID, "tool-1", askKindPermission)
	select {
	case b := <-c.out:
		var ev map[string]any
		if err := json.Unmarshal(b, &ev); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if ev["type"] != mobileEvAskAnswered || ev["toolId"] != "tool-1" {
			t.Fatalf("event = %v, want ask_answered with the tool id", ev)
		}
	default:
		t.Fatal("no ask_answered was broadcast")
	}
}

// TestMobileSendDoesNotBlockOnAFullQueue is invariant 3: a phone that cannot keep up gets a
// lagged signal, and the goroutine emitting events never waits for it.
func TestMobileSendDoesNotBlockOnAFullQueue(t *testing.T) {
	d, _ := newMobileApp(t)
	m, _ := mobileTestServer(t, d)
	c := newMobileConn(m, nil)
	c.out = make(chan []byte, 1)
	c.lagged = make(chan struct{}, 1)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			c.send([]byte(`{"type":"text","text":"x"}`))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("send blocked on a full queue")
	}
	select {
	case <-c.lagged:
	default:
		t.Fatal("a full queue did not raise the lagged signal")
	}
}

// TestMobileUserMessageEcho: a message that another screen sent reaches the phone carrying its
// origin, and the phone's own send is marked as such. This is what stops the window from showing
// a reply to a message it never drew.
func TestMobileUserMessageEcho(t *testing.T) {
	d, sm := newMobileApp(t)
	s, err := sm.New("p", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	m, _ := mobileTestServer(t, d)
	c := newMobileConn(m, nil)
	c.out = make(chan []byte, 4)
	c.subscribe(s.ID)
	m.mu.Lock()
	m.conns[c] = struct{}{}
	m.mu.Unlock()

	d.echoUserMessage(s.ID, "你好", "你好", false, originDesktop)
	select {
	case b := <-c.out:
		var ev map[string]any
		_ = json.Unmarshal(b, &ev)
		if ev["type"] != mobileEvUserMessage || ev["origin"] != string(originDesktop) || ev["text"] != "你好" {
			t.Fatalf("event = %v", ev)
		}
	default:
		t.Fatal("no user_message was broadcast")
	}
}

// TestMobileSessionTitleBroadcasts: a generated title is a field of the list's row, so it leaves
// through the list's exit. The window hears it on its own event stream; the phone has only
// sessions_changed, and without this it kept calling the conversation 未命名会话 for good.
func TestMobileSessionTitleBroadcasts(t *testing.T) {
	d, sm := newMobileApp(t)
	s, err := sm.New("p", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	m, _ := mobileTestServer(t, d)
	// A connection that never subscribed: the list is not part of any conversation's stream.
	c := newMobileConn(m, nil)
	c.out = make(chan []byte, 4)
	m.mu.Lock()
	m.conns[c] = struct{}{}
	m.mu.Unlock()

	d.handleEvent(s.ID, agent.AgentEvent{Type: agent.AgentEventSessionTitle, Title: "新标题"})
	select {
	case b := <-c.out:
		var ev map[string]any
		_ = json.Unmarshal(b, &ev)
		if ev["type"] != mobileEvSessionsChanged {
			t.Fatalf("event = %v, want sessions_changed", ev)
		}
	default:
		t.Fatal("a generated title did not reach the phone")
	}
}

// TestMobileWebSocketProjects: the wire carries the projection (no Messages, no full history)
// and only the subscribed session's events.
func TestMobileWebSocketProjects(t *testing.T) {
	d, sm := newMobileApp(t)
	s, err := sm.New("p", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	writeRecords(t, s.ID, `{"type":"user","content":"hello","timestamp":"2026-10-19T10:00:00Z"}`)
	_, ts := mobileTestServer(t, d)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/v1/ws?token=secret", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"open","sessionId":"`+s.ID+`"}`)); err != nil {
		t.Fatalf("open: %v", err)
	}
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	var snap map[string]any
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatalf("snapshot json: %v", err)
	}
	if snap["type"] != mobileEvSnapshot {
		t.Fatalf("first frame = %v, want a snapshot", snap["type"])
	}
	if _, leaked := snap["event"]; leaked {
		t.Fatal("the frame carries a raw agent event")
	}

	// A text delta for this session arrives; one for another session does not.
	d.mobileNotify(s.ID, map[string]any{"type": mobileEvText, "text": "hi"})
	_, data, err = conn.Read(ctx)
	if err != nil {
		t.Fatalf("read text: %v", err)
	}
	var ev map[string]any
	_ = json.Unmarshal(data, &ev)
	if ev["type"] != mobileEvText || ev["text"] != "hi" {
		t.Fatalf("event = %v, want the text delta", ev)
	}

	d.mobileNotify("another-session", map[string]any{"type": mobileEvText, "text": "nope"})
	short, cancelShort := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancelShort()
	if _, data, err := conn.Read(short); err == nil {
		t.Fatalf("received an event for an unsubscribed session: %s", data)
	}
}

// TestMobileAttachmentOnlyNamesDeliveredFiles is invariant 9: the endpoint resolves an id
// against the session's own SendFile records and never takes a path.
func TestMobileAttachmentOnlyNamesDeliveredFiles(t *testing.T) {
	d, sm := newMobileApp(t)
	work := t.TempDir()
	s, err := sm.New("p", work)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	target := filepath.Join(work, "report.md")
	if err := os.WriteFile(target, []byte("# hi"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	writeRecords(t, s.ID, `{"type":"tool_call","name":"SendFile","tool_call_id":"call-1","args":{"path":"report.md"},"timestamp":"2026-10-19T10:00:00Z"}`)

	if _, _, _, status, code := d.mobileAttachment(s.ID, "call-1"); code != "" || status != 0 {
		t.Fatalf("delivered file = (%d,%s), want it resolved", status, code)
	}
	if _, _, _, status, code := d.mobileAttachment(s.ID, "../../etc/passwd"); code != "no_such_file" || status != http.StatusNotFound {
		t.Fatalf("path pretending to be an id = (%d,%s), want 404", status, code)
	}
	if _, _, _, status, code := d.mobileAttachment("other", "call-1"); code != "no_session" || status != http.StatusNotFound {
		t.Fatalf("another session's id = (%d,%s), want 404", status, code)
	}
}

// TestMobileTokenFileIsAPrivateCredential: the token lives in its own file with 0600, the same
// value is reused, and regenerating changes it. It must never land in config.yaml (it is a
// credential, not a preference) — that is why it has a file of its own at all.
func TestMobileTokenFileIsAPrivateCredential(t *testing.T) {
	config.SetBaseDir(t.TempDir())

	first, err := loadOrCreateMobileToken()
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if first == "" {
		t.Fatal("generated an empty token")
	}
	again, err := loadOrCreateMobileToken()
	if err != nil {
		t.Fatalf("read token: %v", err)
	}
	if again != first {
		t.Fatalf("token changed between reads: %q → %q", first, again)
	}
	st, err := os.Stat(mobileTokenPath())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Fatalf("token file mode = %o, want 600", got)
	}
	next, err := writeNewMobileToken()
	if err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	if next == first {
		t.Fatal("regenerating produced the same token")
	}
	if !mobileTokenEqual(next, next) || mobileTokenEqual(next, first) {
		t.Fatal("token comparison is not doing what it claims")
	}
}

// TestMobileStartStop: enabling the block really opens a listener, and stopping closes it.
func TestMobileStartStop(t *testing.T) {
	d, _ := newMobileApp(t)
	d.cfg.Mobile = config.MobileConfig{Enabled: true, Listen: "127.0.0.1:0"}
	d.startMobile(d.cfg)
	m := d.mobileServer()
	if m == nil || m.ln == nil {
		t.Fatalf("enabled config did not start a listener (server %v)", m)
	}
	addr := m.ln.Addr().String()
	res, err := http.Get("http://" + addr + "/v1/ping")
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("ping = %d, want 200", res.StatusCode)
	}
	d.stopMobile()
	if d.mobileServer() != nil {
		t.Fatal("stopMobile left the server registered")
	}
}

// TestPairingCandidatesAreDialable: the card, the QR and the phone all get exactly the addresses a
// phone could plausibly reach. The two ways that promise breaks are both real: the kernel hands back
// interfaces in an arbitrary order (a utun link, a VM bridge), and a container bridge (docker0) has a
// perfectly ordinary IPv4 that no phone can dial. An address that cannot answer is worse than none —
// the phone spends a probe on it and the reader has one more line to ignore.
func TestPairingCandidatesAreDialable(t *testing.T) {
	cand := func(name, ip string, up bool, p2p bool) lanCandidate {
		return lanCandidate{name: name, ip: net.ParseIP(ip), up: up, pointToPoint: p2p}
	}
	tests := []struct {
		name  string
		cands []lanCandidate
		want  []string
	}{
		{
			name: "物理网卡收，虚拟网桥不收",
			cands: []lanCandidate{
				cand("bridge100", "192.168.139.3", true, false),
				cand("vmenet0", "192.168.64.2", true, false),
				cand("en0", "10.5.178.101", true, false),
			},
			want: []string{"en0"},
		},
		{
			name: "容器网桥（docker0）不收 —— 它是这台机器内部的网络",
			cands: []lanCandidate{
				cand("docker0", "172.17.0.1", true, false),
				cand("eth0", "192.168.1.9", true, false),
			},
			want: []string{"eth0"},
		},
		{
			name:  "只有容器网桥时宁可为空（卡片会说没有可用地址）",
			cands: []lanCandidate{cand("br-2f1a", "172.18.0.1", true, false)},
			want:  nil,
		},
		{
			name: "VPN 的点对点地址不收",
			cands: []lanCandidate{
				cand("utun4", "10.251.1.1", true, true),
				cand("en0", "192.168.1.20", true, false),
			},
			want: []string{"en0"},
		},
		{
			name: "loopback 与链路本地地址不收",
			cands: []lanCandidate{
				cand("lo0", "127.0.0.1", true, false),
				cand("en8", "169.254.32.161", true, false),
			},
			want: nil,
		},
		{
			name:  "没起来了的接口不收",
			cands: []lanCandidate{cand("en0", "192.168.1.20", false, false)},
			want:  nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := make([]string, 0, len(tt.cands))
			for _, c := range tt.cands {
				if lanUsable(c) {
					got = append(got, c.name)
				}
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("可用的接口 = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestPairingOffersTheTailnet: an address from Tailscale's range is a candidate even though it
// arrives on a point-to-point utun — a phone on the same tailnet can reach it from anywhere, which
// is the whole reason this exists. Every OTHER tunnel still stays out: a company VPN's 10.x address
// is one no phone can dial, no matter how many of them the machine has.
func TestPairingOffersTheTailnet(t *testing.T) {
	cand := func(name, ip string, up bool, p2p bool) lanCandidate {
		return lanCandidate{name: name, ip: net.ParseIP(ip), up: up, pointToPoint: p2p}
	}
	tests := []struct {
		name string
		cand lanCandidate
		want bool
	}{
		{"tailnet 收（哪怕挂在点对点网卡上）", cand("utun10", "100.98.239.75", true, true), true},
		{"公司 VPN 的点对点地址不收", cand("utun4", "10.251.1.1", true, true), false},
		{"物理网卡照旧收", cand("en0", "192.168.71.183", true, false), true},
		{"tailnet 段之外的点对点地址不因为落在这个段附近就收", cand("utun6", "100.128.0.1", true, true), false},
		{"连着尾灯的 tailnet 地址照收", cand("tailscale0", "100.79.47.90", true, true), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := lanUsable(tt.cand); got != tt.want {
				t.Fatalf("lanUsable(%s %s, p2p=%v) = %v, want %v",
					tt.cand.name, tt.cand.ip, tt.cand.pointToPoint, got, tt.want)
			}
		})
	}
}

// TestPairingOrderPutsTheLanFirst: the card and the QR show one address first, and that one has to be
// the LAN. In the same house it answers in single-digit milliseconds and works with nothing else
// running; the tailnet is the fallback for everywhere else, and a container bridge is worth listing
// only after both — no phone can ever reach docker0.
func TestPairingOrderPutsTheLanFirst(t *testing.T) {
	mk := func(iface, kind string) MobilePairingCandidate {
		return MobilePairingCandidate{Interface: iface, Kind: kind, Address: iface, URL: "http://" + iface}
	}
	got := orderMobileCandidates([]MobilePairingCandidate{
		mk("utun10", pairingKindTailscale), // 内核可能先给隧道
		mk("docker0", pairingKindLAN),      // 虚拟网桥：手机永远连不到，排最后
		mk("en0", pairingKindLAN),
	})
	want := []string{"en0", "utun10", "docker0"}
	if len(got) != len(want) {
		t.Fatalf("排序后 %d 条, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Interface != want[i] {
			t.Fatalf("第 %d 条 = %s, want %s（顺序：%v）", i, got[i].Interface, want[i], got)
		}
	}
}

// TestPairingURLsCoverEveryAddress: the QR carries the whole set, the chosen one first and no
// duplicates — this is what teaches a phone that there is more than one way in, so losing an
// address out of it means the phone silently loses its fallback.
func TestPairingURLsCoverEveryAddress(t *testing.T) {
	cands := []MobilePairingCandidate{
		{Interface: "en0", Address: "192.168.1.20", Kind: pairingKindLAN, URL: "http://192.168.1.20:8787"},
		{Interface: "utun10", Address: "100.98.239.75", Kind: pairingKindTailscale, URL: "http://100.98.239.75:8787"},
		{Interface: "en7", Address: "10.0.0.5", Kind: pairingKindLAN, URL: "http://192.168.1.20:8787"}, // 同一个地址两个网卡
	}
	got := mobilePairingURLs(cands, "http://100.98.239.75:8787")
	want := []string{"http://100.98.239.75:8787", "http://192.168.1.20:8787"}
	if len(got) != len(want) {
		t.Fatalf("urls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("urls = %v, want %v", got, want)
		}
	}
	// 没有选择的时候就是候选自己的顺序（内网在前）。
	if auto := mobilePairingURLs(cands, ""); len(auto) == 0 || auto[0] != "http://192.168.1.20:8787" {
		t.Fatalf("自动顺序的 urls = %v, want 内网地址在前", auto)
	}
}

// TestPairingPayloadCarriesEveryURL: one scan has to be enough. The payload keeps "v":1 and "url"
// (an older phone build reads those and stops), and adds "urls" — the set the current build races.
func TestPairingPayloadCarriesEveryURL(t *testing.T) {
	config.SetBaseDir(t.TempDir())
	vo := (&AgentService{desk: newTestApp()}).GetMobilePairing()
	if len(vo.Candidates) == 0 {
		t.Skip("这台机器没有可用的地址")
	}
	var payload struct {
		V     int      `json:"v"`
		URL   string   `json:"url"`
		URLs  []string `json:"urls"`
		Token string   `json:"token"`
	}
	if err := json.Unmarshal([]byte(vo.Payload), &payload); err != nil {
		t.Fatalf("payload 不是 JSON: %v", err)
	}
	if payload.V != 1 || payload.URL != vo.URL || payload.Token == "" {
		t.Fatalf("payload = %+v, want v=1 / url=%s / token 非空", payload, vo.URL)
	}
	if len(payload.URLs) != len(vo.URLs) || len(payload.URLs) == 0 {
		t.Fatalf("payload.urls = %v, want %v", payload.URLs, vo.URLs)
	}
	for i := range vo.URLs {
		if payload.URLs[i] != vo.URLs[i] {
			t.Fatalf("payload.urls = %v, want %v", payload.URLs, vo.URLs)
		}
	}
}

// TestMobilePairingHostIsChosenAndKept: the card's address selector is a real choice, it survives
// a token regeneration, and an interface that is not on this machine is refused rather than
// written (a URL pointing at nothing is worse than the automatic one).
func TestMobilePairingHostIsChosenAndKept(t *testing.T) {
	config.SetBaseDir(t.TempDir())
	d := newTestApp()

	cands := mobileLANCandidates("8787")
	if len(cands) == 0 {
		t.Skip("这台机器没有可用的 LAN 地址")
	}
	want := cands[0]
	if got := d.SetMobilePairingHostForTest(want.Interface); got != "ok" {
		t.Fatalf("选了 %s = %q", want.Interface, got)
	}
	pair := d.pairing()
	if pair.Interface != want.Interface || pair.URL != want.URL || pair.Host != want.Address {
		t.Fatalf("配对地址 = %+v, want %s / %s", pair, want.Interface, want.URL)
	}

	// A bogus interface is refused and nothing moves.
	if got := d.SetMobilePairingHostForTest("not-an-interface"); got == "ok" {
		t.Fatal("不存在的接口被接受了")
	}
	if again := d.pairing(); again.Interface != want.Interface {
		t.Fatalf("被拒绝的修改把选择改掉了：%q", again.Interface)
	}

	// Regenerating the token keeps the choice; the token itself changes.
	before := pair.Token
	if tok, err := writeNewMobileToken(); err != nil || tok == before {
		t.Fatalf("regenerate token: %v (same=%v)", err, tok == before)
	}
	if kept := d.pairing(); kept.Interface != want.Interface {
		t.Fatalf("换令牌之后接口选择丢了：%q", kept.Interface)
	}
}

// SetMobilePairingHostForTest is the binding's core without the Wails receiver, for tests in
// this package (the binding itself takes *AgentService).
func (d *desktopApp) SetMobilePairingHostForTest(iface string) string {
	return (&AgentService{desk: d}).SetMobilePairingHost(iface)
}

// pairing is GetMobilePairing's core, on the app rather than the service.
func (d *desktopApp) pairing() MobilePairingVO {
	return (&AgentService{desk: d}).GetMobilePairing()
}

// TestChooseMobileCandidate pins the fallback rule: a stale choice (the interface lost its
// address, or the machine was renamed) must degrade to the automatic one, never to an empty URL.
func TestChooseMobileCandidate(t *testing.T) {
	a := MobilePairingCandidate{Interface: "en0", Address: "10.0.0.5", URL: "http://10.0.0.5:8787"}
	b := MobilePairingCandidate{Interface: "bridge100", Address: "192.168.64.1", URL: "http://192.168.64.1:8787"}
	if got, ok := chooseMobileCandidate([]MobilePairingCandidate{a, b}, "bridge100"); !ok || got.Interface != "bridge100" {
		t.Fatalf("选 bridge100 = %+v", got)
	}
	if got, ok := chooseMobileCandidate([]MobilePairingCandidate{a, b}, "gone0"); !ok || got.Interface != "en0" {
		t.Fatalf("过期选择应退回自动（列表第一个）：%+v", got)
	}
	if _, ok := chooseMobileCandidate(nil, ""); ok {
		t.Fatal("没有候选时不应给出地址")
	}
}

// TestMobileUsageFromRows is issue #51/#53's arithmetic: cost and credit are summed off the
// ledger (not off a live run), and the cached share is the LAST call's reads over miss+read.
func TestMobileUsageFromRows(t *testing.T) {
	now := time.Now()
	rows := []llm.UsageRow{
		{TS: now, SessionID: "s", InputPrice: 2, OutputPrice: 8, CacheReadPrice: 0.2,
			InputTokens: 100, OutputTokens: 10, CacheReadInputTokens: 900},
		{TS: now, SessionID: "s", InputPrice: 2, OutputPrice: 8, CacheReadPrice: 0.2,
			InputTokens: 200, OutputTokens: 20, CacheReadInputTokens: 200},
	}
	u := mobileUsageFromRows(rows, &config.Config{})
	want := rows[0].Cost() + rows[1].Cost()
	if u.Cost < want-1e-9 || u.Cost > want+1e-9 {
		t.Fatalf("cost = %v, want %v (the sum of the ledger rows)", u.Cost, want)
	}
	// 200 reads out of 400 prompt tokens on the last call.
	if u.CacheHitRate < 0.5-1e-9 || u.CacheHitRate > 0.5+1e-9 {
		t.Fatalf("cache hit rate = %v, want 0.5 (the last call only)", u.CacheHitRate)
	}
	if empty := mobileUsageFromRows(nil, &config.Config{}); empty.Cost != 0 || empty.CacheHitRate != 0 {
		t.Fatalf("no rows = %+v, want zeros", empty)
	}
}

// TestMobileContextFallsBackToRecords: a session nothing has opened in this process still reports
// its context size (the last recorded call) and its model's window (from config) — issue #53.
func TestMobileContextFallsBackToRecords(t *testing.T) {
	d, sm := newMobileApp(t)
	d.cfg.Providers = []config.ProviderConfig{{
		Name: "p", Model: "claude-sonnet-4-5", Type: config.ProviderTypeAnthropic,
	}}
	d.cfg.Provider = "p"
	s, err := sm.New("p", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	writeRecords(t, s.ID,
		`{"type":"user","content":"hi","timestamp":"2026-10-19T10:00:00Z"}`,
		`{"type":"assistant","content":"hello","timestamp":"2026-10-19T10:00:01Z",`+
			`"usage":{"input_tokens":1200,"cache_read_input_tokens":300,"estimated_input_tokens":400}}`,
	)

	snap, err := d.mobileSnapshot(s.ID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.Context.Estimate != 1500 {
		t.Fatalf("context estimate = %d, want the recorded prompt size 1500", snap.Context.Estimate)
	}
	if snap.Context.Window <= 0 {
		t.Fatal("context window = 0; a session with no live agent still knows its model's window")
	}
}

// TestMobileCreateDoesNotMoveTheDeskView: creating a conversation from the phone is invariant 7 —
// the window keeps showing what it was showing, and the new row exists for both screens.
func TestMobileCreateDoesNotMoveTheDeskView(t *testing.T) {
	d, sm := newMobileApp(t)
	shown, err := sm.New("p", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	d.mu.Lock()
	d.activeID = shown.ID
	d.mu.Unlock()

	dir := t.TempDir()
	row, status, code := d.mobileCreateSession("", dir)
	if code != "" {
		t.Fatalf("create = %d %s", status, code)
	}
	if row.ID == shown.ID || row.ID == "" {
		t.Fatalf("created row = %+v", row)
	}
	d.mu.Lock()
	got := d.activeID
	d.mu.Unlock()
	if got != shown.ID {
		t.Fatalf("activeID = %q, want the displayed session %q untouched", got, shown.ID)
	}
	if rec := d.sessionRecord(row.ID); rec == nil || rec.WorkingDir != dir {
		t.Fatalf("created session's working dir = %v, want %q", rec, dir)
	}
}

// TestMobileCreateExpandsHomePath is the "~ is the COMPUTER's home directory" rule, without
// touching a real home: a path under a temp dir round-trips unchanged, and "~" expands through
// the same helper the desktop's own new-session path uses.
func TestMobileCreateExpandsHomePath(t *testing.T) {
	expanded, err := expandRootPath("~/workspace")
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	home, _ := os.UserHomeDir()
	if !filepath.IsAbs(expanded) || !strings.HasPrefix(expanded, home) {
		t.Fatalf("~/workspace = %q, want an absolute path under %q", expanded, home)
	}
	if strings.HasPrefix(expanded, "~") {
		t.Fatalf("~/workspace kept the tilde: %q", expanded)
	}
}

// TestMobilePinPersists: the pin is a record field, re-read by both lists, so the phone's swipe
// and the sidebar's 置顶 are the same write.
func TestMobilePinPersists(t *testing.T) {
	d, sm := newMobileApp(t)
	s, err := sm.New("p", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if status, code := d.mobileSetPinned(s.ID, true); code != "" {
		t.Fatalf("pin = %d %s", status, code)
	}
	rows, err := d.mobileSessions()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || !rows[0].Pinned {
		t.Fatalf("rows = %+v, want the single row pinned", rows)
	}
	if status, code := d.mobileSetPinned("nope", true); code != "no_session" {
		t.Fatalf("pin on an unknown session = %d %s", status, code)
	}
}

// TestMobileRenameBroadcastsTheList: a title changed on the machine reaches the phone as a list
// event (issue #57). Without it the row kept the old name until the conversation was reopened.
func TestMobileRenameBroadcastsTheList(t *testing.T) {
	d, sm := newMobileApp(t)
	s, err := sm.New("p", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	m, _ := mobileTestServer(t, d)
	c := newMobileConn(m, nil)
	c.out = make(chan []byte, 4)
	m.mu.Lock()
	m.conns[c] = struct{}{}
	m.mu.Unlock()

	if res := (&AgentService{desk: d}).RenameSession(s.ID, "新名字"); res != "ok" {
		t.Fatalf("rename = %q", res)
	}
	select {
	case b := <-c.out:
		var ev map[string]any
		_ = json.Unmarshal(b, &ev)
		if ev["type"] != mobileEvSessionsChanged {
			t.Fatalf("event = %v, want sessions_changed", ev)
		}
	default:
		t.Fatal("rename broadcast nothing; the phone would keep the old title")
	}
	if rec := d.sessionRecord(s.ID); rec == nil || rec.Title != "新名字" {
		t.Fatalf("stored title = %v", rec)
	}
}

// TestMobilePanelsAreAddressedBySession: the three panels answer for the conversation the phone
// named, and every array is `[]` rather than null — the client uses them as arrays, and a null
// there crashes a render instead of failing a type check (protocol: 数组空的时候是 []).
func TestMobilePanelsAreAddressedBySession(t *testing.T) {
	d, sm := newMobileApp(t)
	s, err := sm.New("p", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	_, ts := mobileTestServer(t, d)

	code, body := mobileCall(t, ts, "GET", "/v1/sessions/"+s.ID+"/panels", "secret", nil)
	if code != http.StatusOK {
		t.Fatalf("panels = %d, want 200", code)
	}
	panels, _ := body["panels"].(map[string]any)
	if panels == nil {
		t.Fatalf("body = %v, want a panels object", body)
	}
	for _, key := range []string{"mcp", "cron"} {
		if _, ok := panels[key].([]any); !ok {
			t.Fatalf("%s = %v, want an array (never null)", key, panels[key])
		}
	}
	agents, _ := panels["agents"].(map[string]any)
	if agents == nil {
		t.Fatalf("agents = %v, want an object", panels["agents"])
	}
	if _, ok := agents["peers"].([]any); !ok {
		t.Fatalf("agents.peers = %v, want an array", agents["peers"])
	}

	if code, _ := mobileCall(t, ts, "GET", "/v1/sessions/nope/panels", "secret", nil); code != http.StatusNotFound {
		t.Fatalf("panels for an unknown session = %d, want 404", code)
	}
}

// TestMobileCronChangedReachesThePhone: a job written on EITHER screen has to reach the other one
// (双端同步). The window has its own event; the phone's only channel is this broadcast, and it is
// emitted from the one place every cron write funnels through (emitCronChanged).
func TestMobileCronChangedReachesThePhone(t *testing.T) {
	d, _ := newMobileApp(t)
	m, _ := mobileTestServer(t, d)
	c := newMobileConn(m, nil)
	c.out = make(chan []byte, 4)
	m.mu.Lock()
	m.conns[c] = struct{}{}
	m.mu.Unlock()

	d.emitCronChanged("job-1")
	select {
	case b := <-c.out:
		var ev map[string]any
		if err := json.Unmarshal(b, &ev); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if ev["type"] != mobileEvCronChanged || ev["id"] != "job-1" {
			t.Fatalf("event = %v, want cron_changed for job-1", ev)
		}
	default:
		t.Fatal("emitCronChanged broadcast nothing; the phone's panel would stay stale")
	}
}

// TestMobileCronRefusalsKeepTheirCode: the three cron actions answer the protocol's table, not a
// generic failure — the phone shows a different sentence for each.
func TestMobileCronRefusalsKeepTheirCode(t *testing.T) {
	cases := []struct {
		res    string
		status int
		code   string
	}{
		{"ok", 0, ""},
		{"任务不存在", http.StatusNotFound, "no_job"},
		{"定时任务已在设置里关闭", http.StatusConflict, "cron_disabled"},
		{"这个任务不属于 desktop", http.StatusConflict, "not_desktop"},
		{"写盘失败了", http.StatusInternalServerError, "cron_failed"},
	}
	for _, c := range cases {
		status, code := mobileCronResult(c.res)
		if status != c.status || code != c.code {
			t.Fatalf("%q → (%d, %q), want (%d, %q)", c.res, status, code, c.status, c.code)
		}
	}
}

// TestMobileUnsubscribeOnEmptyOpen: sessionId 为空串 = 退订（「我回列表了」）。
//
// 不回快照、也不再收到那段会话的事件。缺了它，离开的对话在跑的时候仍然每秒推几十条
// 增量过来 —— 手机为一个没人看的画面重画，列表上划一下就卡。
func TestMobileUnsubscribeOnEmptyOpen(t *testing.T) {
	d, sm := newMobileApp(t)
	s, err := sm.New("p", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	_, ts := mobileTestServer(t, d)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/v1/ws?token=secret", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"open","sessionId":"`+s.ID+`"}`)); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}

	// 退订：既不该回快照，也不该再把这段会话的事件推过来
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"open","sessionId":""}`)); err != nil {
		t.Fatalf("unsubscribe: %v", err)
	}
	// 一条 ping 当栅栏：两条命令在同一条连接上是按序处理的，pong 回来就说明退订已经生效
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"ping"}`)); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if _, data, err := conn.Read(ctx); err != nil {
		t.Fatalf("read pong: %v", err)
	} else if !strings.Contains(string(data), "pong") {
		t.Fatalf("栅栏应答不是 pong：%s", data)
	}
	d.mobileNotify(s.ID, map[string]any{"type": mobileEvText, "text": "should not arrive"})
	short, cancelShort := context.WithTimeout(ctx, 400*time.Millisecond)
	defer cancelShort()
	if _, data, err := conn.Read(short); err == nil {
		t.Fatalf("退订之后还收到了东西: %s", data)
	}

	// 退订只影响那一段会话的流，广播照样到。换一条连接验：读超时之后 coder/websocket
	// 认为这条连接已经不可用了，在同一条上继续读会是「network connection closed」。
	conn2, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/v1/ws?token=secret", nil)
	if err != nil {
		t.Fatalf("dial 2: %v", err)
	}
	defer conn2.CloseNow()
	d.mobileBroadcast(map[string]any{"type": mobileEvSessionsChanged})
	if _, data, err := conn2.Read(ctx); err != nil {
		t.Fatalf("read broadcast: %v", err)
	} else if !strings.Contains(string(data), mobileEvSessionsChanged) {
		t.Fatalf("广播没到：%s", data)
	}
}

// TestMobileAskWireKinds: 待答项在线上只有两种词 —— `permission` 与 `question`。
//
// 内部那张表把它们叫 `permission` / `ask_user`，中间那一个词漏到线上过一次：手机认的是
// `question`，于是真实桌面发来的提问永远落不到卡片那条分支上（mock 的种子数据写的是
// `question`，所以只有它在的时候能看到卡片）。这类不一致不会报错，只会让一条分支永远不成立。
func TestMobileAskWireKinds(t *testing.T) {
	perm := mobileAskOf(&pendingAsk{kind: askKindPermission, toolID: "t1", toolName: "Bash"})
	if perm.Kind != "permission" {
		t.Errorf("权限那张卡的 kind = %q，线上应当是 permission", perm.Kind)
	}
	q := mobileAskOf(&pendingAsk{
		kind: askKindAskUser, toolID: "t2",
		questions: []tools.Question{{Question: "放哪一层？", Options: []tools.QuestionOption{{Label: "甲"}}}},
	})
	if q.Kind != "question" {
		t.Errorf("提问那张卡的 kind = %q，线上应当是 question", q.Kind)
	}
	if len(q.Questions) != 1 || q.Questions[0].Question != "放哪一层？" || len(q.Questions[0].Options) != 1 {
		t.Errorf("提问的内容没传到线上：%+v", q.Questions)
	}
}

// TestMobileUsageIsMemoized: 打开一段会话时，用量是这次读取里最贵的一步（账本按天分文件，从会话
// 建立那天开始 SUM），而手机连点三段对话时那个数字根本没变。所以它被记 TTL 秒 —— 但**过期的缓存
// 必须重算**，而且读失败**不缓存**（否则接下来五秒都在报 0）。
func TestMobileUsageIsMemoized(t *testing.T) {
	d, sm := newMobileApp(t)
	s, err := sm.New("p", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	// 记一个看得见的值：能读到它，就说明没有去扫账本（这个测试的 HOME 里没有账本）。
	d.usageCache.put(s.ID, mobileUsage{Cost: 1.23})
	if got := d.mobileUsage(s.ID); got.Cost != 1.23 {
		t.Fatalf("cost = %v, want the cached 1.23", got.Cost)
	}

	// 过期的条目要重算：没有账本时答案回到 0，那就是「真的又去读了」的证据。
	d.usageCache.mu.Lock()
	d.usageCache.at[s.ID] = time.Now().Add(-2 * mobileUsageTTL)
	d.usageCache.mu.Unlock()
	if got := d.mobileUsage(s.ID); got.Cost != 0 {
		t.Fatalf("cost = %v after the entry expired, want a fresh read (0 here)", got.Cost)
	}
}

// TestMobileGzip: the phone's link is not always good, and the session list is the one response big
// enough for that to matter. Compression has to apply to JSON, and has to stay away from the
// WebSocket upgrade (which needs the raw connection) and from file downloads (already compressed,
// and they may be range requests).
func TestMobileGzip(t *testing.T) {
	body := strings.Repeat(`{"t":"一个不短的中文标题"}`, 200)
	h := mobileGzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))

	read := func(path string, gzipOK bool) (*httptest.ResponseRecorder, string) {
		req := httptest.NewRequest("GET", path, nil)
		if gzipOK {
			req.Header.Set("Accept-Encoding", "gzip")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec, rec.Header().Get("Content-Encoding")
	}

	rec, enc := read("/v1/sessions", true)
	if enc != "gzip" {
		t.Fatalf("JSON 没被压缩: Content-Encoding = %q", enc)
	}
	if rec.Body.Len() >= len(body) {
		t.Fatalf("压缩后 %d 字节，原样 %d —— 一点没小", rec.Body.Len(), len(body))
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("不是合法的 gzip: %v", err)
	}
	got, err := io.ReadAll(zr)
	if err != nil || string(got) != body {
		t.Fatalf("解开之后和原文不一样: err=%v 长度=%d/%d", err, len(got), len(body))
	}

	// 客户端没说要压缩：原样发，别让对方解不开
	if _, enc := read("/v1/sessions", false); enc == "gzip" {
		t.Fatal("客户端没接受 gzip，却压缩了")
	}
	// WebSocket 升级要裸连接；文件下载是已经压过的媒体，还可能有 Range
	if _, enc := read("/v1/ws", true); enc == "gzip" {
		t.Fatal("WebSocket 端点被压缩了，握手会坏")
	}
	if _, enc := read("/v1/sessions/x/files/y", true); enc == "gzip" {
		t.Fatal("文件下载被压缩了")
	}
}
