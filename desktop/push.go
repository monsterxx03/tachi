package main

// 把通知推到手机上。
//
// 它是 notifierHost 的一个实现，和窗口版的系统通知（notify.go）并列：触发点完全一样，
// 判断"该不该叫醒用户"的规则也一样，只是送达的终点不同。无头宿主没有窗口，所以它只有
// 这一半 —— 这正是这个接口当初存在的理由。
//
// 一条通知要不要发，四个问题依次问：
//
//  1. 全局开关开了吗（配置里的 push.enabled）；
//  2. 这一段对话自己关掉推送了吗（会话记录上的 NotifyOff）；
//  3. 桌面前台正显示着这一段吗（watching）；
//  4. 某台手机正连着这一段吗（mobileWatching）。
//
// 任何一个"是"都意味着有人已经看到了，通知只会是重复。第 4 条没有按设备分开：一个人拿着
// 两台手机时，他正在看的就是他正在看的那一台。
//
// 发送是 fire-and-forget：它跑在自己的 goroutine 里，失败只写日志。一轮结束不能因为推送
// 服务没应答而卡住。

import (
	"context"
	"log"
	"time"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/push"
)

// pusher is the phone half of the notifier. It carries a *desktopApp rather than the narrow
// interfaces notify.go uses, because unlike the windowed notifier it is compiled into BOTH
// hosts and needs the same facts from each: the config, the session record, the window's
// focus, and the phones' subscriptions.
type pusher struct {
	desk *desktopApp
}

func newPusher(d *desktopApp) *pusher { return &pusher{desk: d} }

// focused is not this half's question. The windowed notifier answers it, and the composite
// asks that one; answering false here keeps the pusher from claiming a focus it cannot know.
func (p *pusher) focused() bool { return false }

func (p *pusher) notifyAsk(sessionID string, questions []string) {
	p.notify(sessionID, askBody(len(questions)))
}

func (p *pusher) notifyPermission(sessionID, command string) {
	p.notify(sessionID, permBody(command))
}

func (p *pusher) notifyTurnDone(sessionID string, iterations int, took time.Duration) {
	p.notify(sessionID, turnDoneBody(iterations, took))
}

func (p *pusher) notifyTurnFailed(sessionID string, err error) {
	p.notify(sessionID, turnFailedBody(err))
}

func (p *pusher) notifyCronDone(sessionID, jobName string) {
	p.notify(sessionID, cronDoneBody(jobName, true))
}

func (p *pusher) notifyCronFailed(sessionID, jobName string) {
	p.notify(sessionID, cronDoneBody(jobName, false))
}

func (p *pusher) notifyOneOffDone(sessionID, kind string, findings int, outcome string) {
	p.notify(sessionID, oneOffDoneBody(kind, findings, outcome))
}

// notify decides now, delivers in the background. The decision is taken here, on the caller's
// goroutine, so it describes the moment the event happened — the same reason notify.go's
// notify() does it that way.
func (p *pusher) notify(sessionID, body string) {
	if p == nil || p.desk == nil {
		return
	}
	go func() {
		cfg := p.desk.config()
		if cfg == nil || !cfg.Push.Wants() {
			return
		}
		if !shouldPush(
			cfg.Push.Wants(),
			p.desk.sessionNotifyOff(sessionID),
			watching(p.desk.notifyHost().focused(), p.desk.activeSessionID(), sessionID),
			p.desk.mobileWatching(sessionID),
		) {
			return
		}
		msg := push.Message{
			Title:    notifyAppTitle,
			Subtitle: truncateRunes(p.desk.sessionTitle(sessionID), notifySubtitleMaxRune),
			Body:     body,
			Group:    sessionID,
			// 点通知打开的是「那台机器上的那一段对话」：手机可能记着好几台电脑，
			// 只带会话 id 会落到一台不相干的机器上。
			Click: push.DeepLink(sessionID, machineID()),
		}
		ctx, cancel := context.WithTimeout(context.Background(), push.DefaultTimeout)
		defer cancel()
		if err := senderFor(cfg.Push).Send(ctx, msg); err != nil {
			log.Printf("push: %v", err)
		}
	}()
}

// shouldPush is the whole rule, as a pure function: a notification is for somebody who is not
// already looking. The four questions are the four ways that stops being true — the switch is
// off, this conversation was muted, the window is showing it, a phone is subscribed to it.
//
// Written out rather than inlined because the combinations are what a test can pin: each one
// alone would still "work" for the common case while breaking exactly one of the others.
func shouldPush(enabled, muted, desktopWatching, phoneWatching bool) bool {
	return enabled && !muted && !desktopWatching && !phoneWatching
}

// senderFor is rebuilt per notification rather than cached: the settings page can change the
// addresses without a restart, and building two small structs is cheaper than a cache that
// has to be invalidated.
func senderFor(cfg config.PushConfig) push.Multi {
	var out push.Multi
	if cfg.Bark.URL != "" && len(cfg.Bark.Keys) > 0 {
		out = append(out, &push.Bark{BaseURL: cfg.Bark.URL, Keys: cfg.Bark.Keys})
	}
	if cfg.Ntfy.URL != "" && cfg.Ntfy.Topic != "" {
		out = append(out, &push.Ntfy{BaseURL: cfg.Ntfy.URL, Topic: cfg.Ntfy.Topic, Token: cfg.Ntfy.Token})
	}
	return out
}
