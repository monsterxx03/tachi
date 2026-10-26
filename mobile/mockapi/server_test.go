package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// 这几个测试钉的是**协议**（docs/agents/mobile-protocol.md），不是这个假实现。
// 桌面端实现那份接口时，这些断言同样该成立。

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(New("test-token").Handler())
	t.Cleanup(ts.Close)
	return ts
}

func get(t *testing.T, ts *httptest.Server, path, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestPingDoesNotNeedToken(t *testing.T) {
	ts := newTestServer(t)
	// 配对页就是拿这个端点验证地址与令牌的，所以它自己不能先要令牌
	resp := get(t, ts, "/v1/ping", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ping 应当不要令牌，得到 %d", resp.StatusCode)
	}
	defer resp.Body.Close()
	// hostname 是手机端列表里的默认名字；假服务端也要给，否则那条 UI 在 mock 下永远走兜底
	// 分支，测不到
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode ping: %v", err)
	}
	if h, _ := body["hostname"].(string); h == "" {
		t.Fatalf("ping 没有 hostname：%v", body)
	}
}

func TestEverythingElseNeedsToken(t *testing.T) {
	ts := newTestServer(t)
	for _, path := range []string{"/v1/sessions", "/v1/providers", "/v1/awake", "/v1/sessions/s1/page"} {
		if got := get(t, ts, path, "").StatusCode; got != http.StatusUnauthorized {
			t.Errorf("%s 缺令牌应当是 401，得到 %d", path, got)
		}
		if got := get(t, ts, path, "wrong-token").StatusCode; got != http.StatusUnauthorized {
			t.Errorf("%s 错令牌应当是 401，得到 %d", path, got)
		}
		if got := get(t, ts, path, "test-token").StatusCode; got != http.StatusOK {
			t.Errorf("%s 带对令牌应当是 200，得到 %d", path, got)
		}
	}
}

// TestAwakeIsOneSwitchForTheWholeMachine: 保持唤醒是一台机器的一个值，不属于任何会话 —— 读一次、
// 改一次，改完广播给每一条连接。手机卡片和桌面托盘看的就是这一个值。
func TestAwakeIsOneSwitchForTheWholeMachine(t *testing.T) {
	ts := newTestServer(t)
	conn := dialWS(t, ts, "test-token")

	resp := get(t, ts, "/v1/awake", "test-token")
	var read struct {
		Awake AwakeState `json:"awake"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&read); err != nil {
		t.Fatalf("decode awake: %v", err)
	}
	if read.Awake.On || read.Awake.DurationMin != mockAwakeDefaultMinutes {
		t.Fatalf("默认应当是关着、偏好 %d 分钟，得到 %+v", mockAwakeDefaultMinutes, read.Awake)
	}

	// 越界的时长是调用方的错（400），不是「那台机器拒绝」，两者在手机上说的不是一句话
	if code := postJSON(t, ts, "/v1/awake", map[string]any{"on": true, "durationMin": 1}); code != http.StatusBadRequest {
		t.Fatalf("1 分钟的时长应当 400，得到 %d", code)
	}

	if code := postJSON(t, ts, "/v1/awake", map[string]any{"on": true, "durationMin": 45}); code != http.StatusOK {
		t.Fatalf("开启应当 200，得到 %d", code)
	}
	ev := readEvent(t, conn)
	if ev.Type != "awake_changed" || ev.Awake == nil {
		t.Fatalf("开启之后应当广播 awake_changed，得到 %+v", ev)
	}
	if !ev.Awake.On || ev.Awake.DurationMin != 45 || ev.Awake.Until == "" {
		t.Fatalf("广播的状态应当是开着、45 分钟、带到期时刻，得到 %+v", ev.Awake)
	}

	resp = get(t, ts, "/v1/awake", "test-token")
	if err := json.NewDecoder(resp.Body).Decode(&read); err != nil {
		t.Fatalf("decode awake: %v", err)
	}
	if !read.Awake.On || read.Awake.DurationMin != 45 {
		t.Fatalf("重新读应当是改后的状态，得到 %+v", read.Awake)
	}
}

func postJSON(t *testing.T, ts *httptest.Server, path string, body any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp.StatusCode
}

func TestNoSessionForUnknownID(t *testing.T) {
	ts := newTestServer(t)
	// 会话不存在给 404 + no_session，手机据此说「这段对话已经不在了」
	resp := get(t, ts, "/v1/sessions/nope/page", "test-token")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("未知会话应当 404，得到 %d", resp.StatusCode)
	}
	var body map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["error"] != "no_session" {
		t.Fatalf("错误码应当是 no_session，得到 %q", body["error"])
	}
}

func dialWS(t *testing.T, ts *httptest.Server, token string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/ws?token=" + token
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func readEvent(t *testing.T, conn *websocket.Conn) Event {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var ev Event
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

func TestOpenSessionGetsSnapshot(t *testing.T) {
	ts := newTestServer(t)
	conn := dialWS(t, ts, "test-token")

	if err := conn.WriteJSON(map[string]string{"type": "open", "sessionId": "s1"}); err != nil {
		t.Fatal(err)
	}
	ev := readEvent(t, conn)
	if ev.Type != "snapshot" {
		t.Fatalf("open 之后第一条应当是 snapshot，得到 %q", ev.Type)
	}
	if ev.Snapshot.Session.ID != "s1" {
		t.Fatalf("快照应当是 s1，得到 %q", ev.Snapshot.Session.ID)
	}
	if len(ev.Snapshot.Messages) == 0 {
		t.Fatal("快照要带历史")
	}
	if len(ev.Snapshot.Asks) == 0 {
		t.Fatal("s1 有一张待答的权限卡，快照要带上")
	}
}

func TestSendMessageStreamsAndEnds(t *testing.T) {
	ts := newTestServer(t)
	conn := dialWS(t, ts, "test-token")
	if err := conn.WriteJSON(map[string]string{"type": "open", "sessionId": "s3"}); err != nil {
		t.Fatal(err)
	}
	if ev := readEvent(t, conn); ev.Type != "snapshot" {
		t.Fatalf("第一条应当是 snapshot，得到 %q", ev.Type)
	}

	body := strings.NewReader(`{"text":"帮我看一下这段代码","mode":"send"}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/sessions/s3/messages", body)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("发消息应当受理，得到 %d", resp.StatusCode)
	}

	// 事件流：回声 → 若干 text → 最后 idle（手机据此重读快照）
	var sawEcho, sawText, sawIdle bool
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && !sawIdle {
		ev := readEvent(t, conn)
		switch ev.Type {
		case "user_message":
			if ev.Origin != "mobile" {
				t.Errorf("手机自己发的消息 origin 应当是 mobile，得到 %q", ev.Origin)
			}
			sawEcho = true
		case "text":
			sawText = true
		case "idle":
			sawIdle = true
		}
	}
	if !sawEcho {
		t.Error("应当收到自己那条消息的回声")
	}
	if !sawText {
		t.Error("应当收到增量文字")
	}
	if !sawIdle {
		t.Error("一轮结束应当以 idle 收尾")
	}
}

