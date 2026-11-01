//go:build !headless

package main

// Native notifications for the moments the app has news while the user is looking
// somewhere else: a turn has finished, the agent is parked on an AskUserQuestion, or a
// side-channel run (/review, /commit) is over.
//
// "Somewhere else" has two shapes, and `watching` is the single rule that names both:
// another application (the window does not have focus), and another conversation (it has
// focus, but is showing a different session). A notification for what is already on
// screen — that conversation, not merely the window — would only repeat the transcript,
// the status badge and the menu-bar item.
//
// The Wails notification service is REGISTERED with the application (`Options.Services`
// in main.go), because that is the only thing that runs its ServiceStartup — and on Linux
// that call is where the D-Bus session connection is opened. An unstarted service has a
// nil connection there, and posting through it does not fail, it PANICS godbus and takes
// the process down (which is how the app used to die on every turn end while the window
// was unfocused). Registration is not allowed to become a startup dependency, though:
// Wails aborts the whole application when a registered service's ServiceStartup returns an
// error, and it does return one on a system or build that cannot register notifications —
// a macOS build launched outside the .app bundle, a Linux session with no message bus. So
// what is registered is notificationService below, which absorbs that error (see its
// comment) and leaves silence where a notification would have been.

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"
)

// Notification copy lives in notify_text.go: it has no build tag, because the headless host
// sends the same sentences to a phone through push.go.

// notificationService owns the one Wails notification backend and the two rules
// this app adds to it: startup may not abort the process, and nothing is posted
// before it succeeded.
//
// Registration and failure tolerance pull in opposite directions, which is why
// this type exists at all. It must be registered (`Options.Services` in main.go)
// for Wails to call ServiceStartup, which is where the Linux backend opens its
// D-Bus connection. But a registered service whose ServiceStartup returns an
// error aborts the whole application, and Wails returns exactly that error on a
// system or build that cannot register notifications — so the call is made here,
// its failure is logged, and success is reported regardless. The cost is the
// notifications themselves, which `ready` records.
//
// The Wails service underneath is a package singleton — notifications.New()
// hands every caller the same instance — so the object registered with the app
// and the object the notifier posts through are one and the same.
type notificationService struct {
	svc *notifications.NotificationService
	// ready says the platform backend started. Posting before that is not a
	// dropped notification: on Linux it is the nil-connection panic the package
	// comment describes, so the gate lives HERE, inside post, where no caller
	// can forget it.
	ready atomic.Bool
}

func newNotificationService() *notificationService {
	return &notificationService{svc: notifications.New()}
}

// ServiceStartup runs the platform startup and absorbs its error; see the type
// comment. Wails calls it only because the instance is registered.
func (s *notificationService) ServiceStartup(ctx context.Context, options application.ServiceOptions) error {
	if err := s.svc.ServiceStartup(ctx, options); err != nil {
		log.Printf("desktop: notifications unavailable: %v", err)
		return nil
	}
	s.ready.Store(true)
	return nil
}

// ServiceShutdown releases what a successful startup opened (on Linux, the
// D-Bus connection). Wails pairs it with a startup that did not report an error
// — which, for this wrapper, includes the failures it swallows — so the gate is
// what keeps it from closing a backend that was never opened.
func (s *notificationService) ServiceShutdown() error {
	if !s.ready.Load() {
		return nil
	}
	return s.svc.ServiceShutdown()
}

// post sends one notification, or does nothing when the backend never started.
// A backend that cannot deliver is silent rather than fatal here, and the reason
// is already in the log line from ServiceStartup.
func (s *notificationService) post(options notifications.NotificationOptions) {
	if !s.ready.Load() {
		return
	}
	if err := s.svc.SendNotification(options); err != nil {
		log.Printf("desktop: notification failed: %v", err)
	}
}

// The remaining three are pass-throughs, so that the Wails service object itself
// stays inside this type. Only posting has to wait for a started backend: asking
// (or being told) about the permission touches no connection, on either platform.
func (s *notificationService) checkAuthorization() (bool, error) {
	return s.svc.CheckNotificationAuthorization()
}

func (s *notificationService) requestAuthorization() (bool, error) {
	return s.svc.RequestNotificationAuthorization()
}

func (s *notificationService) onResponse(callback func(notifications.NotificationResult)) {
	s.svc.OnNotificationResponse(callback)
}

