package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/monsterxx03/tachi/config"
)

// writeBundle builds a throwaway .app with the files it is given, keyed by their path inside
// Contents. The plists are XML, which plutil reads as happily as the binary ones macOS ships.
func writeBundle(t *testing.T, info string, files map[string]string) string {
	t.Helper()
	app := filepath.Join(t.TempDir(), "Probe.app")
	contents := filepath.Join(app, "Contents")
	if err := os.MkdirAll(contents, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel, body string) {
		path := filepath.Join(contents, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Info.plist", info)
	for rel, body := range files {
		write(rel, body)
	}
	return app
}

const probeInfoPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
	<key>CFBundleDisplayName</key><string>Probe</string>
	<key>CFBundleIdentifier</key><string>com.example.probe</string>
</dict></plist>`

// writeBundleWithPlist builds a throwaway bundle with ONE Info.plist and nothing else — the shape a
// settings extension is read in.
func writeBundleWithPlist(t *testing.T, info, name string) string {
	t.Helper()
	return writeBundleWithPlists(t, map[string]string{"Info.plist": info}, name)
}

// writeBundleWithPlists builds a throwaway bundle out of the given files, keyed by their path
// inside Contents. The plists are XML, which plutil reads as happily as the binary ones macOS ships.
func writeBundleWithPlists(t *testing.T, files map[string]string, name string) string {
	t.Helper()
	app := filepath.Join(t.TempDir(), name)
	contents := filepath.Join(app, "Contents")
	for rel, body := range files {
		path := filepath.Join(contents, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return app
}

func stringsFile(t *testing.T, name string) string {
	t.Helper()
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
	<key>CFBundleDisplayName</key><string>` + name + `</string>
</dict></plist>`
}

// The two shapes a Chinese name comes in: a directory per language (a normal application, 微信 in
// zh-Hans.lproj) and one table holding every language at once (the system's own apps — 访达 exists
// nowhere else). A launcher that reads only one of them shows half its list in English.
func TestLocalizedNameReadsBothShapes(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{
			name:  "lproj",
			files: map[string]string{"Resources/zh-Hans.lproj/InfoPlist.strings": stringsFile(t, "探针")},
			want:  "探针",
		},
		{
			name: "loctable",
			files: map[string]string{"Resources/InfoPlist.loctable": `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
	<key>en</key><dict><key>CFBundleName</key><string>Probe</string></dict>
	<key>zh_CN</key><dict><key>CFBundleName</key><string>探针</string></dict>
</dict></plist>`},
			want: "探针",
		},
		{
			name:  "neither",
			files: nil,
			want:  "",
		},
	}
	for _, c := range cases {
		app := writeBundle(t, probeInfoPlist, c.files)
		if got := localizedName(filepath.Join(app, "Contents")); got != c.want {
			t.Errorf("%s: localizedName = %q, want %q", c.name, got, c.want)
		}
	}
}

// The English name stays reachable: the row says 探针, but typing "probe" has to find it too.
func TestReadAppNameKeepsTheEnglishNameAsAlias(t *testing.T) {
	app := writeBundle(t, probeInfoPlist, map[string]string{
		"Resources/InfoPlist.loctable": `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
	<key>zh_CN</key><dict><key>CFBundleDisplayName</key><string>探针</string></dict>
</dict></plist>`,
	})
	name, aliases, bundleID, _ := readAppName(app)
	if name != "探针" {
		t.Errorf("name = %q, want 探针", name)
	}
	if bundleID != "com.example.probe" {
		t.Errorf("bundleID = %q", bundleID)
	}
	found := false
	for _, alias := range aliases {
		if alias == "Probe" || alias == "probe" {
			found = true
		}
	}
	if !found {
		t.Errorf("aliases = %v, want the English name among them", aliases)
	}
}

