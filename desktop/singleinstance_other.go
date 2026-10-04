//go:build !darwin

package main

import "github.com/wailsapp/wails/v3/pkg/application"

// singleInstanceID names the app on the session bus (Wails builds the final D-Bus name from it:
// org.wails_app_<id>.SingleInstance). It matches the app id the window claims as WM_CLASS and
// the .desktop file's StartupWMClass, so Tachi has one identity everywhere.
const singleInstanceID = "org.wails.tachi"

// singleInstanceOptions is what keeps a second launch from becoming a second Tachi.
//
// On Linux nothing else does. Closing the window only HIDES it — the tray is the way back, and
// the process stays in it (see the close hook in main.go) — so launching the app again from the
// launcher used to start a second process: a second tray icon, and a second writer of every
// state file (~/.tachi's sessions, desktop_ui.json, desktop_status.json). Wails gives the app a
// name on the session bus instead: the first instance exports it, a second finds it taken, hands
// over its argv and exits with ExitCode. The callback is where such a launch lands, and the only
// thing worth doing with it is showing the window — which is what "open Tachi" meant.
//
// Two exemptions, both load-bearing:
//
//   - macOS returns nothing at all (singleinstance_darwin.go): LaunchServices already activates
//     the running app instead of starting a second one, and `open -n` — a deliberate second
//     instance, which the dev flow and the smoke suite rely on — has to keep working.
//   - A SCRIPTED run gets nothing either. The smoke suite starts its own app on the same session
//     bus as the user's Tachi, so a shared name would swallow the suite's launch into the user's
//     app (revealing that window mid-run) instead of starting the isolated instance the run is
//     about.
func singleInstanceOptions(reveal func()) *application.SingleInstanceOptions {
	if demoEnabled() {
		return nil
	}
	return &application.SingleInstanceOptions{
		UniqueID: singleInstanceID,
		// The argv of the second launch is deliberately dropped: the app has no
		// file/URL arguments, so there is nothing to hand over beyond the fact that
		// someone asked for the window.
		OnSecondInstanceLaunch: func(application.SecondInstanceData) { reveal() },
	}
}
