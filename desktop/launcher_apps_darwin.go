//go:build darwin && !headless

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// launcherAppDirs is where macOS keeps the applications a person launches. The first is where
// everything they installed lives (brew cask and Setapp both link there too); the system ones are
// separated on disk but not in a launcher — 系统设置 is as launchable as Chrome.
func launcherAppDirs() []string {
	dirs := []string{
		"/Applications",
		"/Applications/Utilities",
		"/System/Applications",
		"/System/Applications/Utilities",
		// The system's own odds and ends: 钥匙串访问, 无线诊断, 归档实用工具, 关于本机…
		"/System/Library/CoreServices/Applications",
	}
	if home, err := os.UserHomeDir(); err == nil {
		// ~/Applications is where an app installed without an installer lands (and where some
		// Setapp builds go).
		dirs = append(dirs, filepath.Join(home, "Applications"))
	}
	return dirs
}

// systemBundles are the apps that live somewhere no directory scan can find, listed by path.
//
// Finder is the whole reason: it is at /System/Library/CoreServices/Finder.app, and its PARENT
// directory is not scannable — 118 bundles live there and all but a handful are background agents
// (Dock, 控制中心, loginwindow, 通知中心, Spotlight…). Adding that directory wholesale would fill the
// launcher with things nobody launches by name and a few that must never be launched by hand, so the
// exception is stated one bundle at a time instead.
var systemBundles = []string{
	"/System/Library/CoreServices/Finder.app",
}

// scanAppBundles lists the .app bundles in those directories. Two directories can name the same
// bundle through a symlink (brew links a cask into /Applications), so the result is de-duplicated
// by resolved path — the same app must never take two rows.
func scanAppBundles() []string {
	seen := make(map[string]bool)
	var paths []string
	for _, path := range systemBundles {
		if isDirOrLink(path) {
			seen[path] = true
			paths = append(paths, path)
		}
	}
	for _, dir := range launcherAppDirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".app") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			if !isDirOrLink(path) {
				continue
			}
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				resolved = path
			}
			if seen[resolved] {
				continue
			}
			seen[resolved] = true
			paths = append(paths, path)
		}
	}
	return paths
}

