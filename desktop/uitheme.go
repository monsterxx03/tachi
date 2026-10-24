//go:build !headless

package main

// Theme plumbing for the desktop window.
//
// The webview owns the theme the user sees: an inline script in index.html
// resolves it (stored choice, else the system preference) and src/theme.ts
// drives it from the titlebar switch. But two things outside the webview can
// only be decided here:
//
//   - the window's pre-paint colour, which has to be right BEFORE any frontend
//     code runs (otherwise launching into a theme that differs from the system
//     flashes the wrong shade for a few hundred milliseconds);
//   - the native window appearance, which is fixed when the window is created.
//
// So the frontend mirrors every applied theme here over the "ui:theme" event
// (see mirrorTheme in frontend/src/theme.ts) and this file remembers it in
// desktop_ui.json — the launch after a toggle therefore resolves the theme from
// disk, not from localStorage.

import (
	"log"
	"sync"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// uiThemeEventName is the custom event the frontend emits on mount and on every
// theme change; its payload is the user's theme CHOICE ("" = follow the system),
// mirroring src/theme.ts — both sides must stay identical. Mirroring the choice
// rather than the theme on screen is what keeps an untouched switch following
// the OS: a resolved value would freeze the window at the first launch's system
// appearance.
const uiThemeEventName = "ui:theme"

// resolveTheme maps the stored choice and the dev override onto a Cocoa appearance plus the
// "is it dark" answer the window background needs.
func resolveTheme(stored string, forced application.MacAppearanceType, systemDark bool) (application.MacAppearanceType, bool) {
	switch {
	case forced == application.NSAppearanceNameDarkAqua:
		return forced, true
	case forced == application.NSAppearanceNameAqua:
		return forced, false
	case stored == themeDark:
		return application.NSAppearanceNameDarkAqua, true
	case stored == themeLight:
		return application.NSAppearanceNameAqua, false
	default:
		return application.DefaultAppearance, systemDark
	}
}

// themeController keeps the window's colour in step with the theme while the
// app runs. The native appearance itself is only fixed at window creation
// (Wails v3 beta has no runtime setter), so a toggle that disagrees with the
// system takes full effect on the next launch — the colour, which is what is
// visible around and behind the webview, changes immediately.
type themeController struct {
	window *application.WebviewWindow
	// forced is TACHI_DESKTOP_APPEARANCE when the developer set it; anything
	// other than DefaultAppearance outranks both the user and the system.
	forced application.MacAppearanceType

	mu sync.Mutex
	// manual is the choice mirrored from the frontend ("" = follow the system).
	manual string
}

func newThemeController(window *application.WebviewWindow, stored string, forced application.MacAppearanceType) *themeController {
	return &themeController{window: window, forced: forced, manual: stored}
}

// apply paints the window background for the theme the given choice resolves to.
func (c *themeController) apply(stored string, systemDark bool) {
	_, dark := resolveTheme(stored, c.forced, systemDark)
	colour := windowBgLight
	if dark {
		colour = windowBgDark
	}
	c.window.SetBackgroundColour(colour)
}

// manualChoice returns the frontend's current choice ("" = follow the system).
func (c *themeController) manualChoice() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.manual
}

// setFromFrontend records the theme choice the webview just reported ("" = the
// user is following the system). A real choice is persisted so the next launch
// paints the right colour before the frontend runs, and repaints the window
// frame now; an empty one only clears the record — the window is already
// showing whatever the system said, and onSystemChange keeps it in step.
func (c *themeController) setFromFrontend(choice string) {
	if choice != "" && !validTheme(choice) {
		log.Printf("desktop ui: ignoring unknown theme %q from frontend", choice)
		return
	}
	c.mu.Lock()
	c.manual = choice
	c.mu.Unlock()
	persistTheme(choice)
	if choice == "" {
		return
	}
	c.apply(choice, false) // the choice outranks systemDark
}

// persistTheme records the theme choice in desktop_ui.json without touching the
// window — split out from setFromFrontend so the FILE contract is testable on its
// own (the controller's other half reaches into the native window, which blocks
// forever in a test binary with no app event loop).
//
// Read-modify-write, not a fresh struct: desktop_ui.json holds more than the theme
// (the remembered workspace lives there too), so each writer has to preserve the
// other's fields.
func persistTheme(choice string) {
	updateUIState(func(st *uiState) bool {
		if st.Theme == choice {
			return false
		}
		st.Theme = choice
		return true
	})
}

// onSystemChange repaints the frame when the OS appearance changes, unless a
// manual choice (or the dev override) owns the theme.
func (c *themeController) onSystemChange(systemDark bool) {
	if c.forced != application.DefaultAppearance {
		return
	}
	if manual := c.manualChoice(); manual != "" {
		return
	}
	c.apply("", systemDark)
}
