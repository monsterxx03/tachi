//go:build darwin && !headless

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode"
)

// The panes of System Settings: 蓝牙, 网络, 声音… — rows the launcher offers beside the apps.
//
// They are NOT applications and cannot be found like one. A pane is a settings EXTENSION
// (`Foo.appex` declaring `com.apple.Settings.extension.ui`), and 蓝牙 lives at
// /System/Library/ExtensionKit/Extensions/Bluetooth.appex while other panes sit under
// System Settings.app/Contents/PlugIns/ — so no single directory is "the one with the panes",
// and `open -a` cannot start one either: the only way in is the URL scheme
// `x-apple.systempreferences:<bundle id>`, which is what the system opens when you deep-link a
// pane.
//
// Everything from the name down is the same as an app's, so a pane becomes a launcherApp whose
// Path is that URL rather than a bundle path: one catalog, one ranking, one usage record.

// paneDirs are where a settings extension's bundle is looked for. Neither is exclusive — a pane
// is in one or the other — so both are read and the result keyed by bundle id.
var paneDirs = []string{
	"/System/Library/ExtensionKit/Extensions",
	"/System/Applications/System Settings.app/Contents/PlugIns",
}

// settingsExtensionPoint is what a System Settings pane declares. Being a .appex in those
// directories is not enough to be a pane: 241 bundles live in the first one and 52 are panes,
// the rest are thumbnails, widgets and intents.
const settingsExtensionPoint = "com.apple.Settings.extension.ui"

// paneURLScheme is how a settings pane is opened — what the system itself follows when a pane is
// linked to, and the only way in short of opening System Settings at its front page.
const paneURLScheme = "x-apple.systempreferences:"

// settingsSidebar is the pane list System Settings draws in its own sidebar, including the panes
// the 「通用」 page hosts. It is the system's own answer to "which panels is a person meant to
// see": the rest of the 52 (ClassKit, Home privacy, Security Improvements…) are reachable only by
// deep link, and a launcher that offers them offers things nobody searches for.
//
// It is READ rather than written down so an OS update brings its own list with it — and so the
// sidebar's order is available too, which is the order System Settings shows the panes in.
const settingsSidebar = "/System/Applications/System Settings.app/Contents/Resources/Sidebar.plist"

// paneIdentity is what a scan learns about one settings extension: the bundle it was read from,
// and the id that opens it.
type paneIdentity struct {
	// Bundle is where the .appex sits on disk. It is the row's identity, the same way an
	// application's bundle path is: the name and the icon are read from it, and a rescan
	// compares its Info.plist mtime.
	Bundle string
	// ID is the bundle id, which is what the pane's own URL is built from.
	ID string
}

// scanPaneIDs is the sidebar's pane list, in the order it draws them. A sidebar that cannot be
// read answers nil, which the caller treats as "no list" rather than "no panes": an OS update
// that moves the file must not empty the feature.
func scanPaneIDs() []string {
	sidebar := plutilJSONValue(settingsSidebar)
	if sidebar == nil {
		return nil
	}
	var ids []string
	seen := make(map[string]bool)
	var walk func(node any)
	walk = func(node any) {
		switch typed := node.(type) {
		case []any:
			for _, item := range typed {
				walk(item)
			}
		case map[string]any:
			// A sidebar section lists its panes under "content"; the last one is a catch-all whose
			// panes were never written down, so it contributes nothing.
			if content, ok := typed["content"].([]any); ok {
				for _, item := range content {
					if id, ok := item.(string); ok && !seen[id] {
						seen[id] = true
						ids = append(ids, id)
					}
				}
			}
		}
	}
	walk(sidebar)
	return ids
}

