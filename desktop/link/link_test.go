package link

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSource 是一台最小的宿主：一份随时可改的快照、一条攒着的提示音队列，和一条收到的回答的记录。
type fakeSource struct {
	mu       sync.Mutex
	snap     Snapshot
	alerts   []Alert
	subs     map[int]func()
	nextSub  int
	answered []Answer
	// keys 记下设备送回来的手势，顺序即收到的顺序（见 TestDeviceKeysReachTheSource）。
	keys []Key
}

func newFakeSource() *fakeSource { return &fakeSource{subs: map[int]func(){}} }

// alert 攒一条提示音并通知——和 desktop 那边 enqueueAlert 做的事一样。
func (f *fakeSource) alert(kind string) {
	f.mu.Lock()
	f.alerts = append(f.alerts, Alert{Kind: kind})
	f.mu.Unlock()
	f.notify()
}

// Key 收下设备的一个手势。含义由宿主决定，所以这里只记录、不解释（那是 desktop 的 Key）。
func (f *fakeSource) Key(k Key) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, k)
	return nil
}

// keysSeen 是收到的手势（拷贝一份，调用方不该碰内部切片）。
func (f *fakeSource) keysSeen() []Key {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Key(nil), f.keys...)
}

func (f *fakeSource) TakeAlerts() []Alert {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.alerts
	f.alerts = nil
	return out
}

func (f *fakeSource) Snapshot() Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap
}

func (f *fakeSource) Answer(a Answer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answered = append(f.answered, a)
	return nil
}

func (f *fakeSource) Subscribe(fn func()) func() {
	f.mu.Lock()
	f.nextSub++
	id := f.nextSub
	f.subs[id] = fn
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		delete(f.subs, id)
		f.mu.Unlock()
	}
}

// update 改快照并通知，模拟宿主那边发生了一件事。
func (f *fakeSource) update(fn func(*Snapshot)) {
	f.mu.Lock()
	fn(&f.snap)
	f.mu.Unlock()
	f.notify()
}

// notify 叫醒服务循环（不动快照）。
func (f *fakeSource) notify() {
	f.mu.Lock()
	subs := make([]func(), 0, len(f.subs))
	for _, s := range f.subs {
		subs = append(subs, s)
	}
	f.mu.Unlock()
	for _, s := range subs {
		s()
	}
}

func (f *fakeSource) answers() []Answer {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Answer(nil), f.answered...)
}

// peer 是「设备那一头」：一个真的 TCP 连接。用真的网络而不是内存管道，是因为
// net.Pipe 是同步无缓冲的——一次没人读的写会把服务循环卡住，那样测出来的东西
// 是测试自己制造的，而不是协议的行为。
type peer struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
	// srv 是这条连接那头的 Server。VoiceText / VoiceError 这类「从链路自己的 goroutine
	// 之外写出去」的消息只有拿着它才测得到，而它们恰恰是最容易漏的那两条。
	srv *Server
}

func newPeer(t *testing.T, src Source) *peer {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	srvConn, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	srv := NewServer(src, srvConn)
	go func() {
		defer close(done)
		_ = srv.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		_ = conn.Close()
		<-done
	})

	return &peer{t: t, conn: conn, r: bufio.NewReader(conn), srv: srv}
}

func (p *peer) read() map[string]any {
	p.t.Helper()
	_ = p.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		line, err := p.r.ReadString('\n')
		if err != nil {
			p.t.Fatalf("read: %v", err)
		}
		if !strings.HasPrefix(line, prefix) {
			continue // 不是协议行：设备侧这里会进控制台日志
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, prefix)), &m); err != nil {
			p.t.Fatalf("undecodable line %q: %v", line, err)
		}
		return m
	}
}

// readOf 读到指定类型为止，中间的类型照读不误（它们本来就是同一次推送的一部分）。
func (p *peer) readOf(typ string) map[string]any {
	p.t.Helper()
	for i := 0; i < 20; i++ {
		if m := p.read(); m["t"] == typ {
			return m
		}
	}
	p.t.Fatalf("never saw a %q message", typ)
	return nil
}

