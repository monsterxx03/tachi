package main

// The on-disk desktop_ui.json (layout + reading preferences). It is shared with the headless
// build because filetree.go keeps the file tree's expansion state here — a preference file,
// not a window. The window-only half of uitheme.go (the Cocoa appearance plumbing) stays
// behind the !headless tag.

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/fileutil"
)

// Themes the frontend can choose, mirrored from src/theme.ts.
const (
	themeLight = "light"
	themeDark  = "dark"
)

const uiStateFile = "desktop_ui.json"

// uiState is the on-disk shape of desktop_ui.json. Fields are omitted when
// unset, so the file stays readable and future keys can be added without
// rewriting old files.
type uiState struct {
	// Theme is the user's explicit choice; "" means "follow the system", which
	// is also what an absent file means.
	Theme string `json:"theme,omitempty"`
	// LastWorkspace is the directory the user last picked as a session's primary
	// root. A new session starts there (see defaultWorkspaceFor) instead of at the
	// home directory, which is far too wide to be a workspace.
	LastWorkspace string `json:"lastWorkspace,omitempty"`
	// OneOffPanelOpen remembers the side-channel panel (docs/2026-09-12-desktop-oneoff-panel-design.md):
	// "I keep it open" should survive a restart like every other layout choice. The
	// frontend owns the value and mirrors it here, which is also what makes it
	// inspectable in support ("what is this window actually showing?").
	OneOffPanelOpen bool `json:"oneOffPanelOpen,omitempty"`
	// OneOffPanelWidth is that panel's width in CSS pixels (0 = never dragged, use the
	// default). A preference file is editable by hand, so the setter refuses values outside
	// the panel's own bounds rather than trusting the number (see validOneOffPanelWidth).
	OneOffPanelWidth int `json:"oneOffPanelWidth,omitempty"`
	// SidebarWidth is the session list's width in CSS pixels (0 = never dragged, use the default).
	// Same bargain as the panel's: what is STORED is the width the reader chose, what a layout can
	// SHOW is a separate number the frontend derives from the room there is, and the setter refuses
	// a hand-edited value the layout could not honour.
	SidebarWidth int `json:"sidebarWidth,omitempty"`
	// ReadingFontScale is the reading surfaces' size as a percent of the stylesheet's own (100),
	// the reader's step from ⌘= / ⌘- / ⌘0 (frontend/src/fontsize.ts). Like the theme this is a
	// preference about READING rather than geometry, so the webview owns the live value (and the
	// boot script applies it before the first paint); this side keeps the durable record — what a
	// support question reads, what a hand edit changes, and what answers GetUIState when the
	// webview's own storage is gone. 0 = never touched.
	ReadingFontScale int `json:"readingFontScale,omitempty"`
	// FileTreeOpen is whether the file-tree column was open last time (design §5.3). Same kind of
	// preference as the side panel's: "I keep it open" has to survive a restart.
	FileTreeOpen bool `json:"fileTreeOpen,omitempty"`
	// FileTreeWidth is that column's width in CSS pixels (0 = never dragged), with the same bargain
	// as the other two columns: what is STORED is what the reader chose, what can be SHOWN is
	// derived from the room there is, and the setter refuses a hand-edited value no layout could
	// honour (validFileTreeWidth, filetree.go).
	FileTreeWidth int `json:"fileTreeWidth,omitempty"`
	// FileTreeMemo is the remembered expansion state, keyed by WORKSPACE rather than by session —
	// the tree's identity is the trees, not the conversation, so every session of one project sees
	// the same expansion (filetree.go's fileTreeKey). It carries a write time so the table can drop
	// the oldest workspaces; both bounds are clamped on write AND re-checked on read.
	FileTreeMemo map[string]fileTreeMemo `json:"fileTreeMemo,omitempty"`
}

// The panel's width bounds, mirrored by the frontend (desktop/frontend/src/oneoff.tsx) — it
// clamps what the drag produces, and this side keeps a hand-edited file from storing something
// a layout cannot honour.
const (
	oneOffPanelMinWidth = 320
	oneOffPanelMaxWidth = 1000
)

func validOneOffPanelWidth(px int) bool {
	return px >= oneOffPanelMinWidth && px <= oneOffPanelMaxWidth
}

// The sidebar's width bounds, mirrored by the frontend (desktop/frontend/src/sidebar.tsx). The floor
// is where the rows stop being readable; the ceiling is a sanity bound for the stored value — what
// is SHOWN is clamped to the room the window has, so a wide choice survives a narrow window instead
// of being overwritten by it.
const (
	sidebarMinWidth = 200
	sidebarMaxWidth = 600
)

func validSidebarWidth(px int) bool {
	return px >= sidebarMinWidth && px <= sidebarMaxWidth
}

// SetSidebarWidth records how wide the reader made the session list, refusing a value outside the
// bounds above (a hand-editable file, and a width no layout can honour is worse than the default).
//
// It lives here rather than beside a subsystem because the session list has no Go side of its own:
// this is the persistence half of a frontend layout choice, and uiState is what this file owns.
func (s *AgentService) SetSidebarWidth(px int) {
	if !validSidebarWidth(px) {
		return
	}
	updateUIState(func(st *uiState) bool {
		if st.SidebarWidth == px {
			return false
		}
		st.SidebarWidth = px
		return true
	})
}

// The reading scale's bounds, mirrored by the frontend (desktop/frontend/src/fontsize.ts) — the
// steps it can produce, and the guard against a hand-edited file. At the floor prose stops being
// comfortable; above the ceiling a message is a wall of type.
const (
	readingScaleMinPercent = 80
	readingScaleMaxPercent = 160
)

func validReadingFontScale(pct int) bool {
	return pct >= readingScaleMinPercent && pct <= readingScaleMaxPercent
}

// SetReadingFontScale records the reader's font-size step for the reading surfaces (⌘= / ⌘- / ⌘0).
// It refuses a value outside the bounds above: same bargain as the widths — the file is editable by
// hand, and a size no stylesheet could honour is worse than the default.
func (s *AgentService) SetReadingFontScale(pct int) {
	if !validReadingFontScale(pct) {
		return
	}
	updateUIState(func(st *uiState) bool {
		if st.ReadingFontScale == pct {
			return false
		}
		st.ReadingFontScale = pct
		return true
	})
}

func uiStatePath() string { return filepath.Join(config.BaseDir(), uiStateFile) }

func validTheme(theme string) bool { return theme == themeLight || theme == themeDark }

// uiStateMu serializes every READ-MODIFY-WRITE of desktop_ui.json. It guards the sequence, not
// the file: saveUIState is an atomic write, so the file can never be torn — but Wails runs each
// bound call on its own goroutine, and every field here is written by "read the whole struct,
// change my field, write it back" so the other fields survive. Two concurrent writers each read
// the OLD struct and the later write erases the earlier one's field. Measured: pressing ⌘= twice
// in a row left 110 in the file while the window showed 120 (the font-size scenario reads that
// file). Every mutation therefore goes through updateUIState — not its own load → change → save.
var uiStateMu sync.Mutex

// updateUIState performs one read-modify-write under uiStateMu. mutate returns false when the
// value was already what it wanted to store, which skips the write: desktop_ui.json is written on
// every real change, so a no-op repaint should not cost one.
func updateUIState(mutate func(st *uiState) bool) {
	uiStateMu.Lock()
	defer uiStateMu.Unlock()

	st := loadUIState()
	if !mutate(&st) {
		return
	}
	saveUIState(st)
}

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
		// Drop the bad field on its own rather than the whole struct: the file holds
		// more than one preference, and one stale value must not discard the rest.
		st.Theme = ""
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
