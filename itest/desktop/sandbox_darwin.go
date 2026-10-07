//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The macOS half of the sandbox's app handling: a real .app bundle, which is what `make build`
// produces here and what the suite must RE-IDENTIFY before it can be driven — see prepareApp.
// The Linux half (sandbox_other.go) has no bundle at all: the app there is a bare binary.
// Which half is in play decides only how the app is COPIED and STARTED; everything else about a
// scenario (the isolated HOME, the driver, the sink) is shared — see sandbox.go.

// defaultAppPath is what `-app` defaults to: the bundle `make -C desktop build` leaves behind.
func defaultAppPath() string { return "desktop/bin/tachi-desktop.app" }

// smokeAppPath is the sandbox's private copy of the app under test — renamed, so that no
// pkill/`open` aimed at it can reach the user's own running Tachi (see the bundle rules in
// docs/agents/desktop.md).
func smokeAppPath(dir string) string { return filepath.Join(dir, smokeExecName+".app") }

// prepareApp copies the built bundle and re-identifies it as the smoke app: refresh the
// executable from the freshly built bare binary, rename it (the pkill guard), rewrite the
// bundle identity with plutil, re-sign ad-hoc.
//
// The bundle (assets, Info.plist) comes from `make build`; the executable comes from
// `go build -o bin/tachi-desktop .`, which is what the Makefile target re-runs before each suite —
// so the app under test is always the current tree, never a stale package.
func (sb *sandbox) prepareApp(srcApp string) error {
	if _, err := os.Stat(srcApp); err != nil {
		return fmt.Errorf("app bundle %s: %w (run `cd desktop && make build` first)", srcApp, err)
	}
	if err := run("cp", "-R", srcApp, sb.appPath); err != nil {
		return fmt.Errorf("copy bundle: %w", err)
	}
	macOS := filepath.Join(sb.appPath, "Contents", "MacOS")
	target := filepath.Join(macOS, "tachi-desktop")
	if _, err := os.Stat(target); err != nil {
		return fmt.Errorf("bundle has no Contents/MacOS/tachi-desktop: %w", err)
	}
	if bare := filepath.Join(filepath.Dir(srcApp), "tachi-desktop"); fileExists(bare) {
		if err := run("cp", bare, target); err != nil {
			return fmt.Errorf("refresh executable from %s: %w", bare, err)
		}
	} else {
		return fmt.Errorf("no %s next to the bundle: build the binary first (`cd desktop && go build -o bin/tachi-desktop .`)", bare)
	}
	if err := os.Rename(target, filepath.Join(macOS, smokeExecName)); err != nil {
		return err
	}

	plist := filepath.Join(sb.appPath, "Contents", "Info.plist")
	for _, kv := range [][2]string{
		{"CFBundleExecutable", smokeExecName},
		{"CFBundleName", smokeExecName},
		{"CFBundleIdentifier", "com.monsterxx03.tachi.smoke"},
	} {
		if err := run("plutil", "-replace", kv[0], "-string", kv[1], plist); err != nil {
			return fmt.Errorf("plutil %s: %w", kv[0], err)
		}
	}
	// Sign with the local certificate, NOT ad-hoc — see smokeSignID.
	return run("codesign", "--force", "--deep", "--sign", smokeSignID(), sb.appPath)
}

// launch starts the app through LaunchServices (`open`), which is what makes its window
// frontmost and interactive (a direct exec would run, but attached to nothing). The env vars
// are how the app learns about the isolated HOME and the driver.
//
// TACHI_DEMO_NO_ACTIVATE plus `open -g` keep the launch from taking the foreground — the suite
// starts the app once per scenario, and an activated app pulls the window in front of the
// user's work every time (and out of whatever Space they were on). BOTH are needed: `-g` stops
// LaunchServices from asking the app to activate itself, and demo mode then launches as a
// hidden-window accessory app so nothing else activates it either — see activationPolicy and
// demoNoActivate in desktop/demo.go, which spell out what each one was measured to fix.
func (sb *sandbox) launch() error {
	env := []string{"--env", "TACHI_DEMO_NO_ACTIVATE=1"}
	for k, v := range sb.demoEnv() {
		env = append(env, "--env", k+"="+v)
	}
	args := append([]string{"open", "-g", sb.appPath}, env...)
	return run(args[0], args[1:]...)
}

// smokeSignIDDefault is the certificate desktop/Makefile's LOCAL_SIGN_ID names (created once by
// `desktop/scripts/dev-cert.sh`). Kept in sync by hand: itest is its own module and cannot read
// that Makefile.
const smokeSignIDDefault = "Tachi Local Code Signing"

// smokeSignID is the identity the smoke bundle is signed with, or "-" (ad-hoc) when the local
// certificate is not there.
//
// The certificate is the point, not a nicety: an ad-hoc signature's identity IS the content hash,
// so every rebuild is a DIFFERENT app to macOS — and this bundle does ask for a TCC permission,
// on every launch, because desktop links the BLE package and that package builds a CBCentralManager
// in a package-level variable (importing it is enough; nothing has to touch the badge — see
// docs/agents/badge.md). With a stable identity macOS records the answer once (allow OR deny) and
// stops asking; ad-hoc makes the Bluetooth prompt come back on every smoke run.
//
// A machine without the certificate still runs the suite — it just gets asked every time.
func smokeSignID() string {
	id := smokeSignIDDefault
	if override := os.Getenv("TACHI_SMOKE_SIGN_ID"); override != "" {
		id = override
	}
	out, err := exec.Command("security", "find-identity", "-v", "-p", "codesigning").Output()
	if err != nil || !strings.Contains(string(out), "\""+id+"\"") {
		return "-"
	}
	return id
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