// notificationTarget is the part of the app the notifier reasons about: what the window is
// showing, what to call that conversation, and how to take the reader to one. An interface
// rather than a *desktopApp so this file names only what it uses.
type notificationTarget interface {
	// activeSessionID is the conversation the window is showing ("" when none is loaded).
	activeSessionID() string
	// sessionTitle is a conversation's display title ("" when unknown).
	sessionTitle(sessionID string) string
	// openSession takes the reader to a conversation (see onNotificationClick).
	openSession(sessionID string)
}

// notifyRecall is one posted notification, kept only so that clicking it can go to the
// conversation it was about.
type notifyRecall struct {
	id        string
	sessionID string
}

// notifyRecallDepth bounds that memory. A notification nobody ever clicks must not
// accumulate state for the life of the process, and an id that has aged out degrades to
// what every click did before notifications named a session: bring the window forward.
const notifyRecallDepth = 64

type notifier struct {
	svc    *notificationService
	window *application.WebviewWindow
	target notificationTarget
	// seq makes every notification ID unique, so consecutive ones stack in
	// Notification Center instead of replacing each other.
	seq atomic.Uint64

	// mu guards recall, which the posting goroutine writes and the response callback reads.
	mu sync.Mutex
	// recall holds the last few posted notifications, newest last.
	recall []notifyRecall
}

func newNotifier(svc *notificationService, window *application.WebviewWindow, target notificationTarget) *notifier {
	return &notifier{svc: svc, window: window, target: target}
}

// remember records which conversation a notification was about, dropping the oldest once
// the list is full (identity of the id → session pair is what the click needs).
func (n *notifier) remember(id, sessionID string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.recall = append(n.recall, notifyRecall{id: id, sessionID: sessionID})
	if len(n.recall) > notifyRecallDepth {
		n.recall = n.recall[len(n.recall)-notifyRecallDepth:]
	}
}

// sessionForID resolves a notification's id back to the conversation it was about. "" means
// "not one of ours, or long forgotten" — the window still comes forward.
func (n *notifier) sessionForID(id string) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	for i := len(n.recall) - 1; i >= 0; i-- {
		if n.recall[i].id == id {
			return n.recall[i].sessionID
		}
	}
	return ""
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
	if granted, err := n.svc.checkAuthorization(); err == nil && granted {
		return
	}
	granted, err := n.svc.requestAuthorization()
	switch {
	case err != nil:
		log.Printf("desktop: notifications unavailable: %v", err)
	case !granted:
		log.Printf("desktop: notifications declined by the user")
	}
}

// onNotificationClick brings the window back when a notification is actioned, and goes to
// the conversation that notification was about. macOS already activates an app when its
// notification is clicked; this makes sure the window is actually up and key (the app can be
// sitting in the menu bar with its window hidden).
//
// The session half is what makes a background notification worth clicking: it named a
// conversation the reader was NOT looking at, so stopping at the window would land them
// somewhere else than the thing they clicked.
func (n *notifier) onNotificationClick() {
	if n == nil {
		return
	}
	n.svc.onResponse(func(result notifications.NotificationResult) {
		if result.Error != nil {
			log.Printf("desktop: notification response error: %v", result.Error)
			return
		}
		if n.window == nil {
			return
		}
		n.window.Show()
		n.window.Focus()
		if sessionID := n.sessionForID(result.Response.ID); sessionID != "" && n.target != nil {
			n.target.openSession(sessionID)
		}
	})
}

// notify posts a notification about one session unless the user is already looking at that
// conversation (watching). Fire-and-forget: SendNotification blocks until the OS
// acknowledges (up to 5s), and callers sit on the agent's event loop, which must not stall
// — the decision is made at posting time for the same reason, one moment after the event.
func (n *notifier) notify(sessionID, title, body string) {
	if n == nil || n.window == nil {
		return
	}
	go func() {
		if watching(n.window.IsFocused(), n.active(), sessionID) {
			return
		}
		id := fmt.Sprintf("tachi-%d", n.seq.Add(1))
		n.remember(id, sessionID)
		n.svc.post(notifications.NotificationOptions{
			ID:    id,
			Title: title,
			// The conversation's title says WHICH one this is about. With the window focused
			// on another session it is the one thing this notification can tell the reader
			// that the screen does not already say.
			Subtitle: truncateRunes(n.subtitleFor(sessionID), notifySubtitleMaxRune), // macOS/Linux only
			Body:     body,
			// Group them so Notification Center shows one Tachi stack.
			ThreadID: "tachi",
		})
	}()
}

