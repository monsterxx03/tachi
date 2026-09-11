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
	"errors"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// Themes the frontend can choose, mirrored from src/theme.ts.
const (
	themeLight = "light"
	themeDark  = "dark"
)

// uiThemeEventName is the custom event the frontend emits on mount and on every
// theme change; its payload is the user's theme CHOICE ("" = follow the system),
// mirroring src/theme.ts — both sides must stay identical. Mirroring the choice
// rather than the theme on screen is what keeps an untouched switch following
// the OS: a resolved value would freeze the window at the first launch's system
// appearance.
const uiThemeEventName = "ui:theme"

// uiStateFile holds desktop-only UI preferences (currently just the theme) next
// to the rest of tachi's state. A missing file, an empty object and an empty
// theme all mean the same thing: the user never chose a theme.
const uiStateFile = "desktop_ui.json"

// uiState is the on-disk shape of desktop_ui.json. Fields are omitted when
// unset, so the file stays readable and future keys can be added without
// rewriting old files.
type uiState struct {
	// Theme is the user's explicit choice; "" means "follow the system", which
	// is also what an absent file means.
	Theme string `json:"theme,omitempty"`
}

func uiStatePath() string { return filepath.Join(config.BaseDir(), uiStateFile) }

func validTheme(theme string) bool { return theme == themeLight || theme == themeDark }

// loadUIState reads the persisted UI state. A missing, unreadable or malformed
// file is not an error — every one of those cases degrades to "no manual
// choice", which is exactly the state a user who never opened the switch is in.
func loadUIState() uiState {
	var st uiState
	if err := fileutil.ReadJSON(uiStatePath(), &st); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("desktop ui: ignoring %s: %v", uiStatePath(), err)
		}
		return uiState{}
	}
	if !validTheme(st.Theme) {
		return uiState{}
	}
	return st
}

// saveUIState persists st for the next launch (best effort: a failure only
// costs the native chrome its head start, the webview still renders correctly).
func saveUIState(st uiState) {
	if err := fileutil.AtomicWriteJSONShared(uiStatePath(), st); err != nil {
		log.Printf("desktop ui: cannot write %s: %v", uiStatePath(), err)
	}
}

// resolveTheme is the single rule for "which theme should the window show?".
// Priority: TACHI_DESKTOP_APPEARANCE (dev aid, see forcedAppearance) > the
// manual choice mirrored by the frontend > the system appearance. It returns
// the Cocoa appearance to hand the window (used at creation, when the native
// look is first fixed) and whether that theme is dark.
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
	saveUIState(uiState{Theme: choice})
	if choice == "" {
		return
	}
	c.apply(choice, false) // the choice outranks systemDark
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