// scanPanes reads every settings pane's bundle, keeps the ones the sidebar names, and returns
// them in sidebar order.
//
// The sidebar decides membership, so a pane it does not name is dropped. A pane it names but whose
// bundle cannot be found is dropped too (a several OS versions old sidebar names ids that no
// longer ship) — an entry with no name and no icon is a row nobody can use.
func scanPanes() []paneIdentity {
	wanted := scanPaneIDs()
	found := make(map[string]paneIdentity)
	for _, dir := range paneDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".appex") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			identity, ok := readPane(path)
			if !ok {
				continue
			}
			if _, taken := found[identity.ID]; taken {
				continue
			}
			found[identity.ID] = identity
		}
	}

	if wanted == nil {
		// No sidebar to go by: every pane, in an order a person can scan.
		panes := make([]paneIdentity, 0, len(found))
		for _, identity := range found {
			panes = append(panes, identity)
		}
		sort.Slice(panes, func(i, j int) bool { return panes[i].Bundle < panes[j].Bundle })
		return panes
	}
	panes := make([]paneIdentity, 0, len(wanted))
	for _, id := range wanted {
		if identity, ok := found[id]; ok {
			panes = append(panes, identity)
		}
	}
	return panes
}

// readPane answers whether path is a System Settings pane, and what opens it.
//
// The declared extension point is the test, not the directory: those directories hold plenty of
// other extensions, and the sidebar names ids whose bundle lives somewhere else again.
func readPane(path string) (paneIdentity, bool) {
	info := plutilJSON(filepath.Join(path, "Contents", "Info.plist"))
	if info == nil {
		return paneIdentity{}, false
	}
	attributes, _ := info["EXAppExtensionAttributes"].(map[string]any)
	if stringField(attributes, "EXExtensionPointIdentifier") != settingsExtensionPoint {
		return paneIdentity{}, false
	}
	bundleID := stringField(info, "CFBundleIdentifier")
	if bundleID == "" {
		return paneIdentity{}, false
	}
	return paneIdentity{Bundle: path, ID: bundleID}, true
}

// scanPaneCatalog reads the panes' names and turns them into catalog entries, in parallel the
// same way readApps reads applications. A pane whose name could not be resolved is left out.
func scanPaneCatalog() []*launcherApp {
	panes := scanPanes()
	apps := make([]*launcherApp, len(panes))
	slots := make(chan struct{}, plistWorkers)
	var wg sync.WaitGroup
	for i, pane := range panes {
		wg.Add(1)
		slots <- struct{}{}
		go func(i int, pane paneIdentity) {
			defer wg.Done()
			defer func() { <-slots }()
			apps[i] = newPaneLauncherApp(pane)
		}(i, pane)
	}
	wg.Wait()

	named := apps[:0]
	for _, app := range apps {
		if app != nil {
			named = append(named, app)
		}
	}
	return named
}

// newPaneLauncherApp reads a pane's name off its bundle and marks the row as a pane.
//
// A pane's name is not simply its bundle's: System Settings draws its sidebar from the pane's own
// `representations`, each naming a localizable key (`BATTERY_PREF_TITLE`), and 电池 vs 能耗 is
// decided by a predicate on the machine (a laptop shows 电池, a desktop 能耗). The bundle's own
// InfoPlist.loctable is the fallback — that is where 蓝牙 lives — and readAppName reads it.
func newPaneLauncherApp(pane paneIdentity) *launcherApp {
	name, aliases, _, stamp := readAppName(pane.Bundle)
	app := &launcherApp{
		Path:     pane.Bundle,
		Name:     name,
		Aliases:  aliases,
		BundleID: pane.ID,
		Stamp:    stamp,
		Pane:     true,
	}
	if sidebarName := sidebarNameOf(pane.Bundle); sidebarName != "" {
		// The sidebar's own label, when there is one, is what the system calls this page — and the
		// English it replaced is still worth keeping reachable.
		if name != sidebarName {
			app.Aliases = append(app.Aliases, name)
		}
		app.Name = sidebarName
	}
	// System Settings shows only the panes that can be translated or that the system itself leaves
	// in English. Everything else it names with an internal identifier — "FollowUps",
	// "HeadphoneSettingsExtension" — and those pages it does not list until the device or condition
	// they are about shows up. A row nobody could have typed is worse than a missing one.
	if !paneNameIsReadable(app.Name) {
		return nil
	}
	app.prepare()
	return app
}