// isDirOrLink reports whether path is a directory, following a symlink or not — a cask's bundle
// is a link into /opt/homebrew/Caskroom and os.ReadDir reports the link, not the directory.
func isDirOrLink(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// localizations are the Chinese resources to read a bundle's localized name from, in order. macOS
// ships a bundle's name in English in Info.plist and the translated one in the .lproj beside it —
// 「微信」 lives only in zh-Hans.lproj, so an app found by its English name cannot be found by the
// name its owner uses unless this is read.
var localizations = []string{"zh-Hans.lproj", "zh_CN.lproj"}

// loctableLanguages are the keys a Chinese name sits under in Resources/InfoPlist.loctable — the
// other shape of the same thing, one table holding every language at once rather than a directory
// per language. The system's own applications use this one and nothing else: 「访达」 is nowhere in
// Finder's Info.plist or in any .lproj, only in that table (the old-style language names are the
// table's, so it is zh_CN here and not zh-Hans).
var loctableLanguages = []string{"zh_CN"}

// localizedName is the Chinese name a bundle gives itself, from whichever of those two shapes it
// uses, or "" when it has neither. A bundle is free to ship both, so the .lproj is asked first —
// that is the shape an application's own localizations take, while the table belongs to the system.
func localizedName(contents string) string {
	resources := filepath.Join(contents, "Resources")
	for _, lproj := range localizations {
		if name := bundleNameOf(plutilJSON(filepath.Join(resources, lproj, "InfoPlist.strings"))); name != "" {
			return name
		}
	}
	table := plutilJSON(filepath.Join(resources, "InfoPlist.loctable"))
	for _, language := range loctableLanguages {
		entry, ok := table[language].(map[string]any)
		if !ok {
			continue
		}
		if name := bundleNameOf(entry); name != "" {
			return name
		}
	}
	return ""
}

// bundleNameOf picks a display name out of a bundle's name fields, wherever they were decoded from.
func bundleNameOf(fields map[string]any) string {
	return firstNonEmpty(stringField(fields, "CFBundleDisplayName"), stringField(fields, "CFBundleName"))
}

// bundleRoot is the directory holding a bundle's Info.plist.
//
// A macOS app keeps everything in Contents/. An app INSTALLED FROM THE iOS APP STORE does not:
// 小宇宙.app and several others are a wrapper around WrappedBundle/, with Contents/ absent
// entirely. The test is the Info.plist's presence rather than the directory's — a bundle whose
// Contents/ exists but holds no plist is not one this code can read anyway, and guessing wrong
// costs a stamp: without one, the cache believes the app changed on every rescan.
func bundleRoot(appPath string) string {
	for _, candidate := range []string{"Contents", "WrappedBundle"} {
		root := filepath.Join(appPath, candidate)
		if isFile(filepath.Join(root, "Info.plist")) {
			return root
		}
	}
	return filepath.Join(appPath, "Contents")
}

// isFile reports whether path is a readable regular file.
func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// readAppName resolves what to CALL a bundle, plus the other names it answers to.
//
// The display name is the localized one when there is one, else Info.plist's, else the file name.
// Every other candidate becomes an alias, so 微信/WeChat/WeChat.app all find the same row. stamp is
// the Info.plist's modification time, which is what the cache compares to notice an app update.
func readAppName(appPath string) (name string, aliases []string, bundleID string, stamp int64) {
	contents := bundleRoot(appPath)
	infoPath := filepath.Join(contents, "Info.plist")
	if info, err := os.Stat(infoPath); err == nil {
		stamp = info.ModTime().UnixNano()
	}

	base := strings.TrimSuffix(filepath.Base(appPath), ".app")
	info := plutilJSON(infoPath)
	name = bundleNameOf(info)
	bundleID = stringField(info, "CFBundleIdentifier")
	if zh := localizedName(contents); zh != "" {
		name = zh
	}
	if name == "" {
		name = base
	}
	for _, candidate := range []string{base, bundleNameOf(info)} {
		if candidate == "" || candidate == name {
			continue
		}
		// Aliases are matched case-insensitively, so a duplicate that differs only in case would
		// be one alias the cache stores twice.
		duplicate := false
		for _, existing := range aliases {
			if strings.EqualFold(existing, candidate) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			aliases = append(aliases, candidate)
		}
	}
	return name, aliases, bundleID, stamp
}

// plutilJSON decodes a plist (binary or XML, Info.plist and .strings alike) through the system's
// own reader: `plutil -convert json` turns either into one shape, which keeps a plist parser out
// of this module. A missing or unreadable file answers nil — a bundle without a readable Info.plist
// is still launchable by its file name.
func plutilJSON(path string) map[string]any {
	ctx, cancel := context.WithTimeout(context.Background(), plistTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "plutil", "-convert", "json", "-o", "-", path).Output()
	if err != nil {
		return nil
	}
	var decoded map[string]any
	if json.Unmarshal(out, &decoded) != nil {
		return nil
	}
	return decoded
}

// plutilJSONValue is plutilJSON for a plist whose top level is not a dictionary — the System
// Settings sidebar is an ARRAY of sections. A dictionary file answers the same map plutilJSON
// would; anything else that does not decode answers nil.
func plutilJSONValue(path string) any {
	ctx, cancel := context.WithTimeout(context.Background(), plistTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "plutil", "-convert", "json", "-o", "-", path).Output()
	if err != nil {
		return nil
	}
	var decoded any
	if json.Unmarshal(out, &decoded) != nil {
		return nil
	}
	return decoded
}

// plistTimeout bounds one plutil call. It is a local file read, so the only way to reach it is a
// pathological plist; the bound exists so a corrupt one cannot hold up the scan.
const plistTimeout = 5 * time.Second

// plistWorkers is how many bundles are read at once. Each read is a subprocess, and the whole scan
// is a few hundred milliseconds of them on a first run (afterwards the cache answers).
const plistWorkers = 8

// readApps reads every bundle's name in parallel. Order follows paths.
func readApps(paths []string) []*launcherApp {
	apps := make([]*launcherApp, len(paths))
	slots := make(chan struct{}, plistWorkers)
	var wg sync.WaitGroup
	for i, path := range paths {
		wg.Add(1)
		slots <- struct{}{}
		go func(i int, path string) {
			defer wg.Done()
			defer func() { <-slots }()
			apps[i] = newLauncherApp(path)
		}(i, path)
	}
	wg.Wait()
	return apps
}

// openApp launches a bundle through LaunchServices — the same call the Finder makes, so an app
// that is already running is brought forward instead of opened twice.
func openApp(path string) error {
	return exec.Command("open", "-a", path).Run()
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
