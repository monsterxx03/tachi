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

// TestApplicationMenuFreesTheVoiceKey pins the same claim for ⌘⇧M (the microphone shortcut): a key
// equivalent is handled BEFORE the webview sees the event, so a menu item carrying this one would
// make the shortcut dead — and no frontend test can notice, because a synthetic key dispatched into
// the page bypasses the menu bar entirely.
//
// The accelerator goes through Wails' own parser (SetAccelerator → GetAccelerator) rather than being
// written as a literal: what is asserted is "this key is free", not a spelling of it. A misspelling
// parses to the empty string, which the guard below turns into a failure instead of a test that
// quietly compares "" against nothing.
func TestApplicationMenuFreesTheVoiceKey(t *testing.T) {
	application.New(application.Options{Name: "Tachi"})

	probe := application.NewMenuItem("probe").SetAccelerator("Cmd+Shift+M")
	voiceKey := probe.GetAccelerator()
	if voiceKey == "" {
		t.Fatal("Wails 认不出 Cmd+Shift+M：拼写变了，这条测试就再也证明不了什么（见 frontend/src/App.tsx 的 ⌘⇧M）")
	}

	menu := applicationMenu()
	walkMenuItems(menu, func(item *application.MenuItem) {
		if acc := item.GetAccelerator(); acc == voiceKey {
			t.Errorf("菜单项 %q 占用了 %s —— 语音输入的快捷键会变成死键（frontend/src/App.tsx 的 ⌘⇧M）",
				item.Label(), voiceKey)
		}
	})
}