// A cache is reused by each entry's Info.plist mtime, so nothing on disk moves when the way a
// bundle is READ changes — the old cache would keep answering with the old reading forever. A
// version it was not written by is refused, and the rescan on the next press refills it.
func TestCacheFromAnotherVersionIsIgnored(t *testing.T) {
	config.SetBaseDir(t.TempDir())
	t.Cleanup(func() { config.SetBaseDir("") })
	path := launcherCachePath()

	write := func(body string) {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"apps":[{"path":"/Applications/Old.app","name":"Old"}]}`)
	stale := newLauncher()
	stale.loadCatalog()
	if len(stale.apps) != 0 {
		t.Fatalf("a cache from another version was used: %v", stale.apps)
	}

	write(`{"version":` + strconv.Itoa(launcherCacheVersion) + `,"apps":[{"path":"/Applications/New.app","name":"New"}]}`)
	fresh := newLauncher()
	fresh.loadCatalog()
	if len(fresh.apps) != 1 || fresh.apps[0].Name != "New" {
		t.Fatalf("a cache of this version was dropped: %v", fresh.apps)
	}
}

// A settings pane is not an application: it is a settings EXTENSION, it is opened by URL rather
// than by bundle, and its Chinese name comes from a third place again (the sidebar's own key in
// Localizable.loctable). These pin the three facts that differ.
func TestReadPaneRecognisesOnlySettingsExtensions(t *testing.T) {
	pane := writeBundleWithPlist(t, `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
	<key>CFBundleIdentifier</key><string>com.example.settings</string>
	<key>EXAppExtensionAttributes</key><dict>
		<key>EXExtensionPointIdentifier</key><string>com.apple.Settings.extension.ui</string>
	</dict>
</dict></plist>`, "Pane.appex")

	identity, ok := readPane(pane)
	if !ok {
		t.Fatal("a settings extension was not recognised")
	}
	if identity.ID != "com.example.settings" {
		t.Errorf("id = %q", identity.ID)
	}

	// The directories these bundles live in are full of other extensions; the declared extension
	// point is what makes one a pane.
	notPane := writeBundleWithPlist(t, `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
	<key>CFBundleIdentifier</key><string>com.example.thumbnail</string>
	<key>EXAppExtensionAttributes</key><dict>
		<key>EXExtensionPointIdentifier</key><string>com.apple.quicklook.thumbnail</string>
	</dict>
</dict></plist>`, "Thumb.appex")
	if _, ok := readPane(notPane); ok {
		t.Error("a thumbnail extension was taken for a settings pane")
	}
}

// The pane's bundle path is its identity and 电池's is built from it, so a row can be opened
// without a second lookup.
func TestPaneURLIsBuiltFromTheBundleID(t *testing.T) {
	pane := &launcherApp{Pane: true, BundleID: "com.apple.BluetoothSettings"}
	if got := pane.paneURL(); got != "x-apple.systempreferences:com.apple.BluetoothSettings" {
		t.Errorf("paneURL = %q", got)
	}
	app := &launcherApp{Path: "/Applications/X.app"}
	if got := app.paneURL(); got != "" {
		t.Errorf("an application answered paneURL %q", got)
	}
}

// A pane gets no location line: its bundle sits in an .appex directory nobody recognises, and panes
// are never duplicated the way two copies of an app can be.
func TestPaneRowsHaveNoDetailLine(t *testing.T) {
	pane := &launcherApp{Path: "/System/Library/ExtensionKit/Extensions/Bluetooth.appex", Pane: true}
	if got := pane.detail(); got != "" {
		t.Errorf("pane detail = %q, want empty", got)
	}
	app := &launcherApp{Path: "/Applications/X.app"}
	if got := app.detail(); got != "/Applications" {
		t.Errorf("app detail = %q", got)
	}
}

// The sidebar's own label wins, and the predicate picks the label: a laptop shows 电池 where a
// desktop shows 能耗 — the two are representations of ONE page.
func TestSidebarNameFollowsThePredicate(t *testing.T) {
	loc := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
	<key>zh_CN</key><dict>
		<key>BATTERY_PREF_TITLE</key><string>电池</string>
		<key>ENERGY_SAVER_PREF_TITLE</key><string>能耗</string>
	</dict>
</dict></plist>`
	bundle := writeBundleWithPlists(t, map[string]string{
		"Info.plist": `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
	<key>CFBundleIdentifier</key><string>com.example.power</string>
	<key>EXAppExtensionAttributes</key><dict>
		<key>EXExtensionPointIdentifier</key><string>com.apple.Settings.extension.ui</string>
		<key>SettingsExtensionAttributes</key><dict>
			<key>representations</key><array>
				<dict>
					<key>predicate</key><string>powersource.battery != YES</string>
					<key>sidebar-name</key><string>ENERGY_SAVER_PREF_TITLE</string>
				</dict>
				<dict>
					<key>predicate</key><string>powersource.battery == YES</string>
					<key>sidebar-name</key><string>BATTERY_PREF_TITLE</string>
				</dict>
			</array>
		</dict>
	</dict>
</dict></plist>`,
		"Resources/Localizable.loctable": loc,
	}, "Power.appex")

	want := "能耗"
	if batteryPresent() {
		want = "电池"
	}
	if got := sidebarNameOf(bundle); got != want {
		t.Errorf("sidebarNameOf = %q on this machine, want %q", got, want)
	}
}

// An identifier is not a name. A pane that never translated itself reports "BluetoothSettings" or
// "FollowUps" — pages System Settings only lists once the device they are about exists — and a row
// nobody could type is worse than a missing one.
func TestUnreadablePaneNamesAreDropped(t *testing.T) {
	for _, name := range []string{"FollowUps", "HeadphoneSettingsExtension", "GeneralSettings"} {
		if paneNameIsReadable(name) {
			t.Errorf("%q was kept as a name", name)
		}
	}
	for _, name := range []string{"蓝牙", "隐私与安全性", "Wi‑Fi", "VPN", "Siri", "Game Center"} {
		if !paneNameIsReadable(name) {
			t.Errorf("%q was dropped", name)
		}
	}
}

// An app installed from the iOS App Store has no Contents/: the bundle is a wrapper around
// WrappedBundle/. Its Info.plist is what makes the cache work — a stamp that cannot be read means
// the entry looks updated on every rescan, so those apps were re-read on every press.
func TestWrappedBundleIsReadWhereItsInfoPlistIs(t *testing.T) {
	wrapped := filepath.Join(t.TempDir(), "Wrapped.app")
	info := filepath.Join(wrapped, "WrappedBundle", "Info.plist")
	if err := os.MkdirAll(filepath.Dir(info), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(info, []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
	<key>CFBundleDisplayName</key><string>Wrapped</string>
	<key>CFBundleIdentifier</key><string>com.example.wrapped</string>
</dict></plist>`), 0o644); err != nil {
		t.Fatal(err)
	}

	name, _, bundleID, stamp := readAppName(wrapped)
	if stamp == 0 {
		t.Fatal("no stamp: this entry would be re-read on every rescan")
	}
	if name != "Wrapped" || bundleID != "com.example.wrapped" {
		t.Errorf("name = %q, id = %q", name, bundleID)
	}
	if got := bundleRoot(wrapped); got != filepath.Join(wrapped, "WrappedBundle") {
		t.Errorf("bundleRoot = %q", got)
	}
}
