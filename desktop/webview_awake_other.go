//go:build !darwin

package main

import "github.com/wailsapp/wails/v3/pkg/application"

// keepPageAwake is a no-op off macOS: the behaviour it lifts is WebKit's occlusion
// throttling, and nothing here has been measured on WebKitGTK — a Linux smoke run whose
// driver crawls (the suite's own RUNNER is macOS-only today, but the driver injection path
// works here) should start by asking the question this file answers for macOS. An empty
// reason means there is nothing here to leave switched off.
func keepPageAwake(*application.WebviewWindow) string { return "" }
