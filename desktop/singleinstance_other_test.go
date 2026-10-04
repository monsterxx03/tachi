//go:build !darwin

package main

import (
	"testing"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// A scripted run must not take the session-bus name. The smoke suite starts its own app while
// the user's own Tachi may be running on the same bus: sharing the name would swallow the
// suite's launch into the user's app (raising that window mid-run) instead of starting the
// isolated instance the run is about — and the suite's preflight, which fails a run when another
// smoke instance is alive, would never even see it.
func TestSingleInstanceIsSkippedForScriptedRuns(t *testing.T) {
	t.Setenv("TACHI_DEMO", "1")
	if got := singleInstanceOptions(func() {}); got != nil {
		t.Errorf("singleInstanceOptions() = %+v during a scripted run, want nil", got)
	}
}

// A normal launch gets the lock, and its callback is the whole point of having one: the second
// launch has to REVEAL the running window, or "open Tachi" would look like it did nothing.
func TestSingleInstanceRevealsTheWindowOnASecondLaunch(t *testing.T) {
	t.Setenv("TACHI_DEMO", "")
	if demoEnabled() {
		t.Skip("a demo flag file is present on this machine")
	}

	revealed := 0
	opts := singleInstanceOptions(func() { revealed++ })
	if opts == nil {
		t.Fatal("singleInstanceOptions() = nil for a normal launch")
	}
	if opts.UniqueID == "" {
		t.Fatal("no UniqueID: the app would have no name to collide on")
	}
	if opts.OnSecondInstanceLaunch == nil {
		t.Fatal("no callback: a second launch would exit without revealing anything")
	}

	opts.OnSecondInstanceLaunch(application.SecondInstanceData{Args: []string{"tachi"}})
	if revealed != 1 {
		t.Errorf("the second-launch callback revealed the window %d times, want 1", revealed)
	}
}
