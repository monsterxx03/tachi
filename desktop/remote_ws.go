package main

// 远端连接：一条 WebSocket，订阅一段对话，把远端推来的事件原样交给前端。
//
// 协议（docs/agents/mobile-protocol.md）只有一条连接、一次订阅一段会话，换会话就是一次
// 新的 open。所以这里也是一个进程一条（换机器时旧的关掉）：同一时刻读者只看得到一台
// 机器的对话，第二条连接只会多收一份没人画的事件。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

const (
	// remoteEventName 是前端订阅的那个事件名。远端的协议事件原样透传，另外多带一个
	// hostId：窗口可能换过机器，一条不属于当前那台的事件不能被画进来。
	remoteEventName = "remote:event"
	// remoteOpenTimeout 是「要一份快照」的等待上限。它比普通请求宽：对面可能正在跑一轮，
	// 快照要读一段历史。
	remoteOpenTimeout = 15 * time.Second
	// remotePingInterval 是协议的心跳（手机也每 20 秒一次）；两分钟没有心跳服务端会关连接。
	remotePingInterval = 20 * time.Second
	// remoteReadLimit 必须远大于默认的 32 KiB：一次快照带 100 条记录，一条工具结果最多 2 KiB，
	// 再加上正文。
	remoteReadLimit = 8 << 20
	// 断线之后重试几次、每次等多久。重试只负责把连接拉回来，不重放事件：连上之后页面自己重读
	// 快照/列表/面板（记录是权威的）。
	remoteReconnectAttempts   = 6
	remoteReconnectBackoff    = time.Second
	remoteReconnectMaxBackoff = 4 * time.Second
)

type remoteConn struct {
	d      *desktopApp
	hostID string
	ws     *websocket.Conn

	writeMu sync.Mutex
	openMu  sync.Mutex
	snaps   chan RemoteSnapshotVO
	// openErr carries the server's answer when it cannot produce a snapshot (a session that was
	// deleted, say). Without it the request would sit out the whole open timeout even though the
	// answer arrived in the first millisecond.
	openErr chan string
	// done closes when the connection dies or is closed on purpose: a caller waiting for a
	// snapshot must not sit out its whole timeout on a socket that is already gone.
	done   chan struct{}
	closed atomic.Bool

	// 这一段会话是这条连接订着的；断线重连之后页面会用它重新 open（见 reconnected 事件）。
	subMu         sync.Mutex
	lastSessionID string
}

func dialRemoteConn(d *desktopApp, host remoteHost, base string) (*remoteConn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), remoteProbeTimeout)
	defer cancel()
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return nil, errors.New("不认识的地址：" + base)
	}
	u.Path = "/v1/ws"
	u.RawQuery = url.Values{"token": {host.Token}}.Encode()
	ws, resp, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
		HTTPClient: &http.Client{Timeout: remoteProbeTimeout},
	})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return nil, errors.New("令牌不对")
		}
		return nil, err
	}
	ws.SetReadLimit(remoteReadLimit)
	return &remoteConn{d: d, hostID: host.ID, ws: ws, snaps: make(chan RemoteSnapshotVO, 1), openErr: make(chan string, 1), done: make(chan struct{})}, nil
}

// dialAny tries every address this machine knows for the host, in order. It is both the first
// connection and the reconnection path, so a LAN address that has gone away costs one failed
// dial rather than the whole attempt.
func dialAny(d *desktopApp, host remoteHost) (*remoteConn, error) {
	var lastErr error
	for _, base := range host.URLs {
		c, err := dialRemoteConn(d, host, base)
		if err != nil {
			lastErr = err
			continue
		}
		return c, nil
	}
	if lastErr == nil {
		lastErr = errors.New("没有可用地址")
	}
	return nil, lastErr
}

