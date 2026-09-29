package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/desktop/link"
	"github.com/monsterxx03/tachi/pkg/strutil"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// badge 是「把 desktop 的等待搬到一块小屏上」的这一半：它既是 link.Source 的实现
// （设备读到的一切、它送回来的一切），也照看那条链路（设备拔了插了、脚本连了断了、
// 设置页把传输方式改了，都要自己站起来）。
//
// 目标来自 config 的 `badge` 块，TACHI_BADGE 环境变量覆盖它——覆盖的方向是刻意的：
// 环境变量是临时通道（比如把 app 指到一台假设备上），不该被文件里的持久设置挡住。
const (
	// envBadge 的取值与 config 的 badge.transport/address 同构：
	//
	//	serial             打开自动找到的 Espressif 串口设备
	//	serial:/dev/cu.…   打开指定串口
	//	tcp:127.0.0.1:8765 监听一个本地端口，等设备或脚本连上来
	//
	// 未设置即走 config；两边都是空 = 关闭。
	envBadge = "TACHI_BADGE"
	// envBadgeBaud 只在真实 UART 上有意义（USB-Serial-JTAG 是 USB CDC，忽略它）。
	envBadgeBaud = "TACHI_BADGE_BAUD"
)

// badgeLog 是这条链路的日志出口。
//
// 走 pkg/logger 而不是标准库的 log：标准库那几行只进 stderr，而 app 是（从 Finder 或
// 从 .app）启动的——那里没有看客，于是「链路什么时候连上、什么时候断」不留任何痕迹，
// 一次真机故障就只剩猜（这条链路的第一起「屏幕停在最后一帧」就是这么变成悬案的）。
//
// 是函数而不是包级变量：`logger.Init` 在 main 里才调，而包级变量在那之前就构造完了
// ——那时还没有日志目录，`New` 会把行静默丢进 io.Discard（它的文档就写着 Init 必须先
// 调用）。每次现取很便宜：writer 按路径缓存。
func badgeLog() *logger.Logger { return logger.New("desktop") }

// badgeRetry 是两次重连之间的间隔。设备不在线是常态（线拔了、设备正在跑别的固件），
// 所以这个循环必须便宜且安静。
const badgeRetry = 2 * time.Second

// 连上不到这么久就断，说明设备在**复位**而不是"说完话走了"：打开串口这件事本身
// 就会踢它一脚（见 link/serial.go 的 InitialStatusBits），而它重启时 USB 会重新
// 枚举、句柄失效。这时如果还按 badgeRetry 重开，就是一脚接一脚地把设备按在复位
// 循环里——屏幕上表现为背光一闪一闪。退得久一点，让它有时间启动完。
const badgeFlapWindow = 5 * time.Second
const badgeFlapBackoff = 10 * time.Second

// applyBadgeLink 把设备链路指向新的设置：停掉旧的，按新配置起一条。
//
// 它是 applyPush 而不是 applyLazy：链路的 listener 是建立时就定型的（串口只有一个
// 所有者），改设置只能停旧起新。旧的那条连接会断开，设备那边表现为一次重连。
func applyBadgeLink(d *desktopApp, cfg *config.Config) error {
	d.startBadge(cfg)
	return nil
}

// startBadge 起（或重起）设备链路：启动时一次，设置页保存后一次。
//
// 它做的第一件事是撤掉上一条——一个进程里同时开两条链路，就是两个 Server 抢同一个
// 串口，而串口只有一个赢家。**这条不变量由我们自己守**（linkGate + 先换后关），不是
// 靠操作系统拒绝第二次打开：串口和 TCP 会拒绝，BLE 不会，而两个 Server 就是两本 ref
// 账——同一条等待会被登记两次，两条都发出去，其中一条按下去没有任何反应。
func (d *desktopApp) startBadge(cfg *config.Config) {
	// openBadgeLink 放在锁外：串口的 open 可能阻塞，不该占着这把锁（startBadge 只在
	// 启动和保存设置时调用，本来也不需要抢时间）。
	ln, desc := openBadgeLink(badgeTarget(cfg))
	if ln == nil {
		// 配置成 off / 写错了：这也是一次「换链路」，所以照样要停掉旧的并串行化
		// ——否则「关掉」会和一个并发的「打开」互相覆盖。
		d.linkGate.Lock()
		cancel, old := d.detachBadgeLink()
		d.linkGate.Unlock()

		stopBadgeLink(cancel, old)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())

	d.linkGate.Lock()
	oldCancel, oldLn := d.detachBadgeLink()
	d.linkMu.Lock()
	d.linkStop, d.linkLn = cancel, ln
	d.linkMu.Unlock()
	d.linkGate.Unlock()

	// 停旧的放在换完之后：谁需要被停由这一次写入决定。反过来的话，两个并发调用会各自
	// 看到「半换」的状态，于是两条都被认为要停、或者两条都不被认为要停。
	stopBadgeLink(oldCancel, oldLn)

	badgeLog().Info(ctx, "badge link: serving", "target", desc)
	go func() {
		defer ln.Close()
		d.badgeLoop(ctx, ln)
	}()
}

