package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Why this file exists: a smoke run measures the BINARY, not the tree. The app is built once
// (`make -C desktop frontend && go build -o bin/tachi-desktop .`) and then driven by every
// scenario, so a binary left over from before the change under test makes the run report on code
// nobody edited — and it reports with full confidence, which is the expensive part. That happened:
// a suite came back 3/97 red against a binary built at 08:01 while the frontend it embeds had been
// rewritten at 19:46, and the "regressions" all vanished on a rebuild. Nothing in the run said
// which build was being measured, so the only defence was remembering — this is that defence, in
// the one place that cannot forget.

// staleSlack is how much newer an input may be before it counts as a stale build. It absorbs
// filesystem timestamp granularity and the few milliseconds between a build reading a file and
// linking the binary; the mistake this exists for is hours wide, so slack costs nothing.
const staleSlack = 2 * time.Second

// staleApp is the evidence that the binary under test predates the tree it is built from.
type staleApp struct {
	exe    string    // the binary the sandbox will run
	built  time.Time // its mtime
	newest string    // the newest build input, slash-separated, relative to the module dir
	at     time.Time // that input's mtime
	newer  int       // how many inputs are newer than the binary (newest included)
}

// String is the whole message the reader gets, because the useful part is not "stale" but
// WHICH file is newer: that is the change the run would have failed to measure.
func (s staleApp) String() string {
	var b strings.Builder
	b.WriteString("the app under test is older than the tree it is built from — this run would report on the OLD code:\n")
	fmt.Fprintf(&b, "  built   %s  %s\n", s.exe, s.built.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "  newer   %s  %s\n", s.newest, s.at.Format("2006-01-02 15:04:05"))
	if s.newer > 1 {
		fmt.Fprintf(&b, "  (%d files it is built from are newer than the binary; the newest is the line above)\n", s.newer)
	}
	b.WriteString("rebuild it: (cd desktop && make frontend && GOWORK=off go build -o bin/tachi-desktop .)\n")
	b.WriteString("or run `make desktop-smoke`, which builds before it drives.")
	return b.String()
}

// findStaleApp compares the binary the sandbox will run against the files it is built from, and
// reports the evidence when the binary is behind. ok is false when there is nothing to compare:
// no binary yet, or an app that did not come out of this module — prepareApp reports those with a
// better message than "stale" would be.
func findStaleApp(exe, moduleDir string, slack time.Duration) (staleApp, bool) {
	info, err := os.Stat(exe)
	if err != nil || info.IsDir() {
		return staleApp{}, false
	}
	st := staleApp{exe: exe, built: info.ModTime()}
	cutoff := st.built.Add(slack)
	// A walk error is not evidence of anything: an unreadable file cannot make a build stale, and
	// turning "I could not look" into "your binary is old" would be the same class of lie as the
	// one this check exists to stop.
	_ = filepath.WalkDir(moduleDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, relErr := filepath.Rel(moduleDir, p)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && skipTree(rel) {
				return fs.SkipDir
			}
			return nil
		}
		if !buildInput(rel) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if mt := fi.ModTime(); mt.After(cutoff) {
			st.newer++
			if mt.After(st.at) {
				st.newest, st.at = rel, mt
			}
		}
		return nil
	})
	return st, st.newer > 0
}

// skipTree names the directories under the module that hold no build input: build output, the
// installed packages, and go-task's cache. Walking node_modules would cost a second per run and
// could only ever produce a false verdict.
func skipTree(rel string) bool {
	return rel == "bin" || filepath.Base(rel) == "node_modules" || rel == ".task"
}

// buildInput reports whether rel (slash-separated, relative to the desktop module) ends up INSIDE
// the app binary. The rest of the tree only decides how it is built — a Makefile or Taskfile edit
// does not make an existing binary stale, and claiming it does would teach the reader to ignore
// this check, which is worse than not having it.
func buildInput(rel string) bool {
	switch {
	case strings.HasSuffix(rel, ".go"):
		// A test file is not in the binary; editing one is not a reason to rebuild it.
		return !strings.HasSuffix(rel, "_test.go")
	case rel == "go.mod" || rel == "go.sum":
		return true
	case strings.HasSuffix(rel, ".swift"):
		// stt's cgo shim is compiled into a static library the build links (desktop/stt).
		return true
	case strings.HasPrefix(rel, "menuicon/"):
		return true // //go:embed menuicon/*
	case strings.HasPrefix(rel, "frontend/"):
		// dist is embedded (//go:embed all:frontend/dist); src and the build configs are what it is
		// built FROM, and a dist that was never rebuilt is the same bug one step earlier.
		return !strings.HasPrefix(rel, "frontend/node_modules/")
	}
	return false
}

// desktopModuleDir is the module the binary came out of: the Makefile writes bin/tachi-desktop, so
// the module sits two levels up. A binary from anywhere else (a release, a copy in /tmp) has no
// tree here to be compared against, and ok is false.
func desktopModuleDir(exe string) (string, bool) {
	dir := filepath.Dir(filepath.Dir(exe))
	if fi, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil || fi.IsDir() {
		return "", false
	}
	if fi, err := os.Stat(filepath.Join(dir, "frontend")); err != nil || !fi.IsDir() {
		return "", false
	}
	return dir, true
}
