package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/itest/mockllm"
)

// The sandbox: everything a run touches lives under one temp dir, so a scenario can
// create sessions, write plans and dirty a git tree without ever reaching the real
// ~/.tachi.
//
//	<sandbox>/
//	  TachiSmoke.app/      a private copy of the app, renamed so pkill can never hit the
//	                       user's own Tachi (see the bundle rules in docs/agents/desktop.md)
//	  home/                $HOME for the run: .tachi/config.yaml, session store, UI state
//	  work/                the session's working directory (what tools and @-references see)
//	  driver.js            harness.js + the scenario, with the sink URL filled in
//	  mock-requests.txt    what the model was asked, for the Go-side assertions
type sandbox struct {
	dir     string
	appPath string
	home    string
	work    string
}

// smokeBundleName is deliberately unique: `open -a <path>` matches bundles and
// `pkill -f Tachi.app/Contents/MacOS/Tachi` matches the executable path, so either one
// aimed at the real app kills the user's running instance.
const smokeExecName = "TachiSmoke"

func newSandbox(dir, srcApp string) (*sandbox, error) {
	sb := &sandbox{
		dir:     dir,
		appPath: filepath.Join(dir, smokeExecName+".app"),
		home:    filepath.Join(dir, "home"),
		work:    filepath.Join(dir, "work"),
	}
	for _, d := range []string{sb.home, sb.work, filepath.Join(sb.home, ".tachi")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	if err := sb.prepareApp(srcApp); err != nil {
		return nil, err
	}
	return sb, nil
}

// prepareApp copies the built bundle and re-identifies it as the smoke app: refresh the
// executable from the freshly built bare binary, rename it (the pkill guard), rewrite the
// bundle identity with plutil, re-sign ad-hoc.
//
// The bundle (assets, Info.plist) comes from `make build`; the executable comes from
// `go build -o bin/Tachi .`, which is what the Makefile target re-runs before each suite —
// so the app under test is always the current tree, never a stale package.
func (sb *sandbox) prepareApp(srcApp string) error {
	if _, err := os.Stat(srcApp); err != nil {
		return fmt.Errorf("app bundle %s: %w (run `cd desktop && make build` first)", srcApp, err)
	}
	if err := run("cp", "-R", srcApp, sb.appPath); err != nil {
		return fmt.Errorf("copy bundle: %w", err)
	}
	macOS := filepath.Join(sb.appPath, "Contents", "MacOS")
	target := filepath.Join(macOS, "Tachi")
	if _, err := os.Stat(target); err != nil {
		return fmt.Errorf("bundle has no Contents/MacOS/Tachi: %w", err)
	}
	if bare := filepath.Join(filepath.Dir(srcApp), "Tachi"); fileExists(bare) {
		if err := run("cp", bare, target); err != nil {
			return fmt.Errorf("refresh executable from %s: %w", bare, err)
		}
	} else {
		return fmt.Errorf("no %s next to the bundle: build the binary first (`cd desktop && go build -o bin/Tachi .`)", bare)
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
	// Ad-hoc signing is enough here: the smoke app never asks for TCC permissions
	// (notifications are only exercised by hand — see docs/agents/desktop.md).
	return run("codesign", "--force", "--deep", "--sign", "-", sb.appPath)
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// writeConfig points the app at the scenario's mock. Every scenario gets its own provider
// entry, which is why the config is written here and not checked in: the mock's port is
// random.
func (sb *sandbox) writeConfig(baseURL string) error {
	cfg := fmt.Sprintf(`provider: smoke
providers:
  - name: smoke
    type: openai
    model: smoke-model
    base_url: %s
    api_key: smoke-key
    spec:
      context_window: 128000
title_generation: false
language: zh
herdr:
  enabled: false
`, baseURL)
	return os.WriteFile(filepath.Join(sb.home, ".tachi", "config.yaml"), []byte(cfg), 0o600)
}

// seedFiles writes the scenario's working-directory fixtures.
func (sb *sandbox) seedFiles(files map[string]string) error {
	for name, content := range files {
		p := filepath.Join(sb.work, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// gitInit makes the work dir a repository with one commit, so "the working tree is
// dirty" scenarios (diff review) have something to be dirty against.
func (sb *sandbox) gitInit() error {
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "-A"},
		{"-c", "user.email=smoke@test", "-c", "user.name=smoke", "commit", "-qm", "init"},
	} {
		if err := runIn(sb.work, "git", args...); err != nil {
			return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
	}
	return nil
}

// writeDriver concatenates the shared harness and the scenario driver into the single
// file TACHI_DEMO_JS points at, filling in the sink URL.
func (sb *sandbox) writeDriver(driversDir, scenarioName, sinkURL string) (string, error) {
	harness, err := os.ReadFile(filepath.Join(driversDir, "harness.js"))
	if err != nil {
		return "", err
	}
	body, err := os.ReadFile(filepath.Join(driversDir, scenarioName+".js"))
	if err != nil {
		return "", err
	}
	script := strings.ReplaceAll(string(harness)+"\n"+string(body), "__SINK__", sinkURL)
	script = strings.ReplaceAll(script, "__SCENARIO__", scenarioName)
	path := filepath.Join(sb.dir, "driver.js")
	return path, os.WriteFile(path, []byte(script), 0o644)
}

// launch starts the app through LaunchServices (`open`), which is what makes its window
// frontmost and interactive (a direct exec would run, but attached to nothing). The env vars
// are how the app learns about the isolated HOME and the driver.
func (sb *sandbox) launch() error {
	return run("open", sb.appPath,
		"--env", "HOME="+sb.home,
		"--env", "TACHI_DEMO=1",
		"--env", "TACHI_DEMO_JS="+filepath.Join(sb.dir, "driver.js"),
	)
}

// killAll stops any smoke instance. The executable name is unique to the sandbox, so this
// cannot touch the user's own app.
func killAll() {
	_ = exec.Command("pkill", "-f", smokeExecName).Run()
	// Wait for it to actually go away: `open` on a live instance activates it and
	// passes NO environment, which would silently run the next scenario in the
	// previous (or a leaked) app.
	for i := 0; i < 40; i++ {
		out, _ := exec.Command("pgrep", "-f", smokeExecName).Output()
		if strings.TrimSpace(string(out)) == "" {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// smokeRunning reports whether an instance survived killAll (preflight guard).
func smokeRunning() bool {
	out, _ := exec.Command("pgrep", "-f", smokeExecName).Output()
	return strings.TrimSpace(string(out)) != ""
}

// dumpMockRequests writes what the model was asked — the LLM-boundary half of the
// evidence, which no amount of DOM inspection can show.
func dumpMockRequests(path string, requests []*mockllm.RecordedRequest) error {
	var b strings.Builder
	for i, r := range requests {
		fmt.Fprintf(&b, "--- request %d ---\n", i+1)
		for _, m := range r.Messages {
			fmt.Fprintf(&b, "[%s] %s\n", m.Role, truncate(m.Content, 400))
		}
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// readJSON decodes a file into v, for the Go-side assertions (e.g. the saved plan).
func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// run runs a command and folds its stderr into the error, so a failure is one readable
// line instead of "exit status 1".
func run(name string, args ...string) error { return runIn("", name, args...) }

func runIn(dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