// stopBadge 撤掉当前链路；没在跑时是个空操作，所以设置页可以无条件调用它。
func (d *desktopApp) stopBadge() {
	d.linkGate.Lock()
	cancel, ln := d.detachBadgeLink()
	d.linkGate.Unlock()

	stopBadgeLink(cancel, ln)
}

// detachBadgeLink 摘掉当前链路并返回它，调用方负责停。必须在 linkGate 里调用。
func (d *desktopApp) detachBadgeLink() (context.CancelFunc, link.Listener) {
	d.linkMu.Lock()
	defer d.linkMu.Unlock()

	cancel, ln := d.linkStop, d.linkLn
	d.linkStop, d.linkLn = nil, nil
	return cancel, ln
}

// stopBadgeLink 停掉一条已经摘下来的链路。
//
// 先 cancel 再 Close：cancel 是让服务循环知道「该走了」，Close 是把阻塞在 Accept 里的
// listener 叫醒。反过来做的话，那个循环会先看到一次「设备连不上」的错误日志——而真相
// 是我们要它停，日志会把下一次排查引到错的地方。
func stopBadgeLink(cancel context.CancelFunc, ln link.Listener) {
	if cancel != nil {
		cancel()
	}
	if ln != nil {
		_ = ln.Close()
	}
}

// badgeTarget 读出链路要连到哪儿：config 是持久设置，TACHI_BADGE 覆盖它。
func badgeTarget(cfg *config.Config) (transport, address string) {
	if spec := strings.TrimSpace(os.Getenv(envBadge)); spec != "" {
		kind, arg, _ := strings.Cut(spec, ":")
		return kind, arg
	}
	if cfg == nil {
		return "", ""
	}
	return strings.TrimSpace(cfg.Badge.Transport), strings.TrimSpace(cfg.Badge.Address)
}

// openBadgeLink 按 (transport, address) 打开一条链路；没配置就返回 nil。
//
// 配置错了（拼错传输方式、地址为空、端口被占）只打一行日志并返回 nil：设备链路是
// 一个可选的附加物，它连不上不该让 desktop 起不来，也不该让一次设置保存失败。
func openBadgeLink(transport, address string) (link.Listener, string) {
	switch transport {
	case "", "off":
		return nil, ""
	case "serial":
		where := address
		if where == "" {
			where = "（自动查找）"
		}
		return link.NewSerialListener(address, badgeBaud()), "serial " + where
	case "tcp":
		if address == "" {
			badgeLog().Warn(context.Background(),
				"badge link: tcp needs an address (e.g. 127.0.0.1:8765)")
			return nil, ""
		}
		ln, err := link.ListenTCP(address)
		if err != nil {
			badgeLog().Error(context.Background(), "badge link: cannot listen", err,
				"address", address)
			return nil, ""
		}
		return ln, "tcp " + address
	case "ble":
		// address 这里是广播名字，不是地址：macOS 上 CoreBluetooth 给的是它自己那套
		// 标识（一个 UUID），而设备每次重连都不保证不变——按名字找才是稳定的，
		// 也正是用户能在系统设置里看到的那个名字。
		where := address
		if where == "" {
			where = link.DefaultBadgeName
		}
		return link.NewBLEListener(address), "bluetooth " + where
	}
	badgeLog().Warn(context.Background(),
		"badge link: unknown transport (want off, serial, tcp or ble)", "transport", transport)
	return nil, ""
}

