package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// mobileAPIRoundTrip drives the phone interface of the RUNNING app: the same calls the phone
// makes, in the order it makes them.
//
// It exists because nothing else can see this seam. The unit tests in desktop/ hold the handlers
// against a fixture; the phone's own tests hold the client against mockapi. What neither reaches
// is the pair of them around a REAL turn — a message from the phone that the model is actually
// asked about, a Bash command that really parks on the permission card, an answer that arrives
// over HTTP and is handed to the waiting agent goroutine, and the events the phone is subscribed
// to. All of that only exists while the process is alive, which is why it runs in `live`.
func mobileAPIRoundTrip(c *liveCtx) {
	const phoneText = "手机联调：跑一下 echo"

	client := &http.Client{Timeout: 10 * time.Second}
	base := "http://" + c.mobileAddr

	// ── the credential ─────────────────────────────────────────────────────
	// The token is a file, not a config leaf (desktop/mobile_token.go): the scenario reads it the
	// way the settings card hands it to a phone.
	token, ok := mobileSmokeToken(c)
	if !ok {
		return
	}
	auth := func(req *http.Request) *http.Request {
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		return req
	}

	// ── the listener ───────────────────────────────────────────────────────
	// Started with the process, bound to the port the runner picked. Waiting on /v1/ping is
	// waiting on the one thing this scenario needs; a failure here names the address.
	up := false
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		res, err := client.Get(base + "/v1/ping")
		if err == nil {
			res.Body.Close()
			up = res.StatusCode == http.StatusOK
			if up {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !up {
		c.check("移动接口在进程里起来了", false, c.mobileAddr+" 没有应答 /v1/ping")
		return
	}
	c.check("移动接口在进程里起来了", true, c.mobileAddr)

	// ── the list ───────────────────────────────────────────────────────────
	// One row per conversation, and this sandbox has exactly the fixture's. The id comes from
	// here rather than from the fixture file: it is the server's own answer, which is what the
	// phone would hold.
	type summary = mobileSessionRow
	req, _ := http.NewRequest("GET", base+"/v1/sessions", nil)
	res, err := client.Do(auth(req))
	if err != nil {
		c.check("列出会话", false, err.Error())
		return
	}
	var list struct {
		Sessions []summary `json:"sessions"`
	}
	_ = json.NewDecoder(res.Body).Decode(&list)
	res.Body.Close()
	sessionID := ""
	for _, s := range list.Sessions {
		if s.Title == "冒烟会话" {
			sessionID = s.ID
		}
	}
	if sessionID == "" {
		c.check("列表里有沙箱那个会话", false, fmt.Sprintf("%d 行，标题：%s", len(list.Sessions), titles(list.Sessions)))
		return
	}
	c.check("列表里有沙箱那个会话", true, sessionID)

	// ── the socket: subscribe, snapshot ────────────────────────────────────
	wsURL := "ws://" + c.mobileAddr + "/v1/ws?token=" + url.QueryEscape(token)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		c.check("WebSocket 连得上", false, err.Error())
		return
	}
	defer conn.Close()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"open","sessionId":"`+sessionID+`"}`)); err != nil {
		c.check("发 open", false, err.Error())
		return
	}
	first := readEvent(conn, 10*time.Second)
	if first == nil || first.Type != "snapshot" {
		c.check("open 先得到一份快照", false, eventType(first))
		return
	}
	c.check("open 先得到一份快照", true, "")
	c.check("快照里这个会话是空闲的（没有假装在跑）", first.Snapshot != nil && first.Snapshot.State.Status == "idle",
		snapshotState(first))

	// ── the write path ─────────────────────────────────────────────────────
	body := `{"text":"` + phoneText + `","mode":"send"}`
	req, _ = http.NewRequest("POST", base+"/v1/sessions/"+url.PathEscape(sessionID)+"/messages", strings.NewReader(body))
	res, err = client.Do(auth(req))
	if err != nil {
		c.check("手机发消息被接受", false, err.Error())
		return
	}
	var accepted struct {
		Accepted bool   `json:"accepted"`
		Error    string `json:"error"`
	}
	_ = json.NewDecoder(res.Body).Decode(&accepted)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !accepted.Accepted {
		c.check("手机发消息被接受", false, fmt.Sprintf("%d %s", res.StatusCode, accepted.Error))
		return
	}
	c.check("手机发消息被接受", true, "")

	// ── the stream, and the one thing only the phone can answer ────────────
	// Order is the server's; what must all arrive is the echo, the permission card, the tool's
	// result, the reply and the end of the turn. The card is answered the way the phone answers
	// it (POST, allow_once) — that request is what releases the parked agent goroutine.
	var (
		echoSeen, askSeen, answered, resultSeen, replySeen, turnSeen bool
		reply, end, askDetail                                        string
		seen                                                         []string
	)
	for !turnSeen {
		ev := readEvent(conn, 30*time.Second)
		if ev == nil {
			break
		}
		if ev.Type == "user_message" {
			seen = append(seen, fmt.Sprintf("user_message(origin=%q text=%q)", ev.Origin, ev.Text))
		} else {
			seen = append(seen, ev.Type)
		}
		switch ev.Type {
		case "user_message":
			if ev.Origin == "mobile" && ev.Text == phoneText {
				echoSeen = true
			}
		case "ask":
			if ev.Ask == nil || ev.Ask.ToolID == "" || answered {
				break
			}
			askSeen = ev.Ask.Kind == "permission"
			askDetail = fmt.Sprintf("kind=%s permissionKind=%s preview=%q", ev.Ask.Kind, ev.Ask.PermissionKind, ev.Ask.Preview)
			if ev.Ask.PermissionKind != "bash" || !strings.Contains(ev.Ask.Preview, "echo mobile-e2e") {
				askSeen = false
			}
			decide := `{"decision":"allow_once"}`
			req, _ := http.NewRequest("POST",
				base+"/v1/sessions/"+url.PathEscape(sessionID)+"/permissions/"+url.PathEscape(ev.Ask.ToolID),
				strings.NewReader(decide))
			r, err := client.Do(auth(req))
			if err != nil {
				break
			}
			answered = r.StatusCode == http.StatusOK
			r.Body.Close()
		case "tool_result":
			if ev.ToolID == "call_mob1" {
				resultSeen = !ev.IsError
			}
		case "text":
			reply += ev.Text
			replySeen = strings.Contains(reply, "手机发来的命令跑完了")
		case "turn":
			turnSeen = true
			end = ev.End
		}
	}

	c.check("另一块屏的消息带着 origin=mobile 回声", echoSeen, strings.Join(seen, " → "))
	c.check("命令停在了权限卡上（bash 预览是命令本身）", askSeen, askDetail)
	c.check("从手机回答这张卡被接受", answered, "")
	c.check("回答之后命令真的执行了（工具结果回来了）", resultSeen, "")
	c.check("这一轮的正文到达了手机", replySeen, reply)
	c.check("一轮以 complete 结束", turnSeen && end == "complete", end)

	// ── what the model was actually asked ──────────────────────────────────
	// The wire proves the phone was served; these prove the turn was REAL — the same evidence
	// the UI scenarios use for their own writes.
	reqs := c.mock.Requests()
	asked := false
	if len(reqs) > 0 {
		asked = requestHasText(reqs[0], phoneText)
	}
	c.check("模型收到的第一条请求里就是手机发的那句话", asked, requestCount(reqs))
	ran := false
	if len(reqs) > 1 {
		// The command's OUTPUT is what proves it ran, not its arguments.
		ran = requestHasText(reqs[1], "mobile-e2e")
	}
	c.check("第二条请求带着命令的输出（权限放行后才跑的）", ran, requestCount(reqs))
}

// readEvent reads one decoded server event, or nil when nothing arrives in time.
func readEvent(conn *websocket.Conn, wait time.Duration) *mobileEvent {
	_ = conn.SetReadDeadline(time.Now().Add(wait))
	_, data, err := conn.ReadMessage()
	if err != nil {
		return nil
	}
	var ev mobileEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return nil
	}
	return &ev
}

// mobileEvent is the subset of the wire's events this scenario reads. Field names are the
// protocol's (mobile-protocol.md) — nothing here is reshaped for the test.
type mobileEvent struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	ToolID  string `json:"toolId"`
	IsError bool   `json:"isError"`
	End     string `json:"end"`
	Origin  string `json:"origin"`
	// session_prefs: the two switches another screen changed.
	Provider     string `json:"provider"`
	SkipApproval bool   `json:"skipApproval"`
	Ask          *struct {
		Kind           string `json:"kind"`
		ToolID         string `json:"toolId"`
		ToolName       string `json:"toolName"`
		PermissionKind string `json:"permissionKind"`
		Preview        string `json:"preview"`
	} `json:"ask"`
	Snapshot *struct {
		Model        string `json:"model"`
		SkipApproval bool   `json:"skipApproval"`
		State        struct {
			Status string `json:"status"`
		} `json:"state"`
	} `json:"snapshot"`
}

func eventType(ev *mobileEvent) string {
	if ev == nil {
		return "(没有事件)"
	}
	return ev.Type
}

func snapshotState(ev *mobileEvent) string {
	if ev == nil || ev.Snapshot == nil {
		return "(没有快照)"
	}
	return ev.Snapshot.State.Status
}

// mobileSessionRow is the list row this scenario reads.
type mobileSessionRow struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	ReadOnly bool   `json:"readOnly"`
}

func titles(rows []mobileSessionRow) string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Title)
	}
	return strings.Join(out, " | ")
}

// mobileSmokeToken reads the credential the sandbox's app wrote. The token is a file, not a
// config leaf (desktop/mobile_token.go), so this is how the settings card would hand it over.
func mobileSmokeToken(c *liveCtx) (string, bool) {
	raw, err := os.ReadFile(filepath.Join(c.home, ".tachi", "mobile.json"))
	if err != nil {
		c.check("读到 mobile.json", false, err.Error())
		return "", false
	}
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil || tok.Token == "" {
		c.check("mobile.json 里有一枚令牌", false, string(raw))
		return "", false
	}
	return tok.Token, true
}

// mobilePrefsRoundTrip checks that the two per-session switches travel between screens: the phone
// changes them over HTTP, and BOTH the phone (over its socket) and the window (its own controls,
// asserted by the driver that is running at the same time) must show the new pair.
//
// The window's half is why `live` runs concurrently with the driver rather than after it: the
// driver is sitting in a waitFor for exactly this change.
func mobilePrefsRoundTrip(c *liveCtx) {
	const altProvider = "smoke-alt"

	client := &http.Client{Timeout: 10 * time.Second}
	base := "http://" + c.mobileAddr
	token, ok := mobileSmokeToken(c)
	if !ok {
		return
	}
	auth := func(req *http.Request) *http.Request {
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		return req
	}

	up := false
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		res, err := client.Get(base + "/v1/ping")
		if err == nil {
			res.Body.Close()
			up = res.StatusCode == http.StatusOK
			if up {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !up {
		c.check("移动接口在进程里起来了", false, c.mobileAddr+" 没有应答 /v1/ping")
		return
	}
	c.check("移动接口在进程里起来了", true, c.mobileAddr)

	// The phone's model list is the providers this process knows; the switch below picks one of
	// them by name, exactly as the sheet would.
	req, _ := http.NewRequest("GET", base+"/v1/providers", nil)
	res, err := client.Do(auth(req))
	if err != nil {
		c.check("读到模型列表", false, err.Error())
		return
	}
	var providers struct {
		Providers []struct {
			Name string `json:"name"`
		} `json:"providers"`
	}
	_ = json.NewDecoder(res.Body).Decode(&providers)
	res.Body.Close()
	hasAlt := false
	for _, p := range providers.Providers {
		if p.Name == altProvider {
			hasAlt = true
		}
	}
	c.check("手机的模型列表里有第二个 provider", hasAlt, fmt.Sprintf("%d 个", len(providers.Providers)))

	// The session to switch: the fixture's only one.
	req, _ = http.NewRequest("GET", base+"/v1/sessions", nil)
	res, err = client.Do(auth(req))
	if err != nil {
		c.check("列出会话", false, err.Error())
		return
	}
	var list struct {
		Sessions []mobileSessionRow `json:"sessions"`
	}
	_ = json.NewDecoder(res.Body).Decode(&list)
	res.Body.Close()
	sessionID := ""
	for _, s := range list.Sessions {
		if s.Title == "冒烟会话" {
			sessionID = s.ID
		}
	}
	if sessionID == "" {
		c.check("列表里有沙箱那个会话", false, titles(list.Sessions))
		return
	}

	// A socket, so the phone's own view of the change is what is asserted (not just the record).
	wsURL := "ws://" + c.mobileAddr + "/v1/ws?token=" + url.QueryEscape(token)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		c.check("WebSocket 连得上", false, err.Error())
		return
	}
	defer conn.Close()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"open","sessionId":"`+sessionID+`"}`)); err != nil {
		c.check("发 open", false, err.Error())
		return
	}
	snap := readEvent(conn, 10*time.Second)
	if snap == nil || snap.Snapshot == nil {
		c.check("open 先得到一份快照", false, eventType(snap))
		return
	}
	c.check("初始状态：默认 provider、需要审批",
		snap.Snapshot.Model == "smoke" && !snap.Snapshot.SkipApproval,
		fmt.Sprintf("model=%q skip=%v", snap.Snapshot.Model, snap.Snapshot.SkipApproval))

	// The two writes, the same two calls the sheets make.
	for _, w := range []struct {
		path string
		body string
		name string
	}{
		{"/provider", `{"name":"` + altProvider + `"}`, "切模型"},
		{"/skip-approval", `{"skip":true}`, "打开跳过审批"},
	} {
		req, _ := http.NewRequest("POST", base+"/v1/sessions/"+url.PathEscape(sessionID)+w.path, strings.NewReader(w.body))
		r, err := client.Do(auth(req))
		if err != nil {
			c.check(w.name+"被接受", false, err.Error())
			continue
		}
		var body struct {
			Accepted bool   `json:"accepted"`
			Error    string `json:"error"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		r.Body.Close()
		c.check(w.name+"被接受", r.StatusCode == http.StatusOK && body.Accepted,
			fmt.Sprintf("%d %s", r.StatusCode, body.Error))
	}

	// Both switches arrive on the phone's socket. They travel in one event, but the two calls are
	// two writes: the second event carries the final pair, and that is what must land there.
	sawProvider, sawSkip := false, false
	finalProvider, finalSkip := "", true
	readDeadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(readDeadline) && (!sawProvider || !sawSkip) {
		ev := readEvent(conn, 5*time.Second)
		if ev == nil {
			break
		}
		if ev.Type != "session_prefs" {
			continue
		}
		if ev.Provider == altProvider {
			sawProvider = true
		}
		if ev.SkipApproval {
			sawSkip = true
		}
		finalProvider, finalSkip = ev.Provider, ev.SkipApproval
	}
	c.check("手机收到「模型被改了」", sawProvider, "")
	c.check("手机收到「审批开关被改了」", sawSkip, "")
	c.check("事件里带着两个开关的最终值", finalProvider == altProvider && finalSkip,
		fmt.Sprintf("provider=%q skip=%v", finalProvider, finalSkip))

	// The OTHER direction: the window flips the approval switch back, and this socket must hear
	// it. Same event, same writer — proving it once per direction is the whole point of a sync.
	back := false
	backDeadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(backDeadline) {
		ev := readEvent(conn, 5*time.Second)
		if ev == nil {
			break
		}
		if ev.Type == "session_prefs" && !ev.SkipApproval {
			back = true
			break
		}
	}
	c.check("窗口改了审批开关，手机也收到", back, "")
}
