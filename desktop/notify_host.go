package main

import "time"

// notifierHost is how the RUNTIME reaches the native notification centre. It exists so the
// runtime files (agent_turn.go, permission.go, commands.go, asks.go) compile in a process that has
// no notification centre at all — the headless host, which runs on a machine whose owner is
// holding the phone.
//
// The windowed half is *notifier (notify.go); the headless half is noNotifier below. A nil
// *notifier is also a valid member — every one of its methods tolerates nil, which is what lets
// tests run turn code without wiring a notification service.
type notifierHost interface {
	// focused reports whether the app is the foreground application right now. It is half of the
	// "is the user looking at this conversation?" rule (see watching); the other half, which
	// conversation is on screen, the caller supplies.
	focused() bool
	notifyAsk(sessionID string, questions []string)
	notifyTurnDone(sessionID string, iterations int, took time.Duration)
	notifyTurnFailed(sessionID string, err error)
	notifyCronDone(sessionID, jobName string)
	notifyCronFailed(sessionID, jobName string)
	notifyOneOffDone(sessionID, kind string, findings int, outcome string)
	notifyPermission(sessionID, command string)
}

// noNotifier is what a process without a window uses. Everything is a no-op and nothing is ever
// focused: a headless host has no "foreground application" to be, so every event counts as
// "the user is looking elsewhere". That answer is only read to decide whether to interrupt — and
// with no notification centre wired up, there is nothing to interrupt with. Callers stay identical
// to the windowed build, which is the point.
type noNotifier struct{}

func (noNotifier) focused() bool                                { return false }
func (noNotifier) notifyAsk(string, []string)                   {}
func (noNotifier) notifyTurnDone(string, int, time.Duration)    {}
func (noNotifier) notifyTurnFailed(string, error)               {}
func (noNotifier) notifyCronDone(string, string)                {}
func (noNotifier) notifyCronFailed(string, string)              {}
func (noNotifier) notifyOneOffDone(string, string, int, string) {}
func (noNotifier) notifyPermission(string, string)              {}

// compositeNotifier sends one event to every destination this process has.
//
// 两个目的地是同一件事的不同终点：窗口有前台时由系统通知中心叫一声，人不在电脑前时由
// 手机上的推送叫一声。调用点只认这一个接口（见 agent_turn.go 等处），所以加一种送达方式
// 不需要碰运行时的任何一行。
//
// focused is answered by whichever part knows about the window. The pusher always says false
// (it has no window), so the answer stays the windowed notifier's.
type compositeNotifier []notifierHost

func (c compositeNotifier) focused() bool {
	for _, n := range c {
		if n.focused() {
			return true
		}
	}
	return false
}

func (c compositeNotifier) notifyAsk(sessionID string, questions []string) {
	for _, n := range c {
		n.notifyAsk(sessionID, questions)
	}
}

func (c compositeNotifier) notifyTurnDone(sessionID string, iterations int, took time.Duration) {
	for _, n := range c {
		n.notifyTurnDone(sessionID, iterations, took)
	}
}

func (c compositeNotifier) notifyTurnFailed(sessionID string, err error) {
	for _, n := range c {
		n.notifyTurnFailed(sessionID, err)
	}
}

func (c compositeNotifier) notifyCronDone(sessionID, jobName string) {
	for _, n := range c {
		n.notifyCronDone(sessionID, jobName)
	}
}

func (c compositeNotifier) notifyCronFailed(sessionID, jobName string) {
	for _, n := range c {
		n.notifyCronFailed(sessionID, jobName)
	}
}

func (c compositeNotifier) notifyOneOffDone(sessionID, kind string, findings int, outcome string) {
	for _, n := range c {
		n.notifyOneOffDone(sessionID, kind, findings, outcome)
	}
}

func (c compositeNotifier) notifyPermission(sessionID, command string) {
	for _, n := range c {
		n.notifyPermission(sessionID, command)
	}
}

// notifyHost is the notifier every runtime call site goes through.
//
// The nil check is not decoration: a *desktopApp built by a struct literal (tests do this, and the
// old concrete *notifier tolerated being nil because each method checked for it) has a nil
// INTERFACE here, and calling a method on that panics. Answering with a silent host keeps that
// old property across the interface boundary.
func (d *desktopApp) notifyHost() notifierHost {
	if d.notify == nil {
		return noNotifier{}
	}
	return d.notify
}