// focused reports whether this app is the foreground application — the window half of watching
// (watching.go). A notifier built without a window (a test) is never focused, which is the
// "elsewhere" side of that rule and the safe half for a notification: a stray ping costs a beep,
// while the other mistake leaves the reader believing nothing happened.
func (n *notifier) focused() bool {
	return n != nil && n.window != nil && n.window.IsFocused()
}

// active and subtitleFor reach the window's state through the target; a notifier built
// without one (a test) has no session on screen, which is the "elsewhere" side of watching.
func (n *notifier) active() string {
	if n.target == nil {
		return ""
	}
	return n.target.activeSessionID()
}

func (n *notifier) subtitleFor(sessionID string) string {
	if n.target == nil {
		return ""
	}
	return n.target.sessionTitle(sessionID)
}

// notifyTurnDone reports a finished turn: just the fact, plus how much work it
// took — no excerpt of the reply (see the copy comment above).
func (n *notifier) notifyTurnDone(sessionID string, iterations int, took time.Duration) {
	n.notify(sessionID, notifyAppTitle, turnDoneBody(iterations, took))
}

// notifyTurnFailed reports a turn that ended in an error: a stream that died mid-turn (a
// timeout, a dropped connection), a request the provider rejected, an iteration budget that
// ran out. The reader is looking elsewhere and the reply is NOT coming, so the ping is the
// whole message — and unlike the other bodies this one carries the reason, because "what
// went wrong" is the part they would otherwise have to come back for.
//
// A stop the user asked for is NOT a failure (the caller excludes it): that is the same
// normal conclusion it is for every other frontend, and pinging about it is noise.
func (n *notifier) notifyTurnFailed(sessionID string, err error) {
	n.notify(sessionID, notifyAppTitle, turnFailedBody(err))
}

// notifyAsk reports a turn parked on a question. The question text stays in the
// window on purpose: the body only says that the agent is blocked and how many
// questions are waiting.
func (n *notifier) notifyAsk(sessionID string, questions []string) {
	n.notify(sessionID, notifyAskTitle, askBody(len(questions)))
}

// notifyPermission reports a turn parked on a bash permission decision. Unlike a
// question, the thing being decided is one command — so the body names it (the
// user is being asked to approve THAT, and "something is waiting" would send them
// back for a detail the ping could have carried).
func (n *notifier) notifyPermission(sessionID, command string) {
	n.notify(sessionID, notifyPermTitle, permBody(command))
}

// notifyOneOffDone reports a finished side-channel run (/review, /commit). It gets its own copy
// rather than notifyTurnDone for two reasons: a run has no turn on screen to look at, so while
// the reader is looking elsewhere this is the ONLY signal that it is over — and what came of it
// (a review's findings, or that it failed) is the part the reader would otherwise have to come
// back for.
func (n *notifier) notifyOneOffDone(sessionID, kind string, findings int, outcome string) {
	n.notify(sessionID, notifyAppTitle, oneOffDoneBody(kind, findings, outcome))
}

// notifyCronDone / notifyCronFailed report a scheduled run. A scheduled run
// says WHAT finished — the job's name is the part the reader does not already
// know — and a failure is worth a ping for the same reason a one-off's is: the
// report they were waiting for did not arrive.
func (n *notifier) notifyCronDone(sessionID, jobName string) {
	n.notify(sessionID, notifyAppTitle, cronDoneBody(jobName, true))
}

func (n *notifier) notifyCronFailed(sessionID, jobName string) {
	n.notify(sessionID, notifyAppTitle, cronDoneBody(jobName, false))
}

// openSessionEventName is Go→frontend: 「打开那个会话」——一条被点开的通知说的就是这句话。
//
// 与托盘那条 ui:new_session（tray.go）同一个壳，理由也一样：一条从应用外面来的请求只能落在
// 前端，因为 currentId、分组展开、滚进视野和那几份刷新都住在那儿；Go 自己改 activeID 只会让
// 两边说法不一致。
const openSessionEventName = "ui:open_session"

// openSession 请前端切到某个会话（见 n.onNotificationClick）。
func (d *desktopApp) openSession(sessionID string) {
	if d.app == nil {
		return
	}
	d.emit(openSessionEventName, sessionID)
}

