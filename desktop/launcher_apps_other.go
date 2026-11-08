//go:build !darwin && !headless

package main

import (
	"errors"
	"path/filepath"
)

// errLauncherUnsupported is what a platform without an implementation answers: the window and the
// ranking work, there is simply nothing to list or open.
var errLauncherUnsupported = errors.New("launcher: opening applications is not supported on this platform")

// The launcher is macOS-only for now: it finds bundles under the macOS application directories and
// launches them through LaunchServices. Linux has its own answer (the .desktop entries and the
// desktop's own Exec line) and it is not written yet, so there the launcher simply has nothing to
// list — the window and the ranking are shared, only discovery and launching live behind this pair.

// panesOf answers the System Settings panes. There are none off macOS for the same reason there
// are no applications: discovery lives behind this pair.
func panesOf() []*launcherApp { return nil }

// openPane opens one of those panes. Unreachable without panesOf, but the pair stays whole.
func openPane(url string) error { return errLauncherUnsupported }

// paneURL is what opens a pane row, and "" for an application. No panes exist here, so no row
// ever asks.
func (a *launcherApp) paneURL() string { return "" }

// bundleRoot answers the directory holding a bundle's Info.plist. There are no bundles here (see
// scanAppBundles), so nothing asks; the name kept so the catalog's accounting compiles on both
// sides.
func bundleRoot(appPath string) string { return filepath.Join(appPath, "Contents") }

func scanAppBundles() []string { return nil }

func readApps(paths []string) []*launcherApp { return make([]*launcherApp, len(paths)) }

func openApp(path string) error { return errLauncherUnsupported }

// readAppName has no bundle to read off macOS; the file-name shape is kept so the catalog's
// accounting code stays the same on both sides.
func readAppName(appPath string) (name string, aliases []string, bundleID string, stamp int64) {
	return appPath, nil, "", 0
}
