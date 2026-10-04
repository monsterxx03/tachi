//go:build !darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The Linux half of the sandbox's app handling. Where macOS has a .app bundle to copy,
// re-identify and re-sign (sandbox_darwin.go), the app here is a BARE BINARY — `make -C desktop
// build` leaves `desktop/bin/Tachi` — so the whole dance collapses to "copy it under a name of
// our own and start it".
//
// Everything else about a scenario is the same on both platforms: the isolated HOME, the
// seeded session store, the mock, the driver and the sink. See sandbox.go.

// defaultAppPath is what `-app` defaults to: the binary `make -C desktop build` leaves behind.
// NOT a bundle — there is no bundling step on this platform (see desktop/Makefile).
func defaultAppPath() string { return "desktop/bin/Tachi" }

// smokeAppPath is the sandbox's private copy of the app under test. The name is unique for the
// same reason the macOS one is: `pkill -f` in killAll must never reach the user's own Tachi.
func smokeAppPath(dir string) string { return filepath.Join(dir, smokeExecName) }

// prepareApp gives the sandbox its private copy of the built binary.
//
// Hardlinked, NOT copied: /tmp is a tmpfs on many Linux boxes, and one 60-70MB binary per scenario
// (68 of them, and a failing run keeps every sandbox for its artifacts) is several gigabytes —
// measured as a run whose second half failed with `cp: error writing …`, which reads like a
// scenario bug and is a full disk. The app is never written to, so a second name for the same
// inode is exactly as good as a copy, and free. The fallback covers a /tmp on another filesystem
// from the build tree.
//
// No re-identification is possible or needed here: there is no Info.plist to rewrite (the window's
// app_id comes from the app Name at runtime — see the Linux section of docs/agents/desktop.md) and
// no signature to refresh.
func (sb *sandbox) prepareApp(srcApp string) error {
	if _, err := os.Stat(srcApp); err != nil {
		return fmt.Errorf("app binary %s: %w (run `make -C desktop build` first)", srcApp, err)
	}
	if err := os.Link(srcApp, sb.appPath); err != nil {
		if err := run("cp", srcApp, sb.appPath); err != nil {
			return fmt.Errorf("copy app: %w", err)
		}
	}
	if err := os.Chmod(sb.appPath, 0o755); err != nil {
		return fmt.Errorf("chmod app: %w", err)
	}
	return nil
}

// launch starts the app as a child process with the scenario's environment.
//
// It is deliberately NOT `TACHI_DEMO_NO_ACTIVATE` (which macOS sets): that env makes the app
// create its window HIDDEN, and macOS's demo bootstrap then shows it without activating it. The
// GTK window has no such lever, and a hidden one is not merely invisible — its page is laid out
// at 0×0 (measured: the driver runs, every `getBoundingClientRect` is zero), which would turn
// every geometry assertion in the suite into a comparison of zeroes. So a Linux run is VISIBLE,
// and the compositor may well focus it (the trade macOS buys with `open -g` + an accessory
// activation policy does not exist here). One window per scenario is the price of the suite on
// this platform.
//
// The app's own output goes to the sandbox (app.log): when a scenario fails, "the app printed
// something" is the first thing worth reading, and `open` on macOS throws that away.
func (sb *sandbox) launch() error {
	cmd := exec.Command(sb.appPath)
	cmd.Env = envWith(sb.demoEnv())
	logPath := filepath.Join(sb.dir, "app.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return fmt.Errorf("app log: %w", err)
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("start %s: %w", sb.appPath, err)
	}
	// Reap it in the background: we never wait for it (killAll ends it), but an exited child
	// left unreaped is a zombie per scenario.
	go func() { _ = cmd.Wait(); _ = logFile.Close() }()
	return nil
}

// envWith returns the parent's environment with overrides applied, and — the point — with the
// parent's own copies of those keys REMOVED rather than shadowed.
//
// A duplicated variable is a trap, not a tie: Go resolves it last-wins, glib and the GTK/WebKit
// stack resolve it first-wins, and nothing in the child says which it got. Appending the sandbox
// env after os.Environ() (or before it) therefore leaves one of the two readers — the app and the
// libraries under it — resolving HOME to the REAL home. Measured, the hard way: the suite ran the
// whole app against ~/.tachi, seeding nothing it had asked for, creating sessions in the user's
// own store, and reporting failures that looked like UI bugs.
func envWith(overrides map[string]string) []string {
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			if _, over := overrides[kv[:i]]; over {
				continue
			}
		}
		env = append(env, kv)
	}
	for k, v := range overrides {
		env = append(env, k+"="+v)
	}
	return env
}
