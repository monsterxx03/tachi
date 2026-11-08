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
