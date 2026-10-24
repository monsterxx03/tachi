//go:build darwin && !headless

package main

import "os/exec"

// platformOpenFile hands a path to macOS `open`.
func platformOpenFile(args ...string) error {
	return exec.Command("open", args...).Start()
}

// platformRevealFile asks Finder to reveal the item (`open -R`).
func platformRevealFile(path string) error {
	return exec.Command("open", "-R", path).Start()
}
