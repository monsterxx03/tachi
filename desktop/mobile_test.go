package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/monsterxx03/tachi/config"
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

	if code, _ := mobileCall(t, ts, "GET", "/v1/ping", "", nil); code != http.StatusOK {
		t.Fatalf("ping without token = %d, want 200", code)
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

	if code, got := d.mobileSend("nope", "hi", "send"); code != http.StatusNotFound || got != "no_session" {
		t.Fatalf("unknown session = (%d,%s), want (404,no_session)", code, got)
	}
	if code, got := d.mobileSend(s.ID, "hi", "steer"); code != http.StatusConflict || got != "not_running" {
		t.Fatalf("steer while idle = (%d,%s), want (409,not_running)", code, got)
	}

	// A turn in flight with no steer channel is a SCHEDULED turn: it says so rather than
	// claiming nothing is running.
	d.mu.Lock()
	r := d.getRun(s.ID)
	r.running = true
	d.mu.Unlock()
	if code, got := d.mobileSend(s.ID, "hi", "steer"); code != http.StatusConflict || got != "steer_unsupported" {
		t.Fatalf("steer on a scheduled turn = (%d,%s), want (409,steer_unsupported)", code, got)
	}
	if code, got := d.mobileSend(s.ID, "hi", "send"); code != http.StatusConflict || got != "turn_running" {
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

	d.echoUserMessage(s.ID, "你好", false, originDesktop)
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

// TestPairingPicksALANAddress: the QR's URL has to be an address a phone can dial. On a machine
// with a VPN and a container network, "first non-loopback IPv4" is whichever interface the kernel
// happened to list first — which is exactly how the card came to show 10.251.1.1 (a utun link)
// or 192.168.139.3 (a vm bridge) on a machine whose Wi-Fi was en0.
func TestPairingPicksALANAddress(t *testing.T) {
	cand := func(name, ip string, up bool, p2p bool) lanCandidate {
		return lanCandidate{name: name, ip: net.ParseIP(ip), up: up, pointToPoint: p2p}
	}
	tests := []struct {
		name  string
		cands []lanCandidate
		want  string
	}{
		{
			name: "虚拟网桥排在前面也不选它",
			cands: []lanCandidate{
				cand("bridge100", "192.168.139.3", true, false),
				cand("vmenet0", "192.168.64.2", true, false),
				cand("en0", "10.5.178.101", true, false),
			},
			want: "10.5.178.101",
		},
		{
			name: "VPN 的点对点地址不选",
			cands: []lanCandidate{
				cand("utun4", "10.251.1.1", true, true),
				cand("en0", "192.168.1.20", true, false),
			},
			want: "192.168.1.20",
		},
		{
			name:  "只有虚拟网卡时退而求其次（总比空着强）",
			cands: []lanCandidate{cand("bridge100", "192.168.139.3", true, false)},
			want:  "192.168.139.3",
		},
		{
			name: "只有 loopback 与链路本地地址时给空（卡片会说没有可用地址）",
			cands: []lanCandidate{
				cand("lo0", "127.0.0.1", true, false),
				cand("en8", "169.254.32.161", true, false),
			},
			want: "",
		},
		{
			name:  "Linux 的 eth0 也算物理网卡",
			cands: []lanCandidate{cand("docker0", "172.17.0.1", true, false), cand("eth0", "192.168.1.9", true, false)},
			want:  "192.168.1.9",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pickLANIPv4(tt.cands); got != tt.want {
				t.Fatalf("pickLANIPv4 = %q, want %q", got, tt.want)
			}
		})
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