// remoteConnection returns the live connection to this host, dialing if there is none.
//
// 同时只留一条：换机器先关旧的。两条一起跑的时候（两次点击）后来者收下，先到的那条被关掉，
// 而不是让 d.remoteConn 指向一条没人读的连接。
func (d *desktopApp) remoteConnection(host remoteHost) (*remoteConn, error) {
	d.remoteMu.Lock()
	if c := d.remoteConn; c != nil && c.hostID == host.ID && !c.closed.Load() {
		d.remoteMu.Unlock()
		return c, nil
	}
	d.remoteMu.Unlock()
	// 一次显式的连接请求作废掉还在退避中的重试：同一个入口不要有两个拨号者。
	d.cancelRemoteRetry()
	d.closeRemoteConn("")

	c, err := dialAny(d, host)
	if err != nil {
		return nil, err
	}
	d.remoteMu.Lock()
	if cur := d.remoteConn; cur != nil && cur.hostID == host.ID && !cur.closed.Load() {
		// 另一个调用正在同一刻拨同一台机器：用它的，这条关掉。
		d.remoteMu.Unlock()
		c.close()
		return cur, nil
	}
	d.remoteConn = c
	d.remoteMu.Unlock()
	go c.readLoop()
	go c.pingLoop()
	return c, nil
}

// closeRemoteConn drops the live connection to one machine ("" = whichever is live). It is
// deliberately silent: a reader who leaves the remote view does not need a "disconnected"
// notice for a connection they asked to end.
func (d *desktopApp) closeRemoteConn(hostID string) {
	d.cancelRemoteRetry()
	d.remoteMu.Lock()
	c := d.remoteConn
	match := c != nil && (hostID == "" || c.hostID == hostID)
	if match {
		d.remoteConn = nil
	}
	d.remoteMu.Unlock()
	if match {
		c.close()
	}
}

func (c *remoteConn) readLoop() {
	for {
		_, data, err := c.ws.Read(context.Background())
		if err != nil {
			c.fail(err)
			return
		}
		var head struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(data, &head) != nil {
			continue
		}
		if head.Type == mobileEvSnapshot {
			// The protocol wraps it: {"type":"snapshot","snapshot":{...}} — the panel is one
			// field beside the event name, not the envelope itself.
			var env struct {
				Snapshot RemoteSnapshotVO `json:"snapshot"`
			}
			if err := json.Unmarshal(data, &env); err != nil {
				continue
			}
			snap := env.Snapshot
			select {
			case c.snaps <- snap:
			default:
				// 没有人在等快照（例如换轨之后补来的一份）：丢掉，下一次 open 会重新要。
			}
			continue
		}
		var msg map[string]any
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		if head.Type == mobileEvError {
			// The one error that matters to a waitER: the server could not answer `open` with a
			// snapshot. It still goes to the page below (a turn's own error is a notice there).
			select {
			case c.openErr <- stringField(msg, "message"):
			default:
			}
		}
		msg["hostId"] = c.hostID
		c.d.emit(remoteEventName, msg)
	}
}

// fail reports a broken connection once and clears the slot it occupied.
func (c *remoteConn) fail(err error) {
	if !c.closed.CompareAndSwap(false, true) {
		return
	}
	close(c.done)
	_ = c.ws.CloseNow()
	c.d.remoteMu.Lock()
	if c.d.remoteConn == c {
		c.d.remoteConn = nil
	}
	c.d.remoteMu.Unlock()
	// 意外断开：退避重试，并且把进展告诉页面（它据此显示「重连中…」，而不是一句「断开了」）。
	go c.d.reconnectRemote(c.hostID, err.Error())
}

func (c *remoteConn) close() {
	if !c.closed.CompareAndSwap(false, true) {
		return
	}
	close(c.done)
	_ = c.ws.Close(websocket.StatusNormalClosure, "")
}

func (c *remoteConn) pingLoop() {
	t := time.NewTicker(remotePingInterval)
	defer t.Stop()
	for range t.C {
		if c.closed.Load() {
			return
		}
		if err := c.writeJSON(map[string]any{"type": "ping"}); err != nil {
			c.fail(err)
			return
		}
	}
}

