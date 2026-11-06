package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/session"
)

// 测试只往 t.TempDir() 里写（见 .tachi.md 的「测试安全」）：机器表里是令牌，绝不能碰真的 ~/.tachi。
func tempBaseDir(t *testing.T) {
	t.Helper()
	config.SetBaseDir(t.TempDir())
}

func TestParseRemotePairing(t *testing.T) {
	t.Run("json payload", func(t *testing.T) {
		p, err := parseRemotePairing(`{"v":1,"url":"http://nas:8787","urls":["http://nas:8787","http://100.64.0.2:8787"],"token":"abc","deviceId":"dev1"}`)
		if err != nil {
			t.Fatal(err)
		}
		if p.Token != "abc" || p.DeviceID != "dev1" {
			t.Fatalf("payload = %+v", p)
		}
		if len(p.URLs) != 2 || p.URLs[0] != "http://nas:8787" || p.URLs[1] != "http://100.64.0.2:8787" {
			t.Fatalf("urls = %v", p.URLs)
		}
	})
	t.Run("the lines a headless host prints", func(t *testing.T) {
		// 这是 systemd journal 里的真实形状：带前缀、带标签、地址后面还有括号里的中文说明。
		// NAS / VPS 上没有二维码可扫，读者能复制的就是这两行。
		in := "tachi: phone pairing — fill this address in the app, or scan the code below: http://192.168.1.20:8787 (局域网)\n" +
			"tachi:                 fallback address: http://100.96.67.70:8787 (Tailscale)\n" +
			"tachi: token: abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"
		p, err := parseRemotePairing(in)
		if err != nil {
			t.Fatal(err)
		}
		if p.Token != "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG" {
			t.Fatalf("token = %q", p.Token)
		}
		if len(p.URLs) != 2 || p.URLs[0] != "http://192.168.1.20:8787" || p.URLs[1] != "http://100.96.67.70:8787" {
			t.Fatalf("urls = %v", p.URLs)
		}
	})
	t.Run("plain address and token", func(t *testing.T) {
		p, err := parseRemotePairing("http://nas:8787  abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG")
		if err != nil {
			t.Fatal(err)
		}
		if p.URL != "http://nas:8787" || p.Token != "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG" {
			t.Fatalf("payload = %+v", p)
		}
	})
	t.Run("trailing slash is normalised", func(t *testing.T) {
		p, err := parseRemotePairing("http://nas:8787/ abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG")
		if err != nil {
			t.Fatal(err)
		}
		if p.URL != "http://nas:8787" {
			t.Fatalf("url = %q", p.URL)
		}
	})
	t.Run("refusals", func(t *testing.T) {
		for _, in := range []string{"", "abcdef", "{not json", `{"token":"x"}`} {
			if _, err := parseRemotePairing(in); err == nil {
				t.Errorf("%q was accepted", in)
			}
		}
	})
}

// pairRemoteHost has to answer three questions in one gesture: is this address running Tachi,
// is the token right, and where else can this machine be reached. Each half is checked here
// because each one failing looks like a different, unhelpful thing on screen.
func TestPairRemoteHostProbesAddressAndToken(t *testing.T) {
	tempBaseDir(t)
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/ping":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true, "name": "tachi-desktop", "hostname": "nas.local", "version": "1.2.3",
				"deviceId": "dev-1", "urls": []string{"http://127.0.0.1:8787", "http://127.0.0.1:1"},
			})
		case "/v1/sessions":
			gotAuth = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(`{"sessions":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	host, err := pairRemoteHost(context.Background(), srv.URL+" abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG")
	if err != nil {
		t.Fatal(err)
	}
	if host.Token != "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG" || host.DeviceID != "dev-1" || host.Name != "nas" || host.Version != "1.2.3" {
		t.Fatalf("host = %+v", host)
	}
	if gotAuth != "Bearer abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG" {
		t.Fatalf("auth = %q", gotAuth)
	}
	// 地址：配对信息里的、真正应答的那条、对端自述的 —— 去重后都在，且应答的那条排第一。
	if host.URLs[0] != srv.URL {
		t.Fatalf("urls = %v", host.URLs)
	}
	if len(host.URLs) < 2 {
		t.Fatalf("the machine's own urls were dropped: %v", host.URLs)
	}
}

func TestPairRemoteHostRefusesAWrongToken(t *testing.T) {
	tempBaseDir(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/ping" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "deviceId": "dev-1"})
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()
	if _, err := pairRemoteHost(context.Background(), srv.URL+" wrongtokwrongtokwrongtokwrongtokwrong"); err == nil {
		t.Fatal("a wrong token was accepted")
	}
}

// The protocol's refusal codes must survive the trip: the frontend's table is keyed by them,
// and a generic "HTTP 409" would leave the reader nothing to act on.
func TestRemoteSendReportsTheProtocolCode(t *testing.T) {
	tempBaseDir(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sessions/s1/messages" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"turn_running"}`))
	}))
	defer srv.Close()

	d := &desktopApp{}
	if err := d.saveRemoteHosts([]remoteHost{{ID: "h1", Name: "nas", URLs: []string{srv.URL}, Token: "t"}}); err != nil {
		t.Fatal(err)
	}
	s := &AgentService{desk: d}
	if got := s.RemoteSend("h1", "s1", "hello", "send", nil); got != "turn_running" {
		t.Fatalf("RemoteSend = %q, want turn_running", got)
	}
	// An unknown machine is a sentence, not an empty string: the page shows it verbatim.
	if got := s.RemoteSend("nope", "s1", "hello", "send", nil); !strings.Contains(got, "没有这台远端机器") {
		t.Fatalf("RemoteSend(unknown host) = %q", got)
	}
}

