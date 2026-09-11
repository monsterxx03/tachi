package main

// Native notifications for the two moments the app is waiting on the user while
// they are looking somewhere else: a turn has finished, or the agent is parked
// on an AskUserQuestion.
//
// Both are posted ONLY while the window does not have focus. A notification for
// something you are already watching is noise — the transcript, the status
// badge and the menu-bar item all say the same thing — so focus, not the turn
// itself, is what decides.
//
// The notification service is used directly instead of being registered as an
// application service: Wails' own ServiceStartup returns an error on a system
// or build where notifications cannot be registered (a dev build launched
// outside the .app bundle, for instance) and a failing service aborts
// application startup. Trading "no notifications" for "the app will not start"
// is never the right call, and nothing is lost — the Objective-C side
// initialises its delegate lazily on the first call (ensureDelegateInitialized).

import (
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"
)

// Notification copy is fixed rather than configurable, and deliberately terse:
// one short line saying WHAT happened, never a quote of what the model said. A
// notification here is a "come back to the window" signal — the reply, the
// question text and every detail are one click away, and quoting them would turn
// a glanceable ping into a wall of text.
const (
	notifyAppTitle   = "Tachi"
	notifyAskTitle   = "Tachi 需要你的回答"
	notifyAskWaiting = "等待你的回答"
	notifyTurnDone   = "回合完成"
	// notifySubtitleMaxRune keeps a long session title from pushing the body out
	// of the visible area.
	notifySubtitleMaxRune = 40
)

type notifier struct {
	svc    *notifications.NotificationService
	window *application.WebviewWindow
	// seq makes every notification ID unique, so consecutive ones stack in
	// Notification Center instead of replacing each other.
	seq atomic.Uint64
}

func newNotifier(window *application.WebviewWindow) *notifier {
	return &notifier{svc: notifications.New(), window: window}
}

// requestAuthorization asks for notification permission once, at startup, in a
// goroutine. Startup rather than "just in time" on purpose: the prompt itself
// would otherwise appear while the user is in another app, which is exactly the
// moment the notification is meant to be unobtrusive. macOS asks at most once —
// when the user has already denied it, a repeat request is ignored, so a failure
// here is only logged.
func (n *notifier) requestAuthorization() {
	if n == nil {
		return
	}
	if granted, err := n.svc.CheckNotificationAuthorization(); err == nil && granted {
		return
	}
	granted, err := n.svc.RequestNotificationAuthorization()
	switch {
	case err != nil:
		log.Printf("desktop: notifications unavailable: %v", err)
	case !granted:
		log.Printf("desktop: notifications declined by the user")
	}
}

// onNotificationClick brings the window back when a notification is actioned.
// macOS already activates an app when its notification is clicked; this makes
// sure the window is actually up and key (the app can be sitting in the menu
// bar with its window hidden).
func (n *notifier) onNotificationClick() {
	if n == nil {
		return
	}
	n.svc.OnNotificationResponse(func(result notifications.NotificationResult) {
		if result.Error != nil {
			log.Printf("desktop: notification response error: %v", result.Error)
			return
		}
		if n.window == nil {
			return
		}
		n.window.Show()
		n.window.Focus()
	})
}

// notify posts a notification unless the window has the user's attention.
// Fire-and-forget: SendNotification blocks until the OS acknowledges (up to
// 5s), and callers sit on the agent's event loop, which must not stall.
func (n *notifier) notify(title, subtitle, body string) {
	if n == nil || n.window == nil {
		return
	}
	go func() {
		if n.window.IsFocused() {
			return
		}
		opts := notifications.NotificationOptions{
			ID:       fmt.Sprintf("tachi-%d", n.seq.Add(1)),
			Title:    title,
			Subtitle: truncateRunes(subtitle, notifySubtitleMaxRune), // macOS/Linux only
			Body:     body,
			// Group them so Notification Center shows one Tachi stack.
			ThreadID: "tachi",
		}
		if err := n.svc.SendNotification(opts); err != nil {
			log.Printf("desktop: notification failed: %v", err)
		}
	}()
}

// notifyTurnDone reports a finished turn: just the fact, plus how much work it
// took — no excerpt of the reply (see the copy comment above).
func (n *notifier) notifyTurnDone(sessionTitle string, iterations int, took time.Duration) {
	n.notify(notifyAppTitle, sessionTitle, turnDoneBody(iterations, took))
}

// notifyAsk reports a turn parked on a question. The question text stays in the
// window on purpose: the body only says that the agent is blocked and how many
// questions are waiting.
func (n *notifier) notifyAsk(sessionTitle string, questions []string) {
	n.notify(notifyAskTitle, sessionTitle, askBody(len(questions)))
}

// turnDoneBody is the one-line body for a finished turn.
func turnDoneBody(iterations int, took time.Duration) string {
	if iterations <= 0 {
		return notifyTurnDone
	}
	return fmt.Sprintf("%s · %d 次迭代 · %s", notifyTurnDone, iterations, took.Round(time.Millisecond))
}

// askBody is the one-line body for a parked AskUserQuestion. The count only
// appears when there is more than one — "（1 个问题）" is noise.
func askBody(questions int) string {
	if questions > 1 {
		return fmt.Sprintf("%s（%d 个问题）", notifyAskWaiting, questions)
	}
	return notifyAskWaiting
}

// sessionTitle returns the session's display title ("" when unknown), used as
// the notification subtitle so a background session is identifiable.
func (d *desktopApp) sessionTitle(id string) string {
	d.mu.Lock()
	r := d.runs[id]
	d.mu.Unlock()
	if r == nil || r.sm == nil {
		return ""
	}
	if cur := r.sm.Current(); cur != nil {
		return cur.Title
	}
	return ""
}

// truncateRunes cuts s to at most max runes, appending an ellipsis when
// something was dropped. Rune-wise so a CJK session title is not cut
// mid-character.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return strings.TrimSpace(string(runes[:max])) + "…"
}