func (c *remoteConn) writeJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.ws.Write(ctx, websocket.MessageText, b)
}

// open subscribes to one session and waits for its snapshot. 一次只有一个 open 在飞：
// 两次并发换会话会让两份快照落到同一个等待者手里，而后一份属于另一段对话。
func (c *remoteConn) open(sessionID string) (RemoteSnapshotVO, error) {
	var empty RemoteSnapshotVO
	c.openMu.Lock()
	defer c.openMu.Unlock()
	for {
		select {
		case <-c.snaps:
			continue
		case <-c.openErr:
			continue
		default:
		}
		break
	}
	if c.closed.Load() {
		return empty, errors.New("连接已经断开")
	}
	c.subMu.Lock()
	c.lastSessionID = sessionID
	c.subMu.Unlock()
	if err := c.writeJSON(map[string]any{"type": "open", "sessionId": sessionID}); err != nil {
		return empty, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), remoteOpenTimeout)
	defer cancel()
	select {
	case snap := <-c.snaps:
		return snap, nil
	case why := <-c.openErr:
		return empty, errors.New(why)
	case <-c.done:
		return empty, errors.New("连接已经断开")
	case <-ctx.Done():
		return empty, errors.New("等快照等太久了")
	}
}

// remoteReconnect is how a dropped connection comes back: bounded retries with backoff, nothing
// replayed. It is deliberately not a loop that lives forever — after the budget is spent the page
// is told `disconnected` and the reader gets the manual 重连 button back. Leaving the page, or
// switching machines, cancels it (both go through cancelRemoteRetry).
func (d *desktopApp) reconnectRemote(hostID, why string) {
	d.remoteMu.Lock()
	if d.remoteRetryCancel != nil {
		d.remoteRetryCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.remoteRetryCancel = cancel
	d.remoteMu.Unlock()

	host, err := d.remoteHostByID(hostID)
	if err != nil {
		d.emitRemoteEvent(hostID, map[string]any{"type": "disconnected", "message": why})
		cancel()
		return
	}
	go func() {
		defer cancel()
		backoff := remoteReconnectBackoff
		for attempt := 1; attempt <= remoteReconnectAttempts; attempt++ {
			d.emitRemoteEvent(hostID, map[string]any{
				"type": "reconnecting", "attempt": attempt, "message": why,
			})
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			c, err := dialAny(d, host)
			if err != nil {
				if backoff < remoteReconnectMaxBackoff {
					backoff *= 2
					if backoff > remoteReconnectMaxBackoff {
						backoff = remoteReconnectMaxBackoff
					}
				}
				continue
			}
			if ctx.Err() != nil {
				c.close()
				return
			}
			d.remoteMu.Lock()
			if ctx.Err() != nil {
				d.remoteMu.Unlock()
				c.close()
				return
			}
			d.remoteConn = c
			d.remoteRetryCancel = nil
			d.remoteMu.Unlock()
			go c.readLoop()
			go c.pingLoop()
			d.emitRemoteEvent(hostID, map[string]any{"type": "reconnected", "attempt": attempt})
			return
		}
		d.emitRemoteEvent(hostID, map[string]any{"type": "disconnected", "message": why, "final": true})
	}()
}

// cancelRemoteRetry stops a reconnection in flight. Every path that decides "we are done with
// this machine" goes through it: leaving the page, removing the machine, switching to another.
func (d *desktopApp) cancelRemoteRetry() {
	d.remoteMu.Lock()
	cancel := d.remoteRetryCancel
	d.remoteRetryCancel = nil
	d.remoteMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// emitRemoteEvent is the one way a connection-shaped event reaches the page: it stamps the
// machine on it, because a window may have switched machines between the event and its handler.
func (d *desktopApp) emitRemoteEvent(hostID string, ev map[string]any) {
	ev["hostId"] = hostID
	d.emit(remoteEventName, ev)
}
