//go:build !darwin

package main

import (
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
)

// platformOpenFile hands a path to the desktop's default handler. xdg-open is the
// freedesktop convention every Linux DE ships; there is no per-DE API to reach for
// (Hyprland has no launcher of its own either).
func platformOpenFile(args ...string) error {
	return exec.Command("xdg-open", args...).Start()
}

// platformRevealFile asks the desktop's file manager to SHOW the item rather than
// open it: org.freedesktop.FileManager1.ShowItems is the standard "reveal in folder"
// call, the same one a file manager's own context menu uses.
//
// The fallback is the point of the split. FileManager1 is only answered while a file
// manager that implements it is running, and on a bare window manager (or one whose
// file manager does not register the name) the call fails — opening the containing
// directory still lands the user next to the file, which beats a dead action.
func platformRevealFile(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if err := showInFileManager(abs); err == nil {
		return nil
	}
	return platformOpenFile(filepath.Dir(abs))
}

// showInFileManager calls FileManager1.ShowItems for one file through gdbus (shipped
// with glib2). Run, not Start: the failure has to be seen to decide the fallback. The
// trailing argument is the startup-id, empty because we are not an initiating app.
func showInFileManager(abs string) error {
	uri := (&url.URL{Scheme: "file", Path: abs}).String()
	return exec.Command("gdbus", "call", "--session",
		"--dest", "org.freedesktop.FileManager1",
		"--object-path", "/org/freedesktop/FileManager1",
		"--method", "org.freedesktop.FileManager1.ShowItems",
		fmt.Sprintf("['%s']", uri), "").Run()
}
