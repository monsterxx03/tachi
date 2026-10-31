//go:build darwin

package main

// checkWindowSize is the Linux half's question (window_other.go): macOS gives the window the
// size it asked for, and the suite is developed there, so there is nothing to check.
func checkWindowSize(int) {}
