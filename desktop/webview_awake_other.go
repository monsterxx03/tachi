//go:build !darwin

package main

import "github.com/wailsapp/wails/v3/pkg/application"

// keepPageAwake is a no-op off macOS: the behaviour it lifts is WebKit's occlusion
// throttling, and the desktop smoke suite only runs on macOS. An empty reason means there is
// nothing here to leave switched off.
func keepPageAwake(*application.WebviewWindow) string { return "" }