func TestPageCursorIsIndexNotTime(t *testing.T) {
	ts := newTestServer(t)
	// before 是「记录序号」，返回严格小于它的那些：同一时刻的多条也不会漏
	resp := get(t, ts, "/v1/sessions/s1/page?before=3&limit=10", "test-token")
	var body struct {
		Messages []Message `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	for _, m := range body.Messages {
		if m.Index >= 3 {
			t.Fatalf("before=3 不该返回 index=%d", m.Index)
		}
	}
	if len(body.Messages) == 0 {
		t.Fatal("应当返回 0..2 这三条")
	}
}

// TestCompactionMovesThePhone: 压缩成功会把这轮对话搬进子会话，尾巴与 idle 都发往新 id。
// 手机必须收到 session_moved 并换订阅 —— 停在旧 id 上就永远等不到那一轮的结束。
func TestCompactionMovesThePhone(t *testing.T) {
	ts := newTestServer(t)
	conn := dialWS(t, ts, "test-token")
	if err := conn.WriteJSON(map[string]string{"type": "open", "sessionId": "s3"}); err != nil {
		t.Fatal(err)
	}
	if ev := readEvent(t, conn); ev.Type != "snapshot" {
		t.Fatalf("第一条应当是 snapshot，得到 %q", ev.Type)
	}

	body := strings.NewReader(`{"text":"这段太长了，压缩一下","mode":"send"}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/sessions/s3/messages", body)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("发消息应当受理，得到 %d", resp.StatusCode)
	}

	var moved *Event
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && moved == nil {
		ev := readEvent(t, conn)
		if ev.Type == "session_moved" {
			moved = &ev
		}
	}
	if moved == nil {
		t.Fatal("压缩之后没有收到 session_moved：手机永远停在被压缩掉的那一半上")
	}
	if moved.From != "s3" || moved.To == "" || moved.To == moved.From {
		t.Fatalf("session_moved = %+v，应当从一个 id 指向一个新 id", *moved)
	}
	if moved.End != "complete" {
		t.Fatalf("session_moved.end = %q，压缩是这一轮正常结束之后的事", moved.End)
	}

	// 换订阅：新会话的快照先到（压缩摘要在那里面），然后尾巴与 idle
	if err := conn.WriteJSON(map[string]string{"type": "open", "sessionId": moved.To}); err != nil {
		t.Fatal(err)
	}
	// 广播（sessions_changed）可能插在前面，跳过它
	var snap Event
	for snap.Type != "snapshot" {
		snap = readEvent(t, conn)
	}
	if snap.Snapshot == nil || snap.Snapshot.Session.ID != moved.To {
		t.Fatalf("换订阅后应当收到子会话的快照，得到 %+v", snap)
	}
	// 换过来的会话必须已经是空闲的：换轨发生在一轮结束之后，手机要靠这一点补发队列
	// （idle 在换订阅落地之前就发走了，等不到 —— 这正是 session_moved 要带 end 的原因）
	if snap.Snapshot.Session.Running {
		t.Fatal("换轨时的子会话还在跑：手机补发队列会被 turn_running 挡下")
	}
}
