package main

// watching reports whether the user is looking at this conversation right now: the window
// has focus AND it is showing that session.
//
// It is ONE rule with two readers, and they must not drift apart — the OS notification
// (notify, below) and the device's beep (asks.go's alertIfUnwatched) are the two ways this
// app interrupts somebody, and both exist to reach a user who is looking elsewhere. Written
// as a pure function so the combinations can be pinned on the host: trying them for real
// costs a notification, a session switch, and a walk to the device.
//
// An empty active id means no conversation is open (a fresh window, before anything is
// loaded). That is "elsewhere": there is no transcript on screen the news would repeat.
//
// It lives here, apart from notify.go, because askusers on the headless host still ask this
// question (through notifierHost.focused) even though they never post a notification.
func watching(focused bool, activeSessionID, eventSessionID string) bool {
	return focused && eventSessionID == activeSessionID
}