// paneNamesLeftInEnglish are the pages System Settings itself does not translate. They are listed
// rather than inferred from the name: a rule like "no CJK means untranslated" would drop exactly
// these four, and a rule like "short words are names" would keep the identifiers.
var paneNamesLeftInEnglish = map[string]bool{
	"Wi‑Fi":       true,
	"VPN":         true,
	"Siri":        true,
	"Game Center": true,
}

// paneNameIsReadable reports whether a pane's name is something a person would search for: a
// Chinese name, or one of the few the system leaves in English.
func paneNameIsReadable(name string) bool {
	if paneNamesLeftInEnglish[name] {
		return true
	}
	return strings.ContainsFunc(name, func(r rune) bool {
		return unicode.Is(unicode.Han, r)
	})
}

// sidebarNameOf resolves the label System Settings shows for a pane — the first `representations`
// entry whose predicate holds, looked up in the bundle's Localizable table.
//
// Only one predicate is understood, `powersource.battery`, because it is the only one that names a
// page differently on different machines (电池 on a laptop, 能耗 on a desktop). An unrecognised
// predicate is treated as unproven rather than false: the first entry that names anything is taken,
// which is the answer for every pane whose condition this code cannot ask about.
func sidebarNameOf(bundle string) string {
	info := plutilJSON(filepath.Join(bundle, "Contents", "Info.plist"))
	attributes, _ := info["EXAppExtensionAttributes"].(map[string]any)
	settings, _ := attributes["SettingsExtensionAttributes"].(map[string]any)
	representations, _ := settings["representations"].([]any)

	translated := localizedTable(filepath.Join(bundle, "Contents", "Resources", "Localizable.loctable"))
	fallback := ""
	for _, entry := range representations {
		representation, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		key := stringField(representation, "sidebar-name")
		if key == "" {
			continue
		}
		label := translated[key]
		if label == "" {
			continue
		}
		if fallback == "" {
			fallback = label
		}
		if matches, known := evaluateSidebarPredicate(stringField(representation, "predicate")); known && matches {
			return label
		}
	}
	return fallback
}

// localizedTable reads one language out of a .loctable, which holds every language at once.
func localizedTable(path string) map[string]string {
	table := plutilJSON(path)
	names := make(map[string]string)
	for _, language := range loctableLanguages {
		entry, ok := table[language].(map[string]any)
		if !ok {
			continue
		}
		for key, value := range entry {
			if text, ok := value.(string); ok {
				names[key] = text
			}
		}
	}
	return names
}

// evaluateSidebarPredicate answers a representation's condition: whether it holds, and whether this
// code can tell at all.
func evaluateSidebarPredicate(predicate string) (matches, known bool) {
	for _, wanted := range []struct {
		text  string
		holds bool
	}{
		{"powersource.battery == YES", batteryPresent()},
		{"powersource.battery != YES", !batteryPresent()},
	} {
		if predicate == wanted.text {
			return wanted.holds, true
		}
	}
	return false, false
}

// batteryPresent reports whether this machine runs on a battery, asked once. A desktop has no
// battery page at all — System Settings shows it 能耗 instead.
var batteryPresent = sync.OnceValue(func() bool {
	out, err := exec.Command("pmset", "-g", "batt").Output()
	return err == nil && strings.Contains(string(out), "InternalBattery")
})

// paneURL is what opens this row when it is a settings pane, and "" for an application. It is
// built from the bundle id rather than stored: the URL is one constant plus an id the catalog
// already keeps.
func (a *launcherApp) paneURL() string {
	if !a.Pane || a.BundleID == "" {
		return ""
	}
	return paneURLScheme + a.BundleID
}

// openPane opens System Settings at the page the URL names — the same thing happens when the
// system follows a link to a pane.
func openPane(url string) error {
	return exec.Command("open", url).Run()
}

// panesOf is the pane half of the scan: where the settings panes are and how they are read. The
// application half lives in launcher_apps_darwin.go, and both answer the same catalog shape so
// everything above — ranking, icons, usage, the cache — never learns there are two kinds of row.
func panesOf() []*launcherApp { return scanPaneCatalog() }
