package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/desktop/link"
	"github.com/monsterxx03/tachi/desktop/stt"
	"github.com/monsterxx03/tachi/pkg/audio"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/pkg/strutil"
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

// setBadgeLink 记住（或忘掉）当前那条链路。
//
// 它存在的理由只有一条：转写完成后要把回执写回设备，而转写跑在链路自己的 goroutine
// 之外——那条 goroutine 里局部变量的 Server 从别处拿不到。
func (d *desktopApp) setBadgeLink(s *link.Server) {
	d.mu.Lock()
	d.badgeLink = s
	d.mu.Unlock()
}

func (d *desktopApp) badgeSender() *link.Server {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.badgeLink
}

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
// 靠操作系统拒绝第二次打开：BLE 不会（两个 central 能同时连上同一个 peripheral），而两个 Server 就是两本 ref
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
		"badge link: unknown transport (want off, serial or ble)", "transport", transport)
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
		srv := link.NewServer(d, tr)
		d.setBadgeLink(srv)
		// P0-①：量链路的可持续吞吐。**原始**字节/秒才是决定采样率常量的那个数——
		// 线路上的字节（base64 涨 4/3，再加 JSON 骨架）是它的 1.4 倍还多，两个数混在
		// 一起读会把人误导到「链路撑不住」上去。
		srv.OnVoice = func(u link.VoiceUpload) {
			secs := u.Duration.Seconds()
			kbps := 0.0
			if secs > 0 {
				kbps = math.Round(float64(len(u.Payload))/secs/1024*10) / 10
			}
			fields := []any{
				"id", u.ID,
				"codec", u.Codec,
				"hz", u.Hz,
				"bytes", len(u.Payload),
				"declared", u.Declared,
				"parts", u.GotParts,
				"parts_reported", u.Parts,
				"ms", u.Duration.Milliseconds(),
				"kbps", kbps,
			}
			if u.Err != nil {
				// 失败也要打全：半截上传的字节数与耗时同样是「链路能跑多快」的证据
				// （它多半就是跑到一半被追上/丢掉的那一次）。
				badgeLog().Warn(ctx, "badge voice: upload FAILED", append(fields, "err", u.Err)...)
				return
			}
			badgeLog().Info(ctx, "badge voice: upload measured", fields...)
			// 转写放另一个 goroutine：这里跑在链路那条 goroutine 上（link 的 handle），
			// 而识别要几百毫秒——挡住它，这条链路上同时到的 ask 也得跟着等。
			go d.transcribeVoice(ctx, u)
		}
		err = srv.Run(ctx)
		d.setBadgeLink(nil) // 这条链路已经没了：回执不该再往它上面写
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

// transcribeVoice 把一次语音上传解成样本、转成一句话。这是 P1 的主机侧那一半。
//
// 链路那一步（攒字节）已经做完（link/voice.go），这里接手的是**语义**：把 8 KB/s 的
// IMA ADPCM 还原成 16 kHz 的样本，再让 macOS 自己的识别引擎转成文字（本机、不联网）。
//
// 现在只写到日志：这句话该怎么**落到那个会话里**是下一步（设计文档 §7：走前端那条路，
// 空闲就发、在跑就排队等 steer 点）。先能看见文字，才知道采集那头的声学到底行不行——
// 那是这套方案在真机上最后一个没验过的东西。
func (d *desktopApp) transcribeVoice(ctx context.Context, u link.VoiceUpload) {
	hz := u.Hz
	if hz <= 0 {
		hz = 16000 // 设备没报就用设计里那一档，而不是让识别拿到 0 采样率
	}
	pcm := audio.DecodeADPCM(u.Payload)
	if len(pcm) == 0 {
		badgeLog().Warn(ctx, "badge voice: nothing to transcribe", "bytes", len(u.Payload))
		return
	}
	started := time.Now()
	text, err := stt.Transcribe(pcm, hz)
	if err != nil {
		// 「这台机器做不了」要能被看见（设计文档 §7：没能力不能装死）。P1 接上会话
		// 之后，这里还会变成设备屏幕上的一个 error。
		badgeLog().Warn(ctx, "badge voice: transcription failed", "err", err)
		return
	}
	// 空文本不是错误：那多半是「按下了但没说话」，而识别如实回答了。
	badgeLog().Info(ctx, "badge voice: transcribed",
		"samples", len(pcm), "ms", time.Since(started).Milliseconds(), "chars", len([]rune(text)),
		"text", text)

	// 投递：交给**前端**，因为它才知道哪个会话是「当前」的（设计文档 §2：设备手上没有
	// 这个信息，所以协议里也刻意没有会话 id）。前端把它送进 composer 那条既有的路
	// ——空闲时开一个 turn、在跑时进队列等 steer 点——所以「按住说话」和「敲回车」在
	// 后端眼里是同一件事，没有第二条投递路径要维护。
	if strings.TrimSpace(text) == "" {
		// 不投一句空话进会话：那只会让人以为设备发错了东西。
		badgeLog().Info(ctx, "badge voice: 没有可投递的文本（按下了但没说话？）")
		return
	}
	d.app.Event.Emit("badge:voice", map[string]any{"text": text})

	// 回执：设备屏幕上要显示「这句话是什么、到哪了」（设计文档 §5 的 voice_text）。
	// state 由这里算——「会话在不在跑」是后端知道的事实，而「哪个会话是当前的」是前端的
	// 选择（它通过 ActivateSession 同步到 d.activeID），两样都在这里。
	//
	// 注意这只是**屏幕上的那句话**：真正的投递已经发出去了（上面那条事件），而前端那边
	// 的 route() 才是决定「立刻发还是排队」的地方。这里算出来的 state 与它一致，因为
	// 两边看的是同一个 running。
	state := "sent"
	active := ""
	d.mu.Lock()
	active = d.activeID
	if r := d.getRun(active); r != nil && r.running {
		state = "queued"
	}
	sender := d.badgeLink
	d.mu.Unlock()
	if sender == nil {
		// 链路在转写期间断了：没什么可回的，而设备那边会先看到「发送失败」那类提示。
		badgeLog().Warn(ctx, "badge voice: 链路已断，回执没发出去", "state", state)
		return
	}
	sender.VoiceText(u.ID, text, state)
	badgeLog().Info(ctx, "badge voice: 回执已发", "state", state, "chars", len([]rune(text)))
}