func (p *peer) write(v any) {
	p.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		p.t.Fatalf("marshal: %v", err)
	}
	if _, err := p.conn.Write(append(append([]byte(prefix), b...), '\n')); err != nil {
		p.t.Fatalf("write: %v", err)
	}
}

// expectSilence 断言在一小段时间内没有新消息。负向断言只能靠等待，所以窗口要短。
func (p *peer) expectSilence(d time.Duration) {
	p.t.Helper()
	_ = p.conn.SetReadDeadline(time.Now().Add(d))
	if _, err := p.r.ReadString('\n'); err == nil {
		p.t.Fatalf("expected no message, got one")
	}
}

func askFixture() Snapshot {
	return Snapshot{
		Sessions: []Session{{ID: "s1", Title: "修 lint", Status: "tool_running", Label: "执行", Detail: "调用 Bash"}},
		Asks: []Ask{{
			Key: "s1\x00call_1", Kind: kindPermission, SessionID: "s1", ToolID: "call_1",
			Title: "Bash 确认", Body: "$ rm -rf build/",
			Options: []Option{{Label: "允许一次", Value: DecisionAllowOnce}},
		}},
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestConnectingBringsTheWholePicture(t *testing.T) {
	src := newFakeSource()
	src.snap = askFixture()
	p := newPeer(t, src)

	// 不等 hello：设备连上就可能已经错过了主机的推送，所以全量是先送的。
	state := p.readOf("state")
	sessions, _ := state["sessions"].([]any)
	if len(sessions) != 1 {
		t.Fatalf("state carried %d session(s), want 1", len(sessions))
	}
	if got := sessions[0].(map[string]any)["title"]; got != "修 lint" {
		t.Errorf("session title = %v, want 修 lint", got)
	}

	ask := p.readOf("ask")
	if ask["kind"] != kindPermission || ask["session"] != "s1" || ask["toolId"] != "call_1" {
		t.Errorf("ask = %v", ask)
	}
	if ask["body"] != "$ rm -rf build/" {
		t.Errorf("ask body = %v", ask["body"])
	}
	if ask["ref"] == nil {
		t.Error("ask carries no ref: the device would have nothing to answer with")
	}
}

// 「本轮第几个工具调用」要真的上得了线。0 则不发：设备把「0」和「没这个字段」当成同一
// 件事（不画那一行），所以空着它只是白占每 10 秒一趟的字节。
func TestTheToolCallCountTravelsAndZeroIsNotSent(t *testing.T) {
	src := newFakeSource()
	snap := askFixture()
	snap.Sessions[0].Tools = 3
	src.snap = snap
	p := newPeer(t, src)

	first := p.readOf("state")["sessions"].([]any)[0].(map[string]any)
	if got, ok := first["tools"].(float64); !ok || got != 3 {
		t.Errorf("tools = %v, want 3", first["tools"])
	}

	// 回合结束、计数归零：这一项就该从线上消失，而不是发一个 0。
	src.update(func(s *Snapshot) { s.Sessions[0].Tools = 0 })
	after := p.readOf("state")["sessions"].([]any)[0].(map[string]any)
	if got, present := after["tools"]; present {
		t.Errorf("tools = %v, want the field omitted once the turn is over", got)
	}
}

// 每条连接都是一个全新的 ref 空间（Server 是每条连接一个，nextSeq 从 0 开始），所以
// 设备手里的旧等待项必须先被清空：不清的话，同一条等待会以新 ref 再入队一次——屏幕上
// 出现两条，而旧的那条按下去没有任何反应（它在主机的 ref 表里已经不存在了）。
func TestANewConnectionClearsTheDevicesAsksFirst(t *testing.T) {
	src := newFakeSource()
	src.snap = askFixture()
	p := newPeer(t, src)

	if first := p.read(); first["t"] != "reset" {
		t.Fatalf("first message on a new connection = %v, want reset", first["t"])
	}
	// 清空之后必须立刻跟上全量：只发 reset 而后面没有东西，设备会停在一个空屏上。
	if second := p.read(); second["t"] != "state" {
		t.Fatalf("second message = %v, want state", second["t"])
	}
	if third := p.read(); third["t"] != "ask" {
		t.Fatalf("third message = %v, want ask", third["t"])
	}
	// 小窗状态也属于这一次全量（它排在最后，见 push 的末尾）。
	if fourth := p.read(); fourth["t"] != "hud" {
		t.Fatalf("fourth message = %v, want hud", fourth["t"])
	}

	// reset 属于「连接」而不是「每次推送」：一次变化之后只该有推送。
	src.update(func(s *Snapshot) { s.Sessions[0].Label = "思考" })
	if m := p.read(); m["t"] != "state" {
		t.Fatalf("after an update the server sent %v, want state", m["t"])
	}
}

// 小窗的状态是一条**要同步的状态**：它挂在 Snapshot 上，只在真的变了的时候占一行。
//
// 它决定设备底栏写什么、以及上下键要不要发上来——所以「开着」与「关了」这两条都必须真的
// 过去，而不能靠设备猜。
func TestHudStateIsPushedWhenItChanges(t *testing.T) {
	src := newFakeSource()
	p := newPeer(t, src)

	// 刚连上（hello 后的第一次全量推送）就该说清它关着：设备不能靠「还没收到」当默认。
	if m := p.readOf("hud"); m["open"] != false {
		t.Errorf("第一次推送里 open = %v, want false", m["open"])
	}

	src.update(func(s *Snapshot) { s.Hud = Hud{Open: true} })
	if m := p.readOf("hud"); m["open"] != true {
		t.Errorf("小窗开了，设备收到的却是 open = %v", m["open"])
	}

	// 内容没变就不该重复发（这条链路每 10 秒一轮，去抖是它安静的原因）。
	src.notify()
	p.expectSilence(150 * time.Millisecond)

	src.update(func(s *Snapshot) { s.Hud = Hud{Open: false} })
	if m := p.readOf("hud"); m["open"] != false {
		t.Errorf("小窗关了，设备收到的却是 open = %v", m["open"])
	}
}

// 设备的手势原样送到宿主：四个动作一个都不能在路上被解释（语义在 desktop 的 Key 里）。
func TestDeviceKeysReachTheSource(t *testing.T) {
	src := newFakeSource()
	p := newPeer(t, src)

	for _, action := range []string{KeyUp, KeyDown, KeyOK, KeyOKDouble} {
		p.write(map[string]any{"t": "key", "key": action})
	}

	waitFor(t, "the keys to reach the source", func() bool { return len(src.keysSeen()) == 4 })
	got := src.keysSeen()
	for i, want := range []string{KeyUp, KeyDown, KeyOK, KeyOKDouble} {
		if got[i].Action != want {
			t.Errorf("第 %d 个手势 = %q, want %q", i+1, got[i].Action, want)
		}
	}
}

func TestAnswerReachesTheSource(t *testing.T) {
	src := newFakeSource()
	src.snap = askFixture()
	p := newPeer(t, src)

	ask := p.readOf("ask")
	p.write(map[string]any{"t": "answer", "ref": ask["ref"], "decision": DecisionDeny})

	waitFor(t, "the answer to reach the source", func() bool { return len(src.answers()) == 1 })
	got := src.answers()[0]
	if got.SessionID != "s1" || got.ToolID != "call_1" {
		t.Errorf("answer addressed %s/%s, want s1/call_1", got.SessionID, got.ToolID)
	}
	if got.Kind != kindPermission || got.Decision != DecisionDeny {
		t.Errorf("answer = %+v", got)
	}
}

// 一条不再等待的回答必须被拒——设备上的界面可能还停在某个已经被回答或取消的项上。
func TestAStaleRefIsRefused(t *testing.T) {
	src := newFakeSource()
	src.snap = askFixture()
	p := newPeer(t, src)
	p.readOf("ask")

	p.write(map[string]any{"t": "answer", "ref": 9999, "decision": DecisionAllowOnce})

	// 答案根本没有对应的等待，所以只有一个办法能说清它没被投递：等一会儿。
	time.Sleep(200 * time.Millisecond)
	if got := src.answers(); len(got) != 0 {
		t.Errorf("a stale ref reached the source: %+v", got)
	}
}

func TestAVanishedAskIsWithdrawn(t *testing.T) {
	src := newFakeSource()
	src.snap = askFixture()
	p := newPeer(t, src)

	ask := p.readOf("ask")
	// 在电脑上把它答掉了：设备必须知道撤掉，否则它会一直举着一个作废的问题。
	src.update(func(s *Snapshot) { s.Asks = nil })

	gone := p.readOf("ask_gone")
	if gone["ref"] != ask["ref"] {
		t.Errorf("ask_gone ref = %v, want %v", gone["ref"], ask["ref"])
	}
}

// 内容没变就不说话：思考中的每个 delta 都会推一次状态，不去抖的话这条链路一直在
// 说话，而设备上什么都没变。
func TestAnUnchangedPictureIsNotReSent(t *testing.T) {
	src := newFakeSource()
	src.snap = askFixture()
	p := newPeer(t, src)
	p.readOf("state")
	p.readOf("ask")
	p.readOf("hud") // 这一次全量的最后一条（见 push 的末尾）

	src.update(func(*Snapshot) {}) // 通知了，但什么都没变

	p.expectSilence(300 * time.Millisecond)
}

// 设备与控制台日志共用一条流，噪声不能被当成协议，也不能被当成断线。
func TestConsoleNoiseIsIgnored(t *testing.T) {
	src := newFakeSource()
	src.snap = askFixture()
	p := newPeer(t, src)
	p.readOf("state")

	p.t.Helper()
	for _, junk := range []string{
		"I (1234) wifi: state: run -> init\n",
		"ESP-ROM:esp32c3-api1-20210207\n",
		prefix + "not json at all\n", // 冒充协议行的坏数据
	} {
		if _, err := p.conn.Write([]byte(junk)); err != nil {
			t.Fatalf("write junk: %v", err)
		}
	}

	// 坏数据之后，一条 hello 仍然能得到一份完整的答复。
	p.write(map[string]any{"t": "hello", "proto": Proto})
	if m := p.readOf("state"); m["sessions"] == nil {
		t.Errorf("state after noise = %v", m)
	}
}

// hello 报的版本不对：说清楚，然后断开——而不是拿一个猜的协议继续通话。
func TestAProtocolMismatchIsRefused(t *testing.T) {
	src := newFakeSource()
	src.snap = askFixture()
	p := newPeer(t, src)

	p.write(map[string]any{"t": "hello", "proto": Proto + 99})

	m := p.readOf("error")
	if msg, _ := m["message"].(string); !strings.Contains(msg, "protocol") {
		t.Errorf("error message = %q, want it to name the protocol mismatch", msg)
	}
}

// 重连的设备漏掉的东西要能补齐：sync 必须无视去抖，把当前的全量再发一遍。
func TestSyncResendsEverything(t *testing.T) {
	src := newFakeSource()
	src.snap = askFixture()
	p := newPeer(t, src)
	p.readOf("state")
	ask := p.readOf("ask")

	p.write(map[string]any{"t": "sync", "since": 0})

	state := p.readOf("state")
	if len(state["sessions"].([]any)) != 1 {
		t.Errorf("sync sent %v", state)
	}
	// 同一条待答项要带着同样的 ref 回来：设备上那一项是原地更新，游标不该跳。
	again := p.readOf("ask")
	if again["ref"] != ask["ref"] {
		t.Errorf("re-sent ask ref = %v, want the original %v", again["ref"], ask["ref"])
	}
}

// 提示音是事件，不是状态：它必须在**内容什么都没变**时也能发出去。
//
// 这正是它与快照分开的原因——push 会去抖（每个 delta 都会 setSessionState，内容却常常
// 一个字都没变），把一声「好了」挂在去抖上就会被吞掉。
func TestAnAlertReachesTheDeviceEvenWithoutAnyChange(t *testing.T) {
	src := newFakeSource()
	src.snap = askFixture()
	p := newPeer(t, src)

	// 先把它连上时要发的东西读掉：reset + 全量。
	p.readOf("reset")
	p.readOf("state")
	p.readOf("ask")

	src.alert(AlertKindDone)

	alert := p.readOf("alert")
	if alert["kind"] != AlertKindDone {
		t.Errorf("alert kind = %v, want %q", alert["kind"], AlertKindDone)
	}
	if alert["seq"] == nil {
		t.Error("alert carries no seq")
	}
}