// The WebSocket half: open a session, get its snapshot, and keep the connection for the
// increments. It is the one step a unit test can prove without a real host.
func TestRemoteOpenSessionWaitsForTheSnapshot(t *testing.T) {
	tempBaseDir(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ws" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("token") != "tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		c.SetReadLimit(1 << 20)
		// Read the subscription, then answer with a snapshot — then one increment, which must
		// not disturb the snapshot already handed to the caller.
		if _, _, err := c.Read(context.Background()); err != nil {
			return
		}
		_ = c.Write(context.Background(), websocket.MessageText, []byte(`{
			"type":"snapshot","snapshot":{
			"session":{"id":"s1","title":"聊天","updatedAt":"2026-11-06T22:00:00+08:00","running":true,"waiting":"","readOnly":false,"heldByTask":false},
			"messages":[{"index":0,"role":"user","content":"你好"}],
			"hasMore":true,
			"state":{"status":"busy","label":"处理","detail":"组织回答…","waiting":""},
			"asks":[],"model":"m","skipApproval":false,"notifyOff":false,
			"workspace":{"primary":"/tmp/w","additional":[]},
			"usage":{"cost":0.5,"credit":1.25,"cacheHitRate":0.75},"context":{"estimate":100,"window":1000,"parts":[{"label":"历史","tokens":60}]},"attachments":[]
		}}`))
		_ = c.Write(context.Background(), websocket.MessageText, []byte(`{"type":"text","text":"在"}`))
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()

	d := &desktopApp{}
	d.remoteHostsMu.Lock()
	_ = d.saveRemoteHosts([]remoteHost{{ID: "h1", Name: "nas", URLs: []string{srv.URL}, Token: "tok"}})
	d.remoteHostsMu.Unlock()
	t.Cleanup(func() { d.closeRemoteConn("") })

	s := &AgentService{desk: d}
	snap, why := s.RemoteOpenSession("h1", "s1")
	if why != "" {
		t.Fatal(why)
	}
	if snap.Session.ID != "s1" || len(snap.Messages) != 1 || snap.Messages[0].Content != "你好" {
		t.Fatalf("snapshot = %+v", snap)
	}
	if !snap.HasMore || snap.State.Status != "busy" || snap.Workspace.Primary != "/tmp/w" {
		t.Fatalf("snapshot fields = %+v", snap)
	}
	// 上下文圆环与费用也来自这份快照：数字、窗口、分项一个都不能少。
	if snap.Context.Estimate != 100 || snap.Context.Window != 1000 || len(snap.Context.Parts) != 1 || snap.Context.Parts[0].Label != "历史" {
		t.Fatalf("context = %+v", snap.Context)
	}
	if snap.Usage.Cost != 0.5 || snap.Usage.Credit != 1.25 || snap.Usage.CacheHitRate != 0.75 {
		t.Fatalf("usage = %+v", snap.Usage)
	}
}

// 面板的响应是包在一个字段里的：{"panels":{…}}。少了这层信封，解码不报错、只会得到三个空
// 数组 —— 屏幕上就是「明明有定时任务，面板却是空的」。
func TestRemotePanelsReadsTheEnvelopedShape(t *testing.T) {
	tempBaseDir(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sessions/s1/panels" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"panels":{
			"mcp":[{"name":"github","type":"stdio","connected":true,"needsAuth":false,"loaded":2,"tools":5}],
			"agents":{"peers":[{"name":"nas","label":"","id":"abc123","url":"https://nas:9990","note":"","reach":"可用","skills":["fnos"],"err":""}],"checkedAt":"22:00","anyChecked":true},
			"cron":[{"id":"j1","name":"日报","scheduleText":"每天 09:00","type":"recurring","status":"active","targetKind":"session","targetTitle":"日报","nextRun":"2026-11-08T09:00:00+08:00","lastRunStatus":"success","running":false}]
		}}`))
	}))
	defer srv.Close()

	d := &desktopApp{}
	if err := d.saveRemoteHosts([]remoteHost{{ID: "h1", Name: "nas", URLs: []string{srv.URL}, Token: "t"}}); err != nil {
		t.Fatal(err)
	}
	svc := &AgentService{desk: d}
	panels, why := svc.RemotePanels("h1", "s1")
	if why != "" {
		t.Fatalf("RemotePanels: %s", why)
	}
	if len(panels.MCP) != 1 || panels.MCP[0].Name != "github" || panels.MCP[0].Loaded != 2 || panels.MCP[0].Tools != 5 {
		t.Fatalf("mcp = %+v", panels.MCP)
	}
	if !panels.Agents.AnyChecked || len(panels.Agents.Peers) != 1 || panels.Agents.Peers[0].Reach != "可用" {
		t.Fatalf("agents = %+v", panels.Agents)
	}
	if len(panels.Cron) != 1 || panels.Cron[0].Name != "日报" || panels.Cron[0].Status != "active" || panels.Cron[0].NextRun == "" {
		t.Fatalf("cron = %+v", panels.Cron)
	}
}

// 机器的名字只存在这一侧（手机同一条规矩）：改的是本机的记录，不碰那台机器；空名字被拒，
// 而且重新配对不会把读者起过的名字改回机器自称的那个。
func TestRenameRemoteHost(t *testing.T) {
	tempBaseDir(t)
	d := &desktopApp{}
	if err := d.saveRemoteHosts([]remoteHost{{ID: "h1", Name: "ugreen", URLs: []string{"http://nas:8787"}, Token: "t"}}); err != nil {
		t.Fatal(err)
	}
	s := &AgentService{desk: d}
	vo, why := s.RenameRemoteHost("h1", "  家里 NAS  ")
	if why != "" {
		t.Fatalf("rename: %s", why)
	}
	if vo.Name != "家里 NAS" {
		t.Fatalf("name = %q", vo.Name)
	}
	// 落盘：下一次 ListRemoteHosts 读的是文件。
	list := s.ListRemoteHosts()
	if len(list) != 1 || list[0].Name != "家里 NAS" {
		t.Fatalf("list = %+v", list)
	}
	if _, why := s.RenameRemoteHost("h1", "   "); why == "" {
		t.Fatal("an empty name was accepted")
	}
	if _, why := s.RenameRemoteHost("nope", "x"); why == "" {
		t.Fatal("an unknown machine was accepted")
	}
}

// The client against the REAL interface, in one process: the phone server this app already
// has, driven by the client that ships with the remote view. A fake host proves the client's
// reading of the protocol; this proves both halves agree (paths, auth, snapshot shape, and the
// refusal codes a reader actually sees).
func TestRemoteClientAgainstTheRealInterface(t *testing.T) {
	d, sm := newMobileApp(t)
	s, err := sm.New("", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	sm.SetTitle("hello")
	if err := sm.AppendMessage(&session.Message{Type: session.MessageTypeUser, Content: "你好", Timestamp: time.Now()}); err != nil {
		t.Fatalf("append: %v", err)
	}
	_, ts := mobileTestServer(t, d)

	client := &desktopApp{}
	if err := client.saveRemoteHosts([]remoteHost{{ID: "h1", Name: "nas", URLs: []string{ts.URL}, Token: "secret"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.closeRemoteConn("") })
	svc := &AgentService{desk: client}

	rows, why := svc.RemoteSessions("h1")
	if why != "" {
		t.Fatalf("RemoteSessions: %s", why)
	}
	if len(rows) != 1 || rows[0].ID != s.ID || rows[0].Title != "hello" {
		t.Fatalf("rows = %+v", rows)
	}

	snap, why := svc.RemoteOpenSession("h1", s.ID)
	if why != "" {
		t.Fatalf("RemoteOpenSession: %s", why)
	}
	if len(snap.Messages) != 1 || snap.Messages[0].Content != "你好" || snap.Messages[0].Role != "user" {
		t.Fatalf("messages = %+v", snap.Messages)
	}

	// A wrong token is the 401 path, not a generic failure.
	if err := client.saveRemoteHosts([]remoteHost{
		{ID: "h1", Name: "nas", URLs: []string{ts.URL}, Token: "secret"},
		{ID: "h2", Name: "nas", URLs: []string{ts.URL}, Token: "wrong"},
	}); err != nil {
		t.Fatal(err)
	}
	if got := svc.RemoteSend("h2", s.ID, "hi", "send", nil); got != "unauthorized" {
		t.Fatalf("wrong token = %q, want unauthorized", got)
	}

	// No provider is configured for this app, so a real send is refused by the host — the code
	// must come through as the protocol's own word.
	if got := svc.RemoteSend("h1", s.ID, "hi", "send", nil); got != "agent_unavailable" {
		t.Fatalf("send = %q, want agent_unavailable", got)
	}
	// A switch writes the record, which is what both screens read back.
	if got := svc.RemoteSetSkipApproval("h1", s.ID, true); got != "ok" {
		t.Fatalf("skip approval = %q", got)
	}
	if rec, err := sm.Load(s.ID); err != nil || rec == nil || !rec.SkipApproval {
		t.Fatalf("skip approval was not written: rec=%+v err=%v", rec, err)
	}

	// 新建会话：走的是那台机器自己的默认工作目录（~/workspace）。HOME 指到临时目录，
	// 否则这个用例会在真的 home 下建目录（见 .tachi.md 的「测试安全」）。
	t.Setenv("HOME", t.TempDir())
	created, why := svc.RemoteNewSession("h1")
	if why != "" {
		t.Fatalf("RemoteNewSession: %s", why)
	}
	if created.ID == "" {
		t.Fatal("RemoteNewSession returned no id")
	}
	rows, why = svc.RemoteSessions("h1")
	if why != "" {
		t.Fatalf("RemoteSessions after create: %s", why)
	}
	found := false
	for _, r := range rows {
		if r.ID == created.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("the created session %s is not in the list: %+v", created.ID, rows)
	}
	if _, why := svc.RemoteOpenSession("h1", created.ID); why != "" {
		t.Fatalf("open the created session: %s", why)
	}

	// 重命名与删除：走的是那台机器自己的写入方；空标题被它拒绝（bad_request），删除之后
	// 再打开会立刻得到 no_session —— 不是等满一次快照超时。
	if got := svc.RemoteRenameSession("h1", created.ID, "改名了"); got != "ok" {
		t.Fatalf("rename = %q", got)
	}
	rows, _ = svc.RemoteSessions("h1")
	renamed := false
	for _, r := range rows {
		if r.ID == created.ID && r.Title == "改名了" {
			renamed = true
		}
	}
	if !renamed {
		t.Fatalf("rename did not reach the list: %+v", rows)
	}
	if got := svc.RemoteRenameSession("h1", created.ID, "   "); got != "bad_request" {
		t.Fatalf("empty title = %q, want bad_request", got)
	}
	if got := svc.RemoteDeleteSession("h1", created.ID); got != "ok" {
		t.Fatalf("delete = %q", got)
	}
	if _, why := svc.RemoteOpenSession("h1", created.ID); why != "no_session" {
		t.Fatalf("open a deleted session = %q, want no_session", why)
	}

	// 置顶与推送开关：都是那段会话记录上的字段，写入方在对面；这里只按它的答案读回来。
	if got := svc.RemoteSetSessionPinned("h1", s.ID, true); got != "ok" {
		t.Fatalf("pin = %q", got)
	}
	rows, _ = svc.RemoteSessions("h1")
	pinned := false
	for _, r := range rows {
		if r.ID == s.ID && r.Pinned {
			pinned = true
		}
	}
	if !pinned {
		t.Fatalf("pin did not reach the list: %+v", rows)
	}
	if got := svc.RemoteSetSessionNotify("h1", s.ID, true); got != "ok" {
		t.Fatalf("notify = %q", got)
	}
	snapAfter, why := svc.RemoteOpenSession("h1", s.ID)
	if why != "" {
		t.Fatalf("open after notify: %s", why)
	}
	if !snapAfter.NotifyOff {
		t.Fatal("the phone-push switch did not stick")
	}

	// 面板是三个读：MCP 的连接状态、可委派节点、定时任务。空也要是空数组（协议的类型规则）。
	panels, why := svc.RemotePanels("h1", s.ID)
	if why != "" {
		t.Fatalf("panels: %s", why)
	}
	if panels.MCP == nil || panels.Agents.Peers == nil || panels.Cron == nil {
		t.Fatalf("panels returned nil slices: %+v", panels)
	}

	// 上传：本机读字节，落到那段会话自己的 uploads/ 里；发送时递的名字就是它。
	note := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(note, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	up, why := svc.RemoteUploadFile("h1", s.ID, note)
	if why != "" {
		t.Fatalf("upload: %s", why)
	}
	if up.Name == "" || up.Size != 5 {
		t.Fatalf("upload = %+v", up)
	}
	if dir, err := session.UploadDir(sm.BaseDir(), s.ID); err != nil {
		t.Fatalf("upload dir: %v", err)
	} else if st, err := os.Stat(filepath.Join(dir, up.Name)); err != nil || st.Size() != 5 {
		t.Fatalf("the upload did not land in the session's uploads: %v", err)
	}

	// 下载：一个 SendFile 交付过的文件，按那次工具调用的 id 取回来，落在本机 HOME 下。
	if err := sm.AppendMessage(&session.Message{
		Type: session.MessageTypeToolCall, Name: "SendFile", ToolCallID: "tc-file",
		Args: map[string]any{"path": note},
	}); err != nil {
		t.Fatalf("append SendFile record: %v", err)
	}
	saved, why := svc.RemoteDownloadFile("h1", s.ID, "tc-file", "note.txt")
	if why != "" {
		t.Fatalf("download: %s", why)
	}
	if got, err := os.ReadFile(saved); err != nil || string(got) != "hello" {
		t.Fatalf("downloaded file = %q err=%v (path %s)", got, err, saved)
	}
	if !strings.HasPrefix(saved, os.Getenv("HOME")) {
		t.Fatalf("download landed outside HOME: %s", saved)
	}
}

// 断线之后应该自己回来：一条连接被对面掐掉，退避几秒后重拨，重连成功再谈「重读快照」。
// 这里只断言「第二次连接真的建立了」——事件、快照重读是页面那一半的事。
func TestRemoteReconnectsAfterTheSocketDrops(t *testing.T) {
	tempBaseDir(t)
	conns := make(chan int, 8)
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ws" {
			http.NotFound(w, r)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		c.SetReadLimit(1 << 20)
		mu.Lock()
		n++
		idx := n
		mu.Unlock()
		conns <- idx
		if _, _, err := c.Read(context.Background()); err != nil {
			return
		}
		_ = c.Write(context.Background(), websocket.MessageText, []byte(`{"type":"snapshot","snapshot":{
			"session":{"id":"s1","title":"聊天","updatedAt":"2026-11-06T22:00:00+08:00","running":false,"waiting":"","readOnly":false,"heldByTask":false},
			"messages":[],"hasMore":false,"state":{"status":"idle","label":"空闲","detail":"","waiting":""},
			"asks":[],"model":"m","skipApproval":false,"notifyOff":false,
			"workspace":{"primary":"/tmp","additional":[]},"usage":{"cost":0},
			"context":{"estimate":0,"window":0,"parts":[]},"attachments":[]}}`))
		if idx == 1 {
			// 第一段连接：说完话就把它掐了，重连该由客户端自己发起。
			_ = c.Close(websocket.StatusGoingAway, "bye")
			return
		}
		time.Sleep(300 * time.Millisecond)
	}))
	defer srv.Close()

	d := &desktopApp{}
	if err := d.saveRemoteHosts([]remoteHost{{ID: "h1", Name: "nas", URLs: []string{srv.URL}, Token: "t"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.closeRemoteConn("") })
	svc := &AgentService{desk: d}
	if _, why := svc.RemoteOpenSession("h1", "s1"); why != "" {
		t.Fatalf("open: %s", why)
	}
	// 第一条（我们刚才那条）、第二条（重连）。
	select {
	case idx := <-conns:
		if idx != 1 {
			t.Fatalf("first connection index = %d", idx)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the first connection was not seen")
	}
	select {
	case idx := <-conns:
		if idx < 2 {
			t.Fatalf("unexpected connection index %d", idx)
		}
	case <-time.After(remoteReconnectMaxBackoff + 3*time.Second):
		t.Fatal("the client did not reconnect after the socket dropped")
	}
}
