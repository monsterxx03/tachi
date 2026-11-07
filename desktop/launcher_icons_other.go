//go:build !darwin && !headless

package main

// There is no bundle icon to draw off macOS (see launcher_icons_darwin.go): the panel falls back to
// the letter tile, which is why this answers a plain "not available" instead of failing the scan.
func writeAppIcon(appPath, outPath string, size int) error { return errLauncherUnsupported }
