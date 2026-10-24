package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/pkg/logger"
)

// 保持唤醒：让这台机器不要因为闲置而睡。
//
// 它是**一台机器**的开关，不是一段会话的开关，所以它住在进程里而不是某段会话的记录里；托盘菜单
// （菜单栏 / 状态栏图标）和手机是同一个开关的两块屏，协议见 mobile-protocol.md 的 awake_changed。
//
// 三条不变量：
//
//  1. **断言跟着进程走**。macOS 的 IOPMAssertion 与 logind 的 inhibitor fd 都在进程消失时由内核
//     收回，所以崩溃、被杀、断电都不会把一台机器永久钉在醒着 —— 这也正是不把「开着」写进磁盘的
//     理由：进程不在了，那个状态就不再是真的。
//  2. **只有一条写入路径**（desktopApp.setAwake）。托盘、手机、到点自动关都从这里过，所以两块屏
//     不可能各说一套。广播也只从这里发（emitAwakeChanged）。
//  3. **时长是偏好，落盘**（desktop_ui.json 的 awakeMinutes，默认 3 小时，0 = 不自动关）。它跟
//     着这台机器走，与「现在开着没有」无关 —— 后者每次启动都是关的。
//
// 挡不住的事，说清而不是假装：合上盖子（没有外接屏时）由硬件与系统决定，任何用户态断言都拦不了；
// Linux 上屏幕变黑是桌面自己的守护进程（Hyprland 下是 hypridle）干的，这里只挡「机器睡」。

// 时长的默认与边界。下限是「短到没有意义」，上限是一天：再长就不如选「不自动关」。
const (
	awakeDefaultMinutes = 3 * 60
	awakeMinMinutes     = 5
	awakeMaxMinutes     = 24 * 60
)

// awakeWhy 是系统那边记下的理由（`systemd-inhibit --list` 的 Why 一栏）：用户得能看出是谁在挡着，
// 只写进程名不够。macOS 那半由 caffeinate 自己署名，所以这串字只被 logind 用 —— 纯 ASCII 是刻意的：
// 它要出现在任何 locale 的终端里都一样可读。
const awakeWhy = "Tachi: keep this machine awake"

// errAwakeDuration is the refusal for a length outside the bounds above. It is the caller's
// "you asked for something that is not a duration", not "the machine refused" — the two reach
// the phone as different codes.
var errAwakeDuration = errors.New("保持唤醒的时长不合法")

// awakeState is the switch as every screen reads it.
type awakeState struct {
	On bool `json:"on"`
	// DurationMin is the PREFERENCE: how long this 开启 (and the next one) lasts. 0 = 不自动关.
	DurationMin int `json:"durationMin"`
	// Until is when the current 开启 turns itself off (RFC3339, UTC). Empty when it is off, or
	// when the preference is 不自动关.
	Until string `json:"until"`
}

// sleepGrant is one live "do not sleep" assertion. release is called once, from the goroutine
// that turned the switch off (or the timer that expired it); both platform halves release the
// kernel object they hold, which is what makes the state disappear with this process.
type sleepGrant interface {
	release()
}

// awakeController owns the switch. It is a value the app creates once, and it is deliberately
// independent of the window: a headless host (a NAS) has the same switch and is driven by the
// phone.
type awakeController struct {
	mu   sync.Mutex
	kind sleepGrant // nil = not asserting anything right now
	// until is the deadline the current 开启 carries; zero = no deadline.
	until time.Time
	// durationMin is the preference (see awakeState).
	durationMin int
	// gen tells a timer callback whether it still owns the switch. Every change bumps it, so a
	// timer scheduled for a state that has since been replaced does nothing (a stale callback
	// that released a live assertion would turn the switch off under the user's feet).
	gen   int
	timer *time.Timer

	// acquire / after are the platform seam. They are fields rather than direct calls so a test
	// can hold a fake assertion and fire the deadline by hand — a real one would keep the
	// machine that runs the tests awake for three hours.
	acquire func(why string) (sleepGrant, error)
	after   func(time.Duration, func()) *time.Timer

	// onChange is called WITHOUT c.mu held, after every real change: it repaints the tray menu
	// and broadcasts to the phones.
	onChange func(awakeState)
}

func newAwakeController(onChange func(awakeState)) *awakeController {
	return &awakeController{
		durationMin: loadAwakeMinutes(),
		acquire:     acquireSleep,
		after:       time.AfterFunc,
		onChange:    onChange,
	}
}

// state is the switch as every screen should see it.
func (c *awakeController) state() awakeState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stateLocked()
}

func (c *awakeController) stateLocked() awakeState {
	st := awakeState{On: c.kind != nil, DurationMin: c.durationMin}
	if st.On && !c.until.IsZero() {
		st.Until = c.until.UTC().Format(time.RFC3339)
	}
	return st
}

