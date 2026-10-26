//go:build headless

package main

// windowHost is deliberately EMPTY in a headless build.
//
// The runtime files are shared with the windowed build, and they ask the app the same two
// questions: "is anyone watching?" (hasUI) and "tell the frontend" (emit). Both answer "no" and
// "nowhere" here, and leaving the struct empty is what makes a stray d.app / d.tray a compile
// error rather than a nil dereference on a machine that has no screen (a NAS, a server, a
// container). See ui_state_ui.go for the windowed half.
type windowHost struct{}

func newWindowHost() windowHost { return windowHost{} }

// hasUI is false: there is no webview in this process. The phone is the only frontend, and it is
// fed through its own projector (mobile events), not through this gate.
func (d *desktopApp) hasUI() bool { return false }

// emit drops the event. Every runtime push goes through here, so one empty body is what keeps the
// headless process from needing a window at all.
func (d *desktopApp) emit(name string, data any) {}

// newNotifierHost returns the notifier a headless process has: none. Every call is a no-op, and
// focused() is false — there is no "foreground application" here, which is the "the user is
// looking elsewhere" side of watching.go. See notify_host.go.
func newNotifierHost() notifierHost { return noNotifier{} }
