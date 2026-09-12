package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/monsterxx03/tachi/config"
	"github.com/wailsapp/wails/v3/pkg/application"
)

func TestResolveTheme(t *testing.T) {
	tests := []struct {
		name         string
		stored       string
		forced       application.MacAppearanceType
		systemDark   bool
		wantAppear   application.MacAppearanceType
		wantDarkness bool
	}{
		{
			name:         "no choice follows the system",
			systemDark:   true,
			wantAppear:   application.DefaultAppearance,
			wantDarkness: true,
		},
		{
			name:         "stored light wins on a dark system",
			stored:       themeLight,
			systemDark:   true,
			wantAppear:   application.NSAppearanceNameAqua,
			wantDarkness: false,
		},
		{
			name:         "stored dark wins on a light system",
			stored:       themeDark,
			systemDark:   false,
			wantAppear:   application.NSAppearanceNameDarkAqua,
			wantDarkness: true,
		},
		{
			name:         "dev override outranks a stored choice",
			stored:       themeLight,
			forced:       application.NSAppearanceNameDarkAqua,
			systemDark:   false,
			wantAppear:   application.NSAppearanceNameDarkAqua,
			wantDarkness: true,
		},
		{
			name:         "unknown stored value degrades to the system",
			stored:       "solarized",
			systemDark:   false,
			wantAppear:   application.DefaultAppearance,
			wantDarkness: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			appearance, dark := resolveTheme(tt.stored, tt.forced, tt.systemDark)
			if appearance != tt.wantAppear || dark != tt.wantDarkness {
				t.Errorf("resolveTheme(%q, %q, %v) = (%v, %v), want (%v, %v)",
					tt.stored, tt.forced, tt.systemDark, appearance, dark, tt.wantAppear, tt.wantDarkness)
			}
		})
	}
}

func TestUIStateRoundTrip(t *testing.T) {
	// config.SetBaseDir is a process global — point it at a temp dir, never the
	// real ~/.tachi (same rule as fileservice_test.go).
	config.SetBaseDir(t.TempDir())

	if got := loadUIState(); got.Theme != "" {
		t.Fatalf("loadUIState() on a fresh dir = %+v, want empty", got)
	}

	saveUIState(uiState{Theme: themeDark})
	if got := loadUIState(); got.Theme != themeDark {
		t.Fatalf("loadUIState() after save = %q, want %q", got.Theme, themeDark)
	}

	// Clearing the choice (the frontend mirroring "follow the system") must not
	// leave the old theme behind for the next launch to pick up.
	saveUIState(uiState{})
	if got := loadUIState(); got.Theme != "" {
		t.Fatalf("loadUIState() after clearing = %q, want empty", got.Theme)
	}
}

func TestLoadUIStateIgnoresGarbage(t *testing.T) {
	config.SetBaseDir(t.TempDir())

	// A hand-edited file must not break startup: unknown theme values and
	// unparseable JSON both mean "no manual choice".
	cases := map[string]string{
		"unknown theme": `{"theme":"solarized"}`,
		"unparseable":   `{`,
		"wrong shape":   `{"theme":42}`,
		"empty file":    ``,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(uiStatePath(), []byte(content), 0o644); err != nil {
				t.Fatalf("write %s: %v", filepath.Base(uiStatePath()), err)
			}
			if got := loadUIState(); got.Theme != "" {
				t.Errorf("loadUIState() = %q, want empty for %s", got.Theme, content)
			}
		})
	}
}

// TestOneOffPanelWidthBounds: the panel's width is a preference a user can also hand-edit, so
// the setter refuses a number the layout cannot honour instead of storing it — the default is a
// better failure than a 3px-wide panel.
func TestOneOffPanelWidthBounds(t *testing.T) {
	config.SetBaseDir(t.TempDir())
	svc := &AgentService{}

	svc.SetOneOffPanelWidth(480)
	if got := svc.GetUIState().OneOffPanelWidth; got != 480 {
		t.Errorf("width = %d, want 480", got)
	}
	for _, bad := range []int{0, 10, oneOffPanelMinWidth - 1, oneOffPanelMaxWidth + 1, 100000, -400} {
		svc.SetOneOffPanelWidth(bad)
		if got := svc.GetUIState().OneOffPanelWidth; got != 480 {
			t.Errorf("width after %d = %d, want the previous 480 (out of bounds is not stored)", bad, got)
		}
	}
	// Both bounds are inclusive: they are what the drag clamps to.
	for _, ok := range []int{oneOffPanelMinWidth, oneOffPanelMaxWidth} {
		svc.SetOneOffPanelWidth(ok)
		if got := svc.GetUIState().OneOffPanelWidth; got != ok {
			t.Errorf("width = %d, want %d", got, ok)
		}
	}
	// The open flag and the width share the file without clobbering each other.
	svc.SetOneOffPanelOpen(true)
	if st := svc.GetUIState(); !st.OneOffPanelOpen || st.OneOffPanelWidth != oneOffPanelMaxWidth {
		t.Errorf("state = %+v, want both fields kept", st)
	}
}
