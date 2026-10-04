//go:build !darwin

package main

import (
	"testing"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// TestNoApplicationMenuOffDarwin pins the Linux half of the menu pair: the app must leave
// the application menu UNSET.
//
// Why it is worth a test: the failure mode is a visible strip of GTK chrome (File / Edit /
// View / Window / Help) that no frontend test can see, and it comes back the moment someone
// "restores" the SetApplicationMenu call — or calls it with nil, which makes Wails install
// its own default (see menu_other.go).
func TestNoApplicationMenuOffDarwin(t *testing.T) {
	// New only — no Run, no window: what is asserted is the app's menu slot, nothing else.
	app := application.New(application.Options{Name: "Tachi"})

	installApplicationMenu(app)

	if menu := app.Menu.GetApplicationMenu(); menu != nil {
		t.Errorf("Linux 上不应安装应用菜单，实际拿到了 %q", menu.ItemAt(0).Label())
	}
}