func badgeBaud() int {
	baud, _ := strconv.Atoi(os.Getenv(envBadgeBaud))
	return baud
}

func (d *desktopApp) badgeLoop(ctx context.Context, ln link.Listener) {
	var lastErr string
	for ctx.Err() == nil {
		tr, err := ln.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// 同一个原因只打一次：设备不在线是常态，逐次重试都打日志会刷屏。
			if msg := err.Error(); msg != lastErr {
				lastErr = msg
				badgeLog().Info(ctx, "badge link: waiting for device", "err", err)
			}
			if !sleepCtx(ctx, badgeRetry) {
				return
			}
			continue
		}

		lastErr = ""
		badgeLog().Info(ctx, "badge link: device connected")
		connectedAt := time.Now()
		err = link.NewServer(d, tr).Run(ctx)
		_ = tr.Close()
		if ctx.Err() != nil {
			return
		}

		wait := badgeRetry
		if time.Since(connectedAt) < badgeFlapWindow {
			wait = badgeFlapBackoff
			badgeLog().Warn(ctx, "badge link: device reset right after connecting — "+
				"likely the open itself pulsed DTR/RTS; backing off",
				"since", time.Since(connectedAt).Round(time.Millisecond), "backoff", wait)
		} else {
			badgeLog().Info(ctx, "badge link: device disconnected", "err", err)
		}
		if !sleepCtx(ctx, wait) {
			return
		}
	}
}

// badgeAlertBacklog 是攒着的提示音请求的上限。
//
// 有上限是为了「一条迟到的通报没有意义」：链路断了又回来时，攒了一小时的十声提示音
// 只是把用户吵十次。满了就丢最旧的——最新那条才是此刻的事。
const badgeAlertBacklog = 4

// enqueueAlert 攒下一次提示音请求，并叫醒 link 层去取（TakeAlerts）。
func (d *desktopApp) enqueueAlert(kind string) {
	d.mu.Lock()
	if len(d.alerts) >= badgeAlertBacklog {
		d.alerts = d.alerts[1:]
	}
	d.alerts = append(d.alerts, link.Alert{Kind: kind})
	d.mu.Unlock()
	d.notifyLink()
}

// TakeAlerts 实现 link.Source：把攒下的提示音请求交给 link 层并清空。
//
// 由 link 层的 Server 调用（每轮推送前一次，连上时还会丢掉一批陈的），所以它必须
// 假定自己随时被另一个 goroutine 调用。
func (d *desktopApp) TakeAlerts() []link.Alert {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.alerts) == 0 {
		return nil
	}
	out := d.alerts
	d.alerts = nil
	return out
}

// deviceDetailLimit 是设备那条副标题能收下的字节数。
//
// 设备侧是 `BADGE_DETAIL_MAX = 96`（ai-passport 的 main/badge_proto.h：状态副标题、
// 主机报错文本），而它按**字节**截断——贴着上限写就是等着切出半个汉字，屏幕上是一串
// 乱码。这里留几个字节余量，并且在自己的边界上按字符切，超出那部分本来也没人看得见。
const deviceDetailLimit = 88

// deviceDetail 把一条状态副标题裁到设备收得下的长度。
func deviceDetail(s string) string {
	return strutil.TruncateBytes(s, deviceDetailLimit)
}

