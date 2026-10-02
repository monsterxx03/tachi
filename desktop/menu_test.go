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

// TestApplicationMenuFreesTheFrontendKeys pins "every key the page binds is free in the menu bar": a
// key equivalent is handled BEFORE the webview sees the event, so a menu item carrying one of them
// makes that shortcut dead — and no frontend test can notice, because a synthetic key dispatched into
// the page bypasses the menu bar entirely. Wails' default does claim Cmd+M (Minimize) and Cmd+H
// (Hide), which is why the app's own bindings step around those two: the voice toggle is ⌘⇧M and the
// rewind chain is ⌘⇧H.
//
// The accelerators go through Wails' own parser (SetAccelerator → GetAccelerator) rather than being
// written as literals: what is asserted is "this key is free", not a spelling of it. A misspelling
// parses to the empty string, which the guard below turns into a failure instead of a test that
// quietly compares "" against nothing.
//
// The composer's own ⌘⇧↑/↓ and ⌘↩ are deliberately absent: they only mean anything inside the input
// box, and an arrow or a Return is not something the menu bar binds.
func TestApplicationMenuFreesTheFrontendKeys(t *testing.T) {
	application.New(application.Options{Name: "Tachi"})

	// One entry per binding in frontend/src/App.tsx's keydown handler, spelled in Wails' own syntax.
	bindings := []struct{ accel, what string }{
		{"Cmd+N", "⌘N 新建会话（跟眼前这个一样）"},
		{"Cmd+Shift+N", "⌘⇧N 新建会话（不属于任何项目）"},
		{"Cmd+Shift+Backspace", "⌘⇧⌫ 删除当前会话"},
		{"Cmd+P", "⌘P 项目搜索"},
		{"Cmd+K", "⌘K 会话搜索"},
		{"Cmd+B", "⌘B 折叠侧栏"},
		{"Cmd+/", "⌘/ 聚焦输入框"},
		{"Cmd+,", "⌘, 打开设置"},
		{"Cmd+Shift+H", "⌘⇧H 回退链"},
		{"Cmd+Shift+M", "⌘⇧M 语音输入"},
	}
	keys := map[string]string{}
	for _, b := range bindings {
		acc := application.NewMenuItem("probe").SetAccelerator(b.accel).GetAccelerator()
		if acc == "" {
			t.Fatalf("Wails 认不出 %s：拼写变了，这条测试就再也证明不了什么（frontend/src/App.tsx 的 %s）",
				b.accel, b.what)
		}
		keys[acc] = b.what
	}

	menu := applicationMenu()
	walkMenuItems(menu, func(item *application.MenuItem) {
		if what, bound := keys[item.GetAccelerator()]; bound {
			t.Errorf("菜单项 %q 占用了 %s —— %s 会变成死键（frontend/src/App.tsx）",
				item.Label(), item.GetAccelerator(), what)
		}
	})
}
