package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The filter decides what counts as evidence of a stale build. Both directions matter: a missed
// input lets the run measure old code, and a file that is not an input (a Makefile, a test, a
// node_modules entry) makes the check cry wolf — which is how a reader learns to skip it.
func TestBuildInput(t *testing.T) {
	tests := []struct {
		rel  string
		want bool
	}{
		{"main.go", true},
		{"link/link.go", true},
		{"go.mod", true},
		{"go.sum", true},
		{"stt/shim.swift", true},
		{"menuicon/tray.png", true},
		{"frontend/dist/index.html", true},
		{"frontend/src/SessionList.tsx", true},
		{"frontend/package.json", true},
		{"frontend/bindings/router.ts", true},

		{"main_test.go", false}, // a test file is not in the binary
		{"frontend/node_modules/react/index.js", false},
		{"Makefile", false},
		{"Taskfile.yml", false},
		{"build/linux/Taskfile.yml", false},
		{"omarchy/Bar.qml", false},
		{"scripts/dev-cert.sh", false},
		{"README.md", false},
	}
	for _, tc := range tests {
		if got := buildInput(tc.rel); got != tc.want {
			t.Errorf("buildInput(%q) = %v, want %v", tc.rel, got, tc.want)
		}
	}
}

// touch writes a file and stamps its mtime; both halves are the fixture here.
func touch(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// staleFixture is a module that looks enough like desktop/ for the walk: one input of each kind the
// filter accepts, plus the traps it must ignore.
type staleFixture struct {
	module string
	exe    string
	built  time.Time
}

func newStaleFixture(t *testing.T) staleFixture {
	t.Helper()
	module := t.TempDir()
	built := time.Date(2026, 11, 1, 8, 1, 0, 0, time.Local)
	f := staleFixture{module: module, exe: filepath.Join(module, "bin", "tachi-desktop"), built: built}
	touch(t, filepath.Join(module, "go.mod"), built.Add(-time.Hour))
	touch(t, filepath.Join(module, "main.go"), built.Add(-time.Hour))
	touch(t, filepath.Join(module, "main_test.go"), built.Add(-time.Hour))
	touch(t, filepath.Join(module, "frontend", "dist", "index.html"), built.Add(-time.Hour))
	touch(t, filepath.Join(module, "Makefile"), built.Add(-time.Hour))
	touch(t, filepath.Join(module, "frontend", "node_modules", "react", "index.js"), built.Add(-time.Hour))
	touch(t, f.exe, built)
	return f
}

func TestFindStaleApp(t *testing.T) {
	t.Run("binary newer than everything it is built from", func(t *testing.T) {
		f := newStaleFixture(t)
		if st, stale := findStaleApp(f.exe, f.module, staleSlack); stale {
			t.Fatalf("fresh build reported stale: %s", st)
		}
	})

	t.Run("frontend source newer than the binary", func(t *testing.T) {
		f := newStaleFixture(t)
		touch(t, filepath.Join(f.module, "frontend", "src", "SessionList.tsx"), f.built.Add(11*time.Hour))
		st, stale := findStaleApp(f.exe, f.module, staleSlack)
		if !stale {
			t.Fatal("a source 11h newer than the binary was not reported")
		}
		if st.newest != "frontend/src/SessionList.tsx" {
			t.Errorf("newest = %q, want the file that is actually newer", st.newest)
		}
		if st.newer != 1 {
			t.Errorf("newer = %d, want 1", st.newer)
		}
	})

	t.Run("the newest input is the one named", func(t *testing.T) {
		f := newStaleFixture(t)
		touch(t, filepath.Join(f.module, "main.go"), f.built.Add(time.Hour))
		touch(t, filepath.Join(f.module, "frontend", "dist", "index.html"), f.built.Add(2*time.Hour))
		st, stale := findStaleApp(f.exe, f.module, staleSlack)
		if !stale {
			t.Fatal("stale build not reported")
		}
		if st.newest != "frontend/dist/index.html" {
			t.Errorf("newest = %q, want the most recent of the two", st.newest)
		}
		if st.newer != 2 {
			t.Errorf("newer = %d, want 2", st.newer)
		}
	})

	// The three traps: each is newer than the binary, none is in it. A check that fires on these
	// costs a rebuild for nothing and teaches the reader to ignore it.
	for _, rel := range []string{
		"Makefile",
		"main_test.go",
		"frontend/node_modules/react/index.js",
	} {
		t.Run("not an input: "+rel, func(t *testing.T) {
			f := newStaleFixture(t)
			touch(t, filepath.Join(f.module, filepath.FromSlash(rel)), f.built.Add(time.Hour))
			if st, stale := findStaleApp(f.exe, f.module, staleSlack); stale {
				t.Fatalf("%s is not in the binary, but the run was refused: %s", rel, st)
			}
		})
	}

	// Slack exists so a build and a file write in the same second do not read as a stale build;
	// it must not swallow the hours-wide case above.
	t.Run("slack", func(t *testing.T) {
		f := newStaleFixture(t)
		touch(t, filepath.Join(f.module, "main.go"), f.built.Add(time.Second))
		if _, stale := findStaleApp(f.exe, f.module, staleSlack); stale {
			t.Error("an input within the slack window was reported as stale")
		}
		touch(t, filepath.Join(f.module, "main.go"), f.built.Add(5*time.Second))
		if _, stale := findStaleApp(f.exe, f.module, staleSlack); !stale {
			t.Error("an input past the slack window was not reported")
		}
	})

	t.Run("no binary yet is not a stale binary", func(t *testing.T) {
		f := newStaleFixture(t)
		if _, stale := findStaleApp(filepath.Join(f.module, "bin", "nope"), f.module, staleSlack); stale {
			t.Error("a missing binary was reported as stale; prepareApp has the better message for that")
		}
	})
}

// The preflight only applies to a binary that came out of a module laid out like desktop/. Anything
// else is somebody measuring a build from elsewhere, and there is no local tree to compare it to.
func TestDesktopModuleDir(t *testing.T) {
	f := newStaleFixture(t)
	module, ok := desktopModuleDir(f.exe)
	if !ok || module != f.module {
		t.Fatalf("desktopModuleDir(%q) = %q, %v; want %q, true", f.exe, module, ok, f.module)
	}
	if _, ok := desktopModuleDir(filepath.Join(t.TempDir(), "bin", "tachi-desktop")); ok {
		t.Error("a binary with no module above it was accepted")
	}
}
