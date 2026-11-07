//go:build !darwin && !headless

package main

// launcherCursor has no cross-platform answer here, so the panel falls back to the primary screen
// (see launcher.place). A one-screen machine — which is what Linux desktop support targets today —
// never notices.
func launcherCursor() (int, int, bool) { return 0, 0, false }
