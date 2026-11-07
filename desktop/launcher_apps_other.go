//go:build !darwin && !headless

package main

import "errors"

// errLauncherUnsupported is what a platform without an implementation answers: the window and the
// ranking work, there is simply nothing to list or open.
var errLauncherUnsupported = errors.New("launcher: opening applications is not supported on this platform")

// The launcher is macOS-only for now: it finds bundles under the macOS application directories and
// launches them through LaunchServices. Linux has its own answer (the .desktop entries and the
// desktop's own Exec line) and it is not written yet, so there the launcher simply has nothing to
// list — the window and the ranking are shared, only discovery and launching live behind this pair.

func scanAppBundles() []string { return nil }

func readApps(paths []string) []*launcherApp { return make([]*launcherApp, len(paths)) }

func openApp(path string) error { return errLauncherUnsupported }

// readAppName has no bundle to read off macOS; the file-name shape is kept so the catalog's
// accounting code stays the same on both sides.
func readAppName(appPath string) (name string, aliases []string, bundleID string, stamp int64) {
	return appPath, nil, "", 0
}