// Snapshot 实现 link.Source。
//
// 只发非空闲的会话：设备要回答的问题是「谁在等我」，而一个桌面上躺着的几十个空闲
// 会话只是噪声。会话按 ID 排序——同一份内容每次都要长得一样，link 层的去抖才有意义。
func (d *desktopApp) Snapshot() link.Snapshot {
	d.mu.Lock()
	sessions := make([]link.Session, 0, len(d.runs))
	for id, r := range d.runs {
		if r.state.Status == StatusIdle {
			continue
		}
		sessions = append(sessions, link.Session{
			ID:     id,
			Status: string(r.state.Status),
			Label:  r.state.Label,
			Detail: deviceDetail(r.state.Detail),
		})
	}
	d.mu.Unlock()

	// 标题在锁外取：sessionTitle 自己加锁，而且在锁里再去找 session manager 是没必要的。
	active := d.currentID()
	for i := range sessions {
		sessions[i].Title = d.sessionTitle(sessions[i].ID)
	}
	// 正在屏幕上显示的那个排最前。设备的状态屏拿「第一个」当「现在这个」，而按 ID
	// 排序会把最早创建的会话摆上去——会话 ID 带时间戳，所以那句话的顺序是反的，
	// 设备上会一直显示一个几小时前的会话。（有待答项的会话由设备侧另行优先。）
	sort.SliceStable(sessions, func(i, j int) bool {
		if (sessions[i].ID == active) != (sessions[j].ID == active) {
			return sessions[i].ID == active
		}
		return sessions[i].ID < sessions[j].ID
	})

	pendings := d.snapshotAsks()
	asks := make([]link.Ask, 0, len(pendings))
	for _, a := range pendings {
		asks = append(asks, askFor(a))
	}
	return link.Snapshot{Sessions: sessions, Asks: asks}
}

// askFor 把一条待答项翻译成设备看得懂的形状。翻译只在这一处发生：设备不该知道
// pendingAsk 长什么样，也不该知道 allow_once 是什么意思。
func askFor(a *pendingAsk) link.Ask {
	out := link.Ask{
		Key:       permKey(a.sessionID, a.toolID),
		Kind:      string(a.kind),
		SessionID: a.sessionID,
		ToolID:    a.toolID,
	}
	switch a.kind {
	case askKindPermission:
		out.Title = "Bash 确认"
		out.Body = a.preview
		out.Options = permissionOptions()
	case askKindAskUser:
		if len(a.questions) > 0 && a.questions[0].Header != "" {
			out.Title = a.questions[0].Header
		} else {
			out.Title = "一个问题"
		}
		out.Questions = a.questions
	}
	return out
}

// permissionOptions 是设备上的三个选择。标签是给人在小屏上扫的短词，机器值与
// desktop 卡片上的按钮完全一致——同一个决定，只是屏幕小得多。
func permissionOptions() []link.Option {
	return []link.Option{
		{Label: "允许一次", Value: link.DecisionAllowOnce},
		{Label: "本会话允许", Value: link.DecisionAllowSession},
		{Label: "拒绝", Value: link.DecisionDeny},
	}
}

// Answer 实现 link.Source：把设备送回来的回答交给已有的两条回答路径。
//
// 不另造一条回答通道，是这套东西能这么小的原因——设备上按下的那一下，和窗口里点
// 卡片、终端里敲一个键，最终走的是同一个函数，因此不可能有三种行为。
func (d *desktopApp) Answer(a link.Answer) error {
	switch a.Kind {
	case string(askKindPermission):
		return d.answerPermission(a.SessionID, a.ToolID, a.Decision)
	case string(askKindAskUser):
		return d.answerAskUser(a.SessionID, a.Answers, a.Annotations)
	}
	return fmt.Errorf("unknown ask kind %q", a.Kind)
}

// Subscribe 实现 link.Source。
func (d *desktopApp) Subscribe(fn func()) (cancel func()) {
	d.linkMu.Lock()
	if d.linkSubs == nil {
		d.linkSubs = make(map[int]func())
	}
	d.linkSubSeq++
	id := d.linkSubSeq
	d.linkSubs[id] = fn
	d.linkMu.Unlock()

	return func() {
		d.linkMu.Lock()
		delete(d.linkSubs, id)
		d.linkMu.Unlock()
	}
}

// notifyLink 通知链路「有变化了」。
//
// 订阅者只做一件事——往自己的 channel 里塞一个信号——所以这里在放掉 linkMu 之后再
// 调用它们是安全的；也是必须的：持锁调用会让任何一个慢订阅者卡住 setSessionState，
// 而那个函数跑在 turn 自己的 goroutine 上。
func (d *desktopApp) notifyLink() {
	d.linkMu.Lock()
	if len(d.linkSubs) == 0 {
		d.linkMu.Unlock()
		return
	}
	subs := make([]func(), 0, len(d.linkSubs))
	for _, fn := range d.linkSubs {
		subs = append(subs, fn)
	}
	d.linkMu.Unlock()

	for _, fn := range subs {
		fn()
	}
}

// sleepCtx 等一段时间，ctx 取消立刻返回 false。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
