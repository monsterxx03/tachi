package main

import (
	"testing"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// TestApplicationMenuFreesTheZoomKeys pins the mechanism behind ⌘= / ⌘- / ⌘0 belonging to
// the message area: a menu item's key equivalent is handled BEFORE the webview ever sees
// the event, so the three WebKit zoom items have to be gone — and the rest of the default
// menu has to survive, since dropping them is the only change applicationMenu makes.
//
// No frontend test can cover this: a driver dispatches a synthetic key straight into the
// page, which bypasses the menu bar entirely. This is the only place the claim is checkable.
func TestApplicationMenuFreesTheZoomKeys(t *testing.T) {
	// The default menu is assembled against the running application (its App menu is
	// labelled with the app's name), so the test makes one. New only — no Run, no window:
	// building the menu is all that is being asserted.
	application.New(application.Options{Name: "Tachi"})

	// The three keys are referenced through Wails' OWN item constructors, never as literal
	// strings: what is asserted is "these keys are free", not a spelling of them.
	zoom := []*application.MenuItem{
		application.NewZoomInMenuItem(),
		application.NewZoomOutMenuItem(),
		application.NewZoomResetMenuItem(),
	}
	claimed := map[string]string{}
	for _, item := range zoom {
		claimed[item.GetAccelerator()] = item.Label()
	}

	menu := applicationMenu()
	seen := 0
	walkMenuItems(menu, func(item *application.MenuItem) {
		if acc := item.GetAccelerator(); acc != "" {
			seen++
			if label, isZoom := claimed[acc]; isZoom {
				t.Errorf("menu item %q still claims %s — that key would magnify the whole window instead of reaching the page",
					label, acc)
			}
		}
	})
	if seen < 5 {
		// An empty or half-built menu would pass the loop above by saying nothing.
		t.Fatalf("walked only %d accelerators — the menu walk itself is broken", seen)
	}

	for _, item := range zoom {
		if menu.FindByLabel(item.Label()) != nil {
			t.Errorf("menu item %q is still in the menu", item.Label())
		}
	}
	// What the default menu must still carry: the app, file, edit, view and window menus,
	// with the developer affordances (reload / devtools / full screen) intact.
	for _, want := range []string{"Edit", "View", "Reload", "Force Reload", "Open Developer Tools", "Toggle Full Screen", "Copy", "Paste", "Undo", "Minimize"} {
		if menu.FindByLabel(want) == nil {
			t.Errorf("menu item %q is missing from the app menu", want)
		}
	}
}

// walkMenuItems visits every item in the menu tree, submenus included. Menu exposes no
// iterator, only ItemAt/GetSubmenu, which is why this exists at all.
func walkMenuItems(menu *application.Menu, visit func(*application.MenuItem)) {
	if menu == nil {
		return
	}
	for i := 0; ; i++ {
		item := menu.ItemAt(i)
		if item == nil {
			return
		}
		visit(item)
		walkMenuItems(item.GetSubmenu(), visit)
	}
}