// set is the ONE writer: on is the switch itself, minutes (when not nil) is the new preference,
// which also becomes this 开启's length.
//
// A failure answers with the state the machine is ACTUALLY in (off) rather than with what was
// asked for: the screens draw the answer, and a switch that shows "on" while nothing holds the
// machine up is the one lie this feature must not tell.
func (c *awakeController) set(on bool, minutes *int) (awakeState, error) {
	c.mu.Lock()

	if minutes != nil {
		if !validAwakeMinutes(*minutes) {
			st := c.stateLocked()
			c.mu.Unlock()
			return st, errAwakeDuration
		}
	}
	if minutes != nil && *minutes != c.durationMin {
		c.durationMin = *minutes
		// The preference is written outside the lock (the file write is slow and nobody else
		// needs it), but the value is captured here so a later change cannot be stored twice.
		saved := c.durationMin
		defer saveAwakeMinutes(saved)
	}

	if !on {
		c.releaseLocked()
		c.gen++
		c.stopTimerLocked()
		st := c.stateLocked()
		c.mu.Unlock()
		c.notify(st)
		return st, nil
	}

	if c.kind == nil {
		kind, err := c.acquire(awakeWhy)
		if err != nil {
			// Nothing is held, so the answer is "off" plus the reason. A machine whose logind
			// refuses must not look like it is being kept awake.
			st := c.stateLocked()
			c.mu.Unlock()
			logger.New("desktop").Error(context.Background(), "awake: the operating system refused the assertion", err)
			return st, err
		}
		c.kind = kind
	}
	c.gen++
	c.armLocked()
	st := c.stateLocked()
	c.mu.Unlock()
	c.notify(st)
	return st, nil
}

// armLocked (re)schedules the auto-off for the current generation. Called with c.mu held.
func (c *awakeController) armLocked() {
	c.stopTimerLocked()
	if c.durationMin == 0 {
		c.until = time.Time{}
		return
	}
	d := time.Duration(c.durationMin) * time.Minute
	c.until = time.Now().Add(d)
	gen := c.gen
	c.timer = c.after(d, func() { c.expire(gen) })
}

// expire is the deadline firing — the only path that turns the switch off without the user
// asking. It ignores a callback whose generation is stale (see awakeController.gen).
func (c *awakeController) expire(gen int) {
	c.mu.Lock()
	if gen != c.gen {
		c.mu.Unlock()
		return
	}
	c.releaseLocked()
	c.until = time.Time{}
	c.timer = nil
	c.gen++
	st := c.stateLocked()
	c.mu.Unlock()
	c.notify(st)
}

func (c *awakeController) releaseLocked() {
	if c.kind != nil {
		c.kind.release()
		c.kind = nil
	}
}

func (c *awakeController) stopTimerLocked() {
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
}

func (c *awakeController) notify(st awakeState) {
	if c.onChange != nil {
		c.onChange(st)
	}
}

// validAwakeMinutes accepts a real length or the explicit 不自动关 (0). Bounds are here because
// the value comes from two screens and from a hand-editable file: an hour count nobody meant is
// worse than the default.
func validAwakeMinutes(m int) bool {
	return m == 0 || (m >= awakeMinMinutes && m <= awakeMaxMinutes)
}

// loadAwakeMinutes is the stored preference, or the default when it was never set (or a hand
// edit left something no timer could honour). "Never set" and "不自动关" are different answers,
// which is why the file keeps a pointer.
func loadAwakeMinutes() int {
	st := loadUIState()
	if st.AwakeMinutes == nil || !validAwakeMinutes(*st.AwakeMinutes) {
		return awakeDefaultMinutes
	}
	return *st.AwakeMinutes
}

func saveAwakeMinutes(m int) {
	if !validAwakeMinutes(m) {
		return
	}
	updateUIState(func(st *uiState) bool {
		if st.AwakeMinutes != nil && *st.AwakeMinutes == m {
			return false
		}
		v := m
		st.AwakeMinutes = &v
		return true
	})
}

// awakeState is the switch as any screen reads it (the phone's GET /v1/awake).
func (d *desktopApp) awakeState() awakeState { return d.awake.state() }

// setAwake is the one writer every entry point goes through (tray menu, phone, the auto-off
// timer's own broadcast). See awakeController.set.
func (d *desktopApp) setAwake(on bool, minutes *int) (awakeState, error) {
	return d.awake.set(on, minutes)
}

// onAwakeChanged repaints the two surfaces the switch appears on. It runs without the
// controller's lock held, so both halves are free to take their own.
func (d *desktopApp) onAwakeChanged(st awakeState) {
	d.applyAwakeUI(st)
	d.mobileBroadcast(map[string]any{"type": mobileEvAwakeChanged, "awake": st})
}
