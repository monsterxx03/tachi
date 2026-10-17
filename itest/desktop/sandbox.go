package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/itest/mcpauth"
	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/monsterxx03/tachi/pkg/fileutil"
)

// The sandbox: everything a run touches lives under one temp dir, so a scenario can
// create sessions, write plans and dirty a git tree without ever reaching the real
// ~/.tachi.
//
//	<sandbox>/
//	  TachiSmoke(.app)     a private copy of the app, renamed so pkill can never hit the
//	                       user's own Tachi (see the bundle rules in docs/agents/desktop.md);
//	                       a bundle on macOS, a bare binary on Linux — see the platform pair
//	  home/                $HOME for the run: .tachi/config.yaml, session store, UI state
//	  work/                the session's working directory (what tools and @-references see)
//	  driver.js            harness.js + the scenario, with the sink URL filled in
//	  mock-requests.txt    what the model was asked, for the Go-side assertions
//	  app.log              the app's own stdout/stderr (Linux only: `open` on macOS discards it)
//	  opened.txt           what the app asked the SYSTEM to open — a link, a file (see demoEnv)
type sandbox struct {
	dir     string
	appPath string
	home    string
	work    string
	// browserFetch asks the app to replace the MCP OAuth browser with a headless fetch
	// (see launch): set by scenarios that authorize an MCP server, because a run must not
	// open a window on the machine it is running on.
	browserFetch bool
	// voiceText, when set, is what a finished recording answers with — the microphone's
	// stand-in (see the scenario field of the same name, and desktop/voice.go).
	voiceText string

	// mcpConfig is the mcp.json exactly as it was seeded. The scenario's Go half compares the
	// file AFTER the run against it: "the panel wrote back exactly one line and left the rest
	// byte-for-byte alone" is a claim about the original bytes, so the original bytes are what
	// the assertion needs — reconstructing them in the assertion would be a second copy of the
	// fixture's formatting, and the copy is the part that goes stale.
	mcpConfig []byte
}

// smokeExecName is deliberately unique: it names both the sandbox's copy of the app and the
// process the suite starts, so `pkill -f TachiSmoke` can only ever reach a smoke run — while
// the REAL app (tachi-desktop.app/Contents/MacOS/tachi-desktop, or bin/tachi-desktop) keeps its own name.
const smokeExecName = "TachiSmoke"

func newSandbox(dir, srcApp string) (*sandbox, error) {
	sb := &sandbox{
		dir:     dir,
		appPath: smokeAppPath(dir),
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

// demoEnv is the environment a scripted run needs, whichever platform starts it: the isolated
// HOME, demo mode, and the driver. What each platform adds on top of it — macOS's
// TACHI_DEMO_NO_ACTIVATE and `open -g`, and the absence of either on Linux — is launch's
// business: see sandbox_darwin.go and sandbox_other.go.
//
// The names are duplicated here rather than shared: the app's copies live in desktop/demo.go,
// desktop/voice.go and desktop/voice.go's browser switch, and the runner cannot import the
// app's own package. Same reason smokeExecName is duplicated.
func (sb *sandbox) demoEnv() map[string]string {
	env := map[string]string{
		"HOME":          sb.home,
		"TACHI_DEMO":    "1",
		"TACHI_DEMO_JS": filepath.Join(sb.dir, "driver.js"),
		// Every launcher action (OpenURL, OpenPath) is RECORDED instead of performed, and the file
		// lives in the sandbox because the runner owns it — see desktop/demo.go. Set for every
		// scenario, not only the one that clicks a link: a run must never open a browser or a Finder
		// window on the machine it is happening on, whoever's driver does the clicking.
		"TACHI_DEMO_OPEN_LOG": filepath.Join(sb.dir, "opened.txt"),
	}
	if sb.browserFetch {
		// The MCP OAuth flow would otherwise open a real browser — on the machine this run is
		// happening on. The app swaps in a headless fetch (desktop/demo.go), which is enough
		// against the fixture's authorization endpoint: it redirects straight back to the
		// callback listener the app already has up.
		env["TACHI_DEMO_BROWSER_FETCH"] = "1"
	}
	if sb.voiceText != "" {
		// The microphone's stand-in: a real recording would raise a permission prompt on the
		// machine running the suite and capture the room. Duplicated from desktop/voice.go
		// (voiceDemoTextEnv) — see above.
		env["TACHI_DEMO_VOICE_TEXT"] = sb.voiceText
	}
	return env
}

// writeConfig points the app at the scenario's mock. Every scenario gets its own provider
// entry, which is why the files are written here and not checked in: the mock's port is
// random.
//
// Two files, because that is what an install has (config/provider_json.go): the preferences in
// config.yaml, the endpoint definition in provider.smoke.json beside it.
//
// extra is the scenario's own YAML block (see scenario.config) — appended verbatim so a
// scenario that needs settings the shared config does not carry (permissions.bash.ask, for
// instance) does not have to widen this function's signature with one field per feature. It
// cannot carry provider settings: providers are not keys of this file.
//
// pricing rides on the provider entry rather than on `extra` because that is where a price table
// lives. nil — every scenario but perm-mcp-budget — leaves the model unpriced, which is what keeps
// the other scenarios' ¥/积分 numbers at zero and out of their assertions.
func (sb *sandbox) writeConfig(baseURL string, pricing *config.PricingConfig, extraProviders []string, extra string) error {
	dir := filepath.Join(sb.home, ".tachi")
	// The provider goes through the types the app's loader reads (config.ProviderConfig /
	// config.ModelSpec + fileutil.MarshalJSON), so a scenario cannot hand-write an entry the
	// loader would read differently.
	contextWindow, vision := int64(128000), true
	provider := config.ProviderConfig{
		Name:    "smoke",
		Type:    config.ProviderTypeOpenAI,
		Model:   "smoke-model",
		BaseURL: baseURL,
		APIKey:  "smoke-key",
		Spec: config.ModelSpec{
			ContextWindow: &contextWindow,
			// vision: the mock accepts anything, and a provider that cannot see images makes the
			// agent describe them instead of attaching them (agent/vision_fallback.go) — which is
			// correct behaviour but leaves the image routes untestable. Scenarios that never send
			// an image are unaffected: the flag only decides whether image parts are passed on as
			// parts.
			Vision:  &vision,
			Pricing: pricing,
		},
	}
	body, err := fileutil.MarshalJSON(struct {
		Providers []config.ProviderConfig `json:"providers"`
	}{Providers: []config.ProviderConfig{provider}})
	if err != nil {
		return fmt.Errorf("marshal provider fixture: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "provider.smoke.json"), append(body, '\n'), 0o600); err != nil {
		return err
	}
	// The extra entries a scenario asked for, each in a file of its own. Every one points at the
	// same mock — the picker needs a list to filter, not other endpoints to talk to — and the model
	// string carries the name so a filter that matches the MODEL rather than the name is driven too.
	for _, name := range extraProviders {
		entry := provider
		entry.Name = name
		entry.Model = "fixture/" + name
		one, err := fileutil.MarshalJSON(struct {
			Providers []config.ProviderConfig `json:"providers"`
		}{Providers: []config.ProviderConfig{entry}})
		if err != nil {
			return fmt.Errorf("marshal provider fixture %q: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "provider."+name+".json"), append(one, '\n'), 0o600); err != nil {
			return err
		}
	}

	cfg := fmt.Sprintf(`provider: smoke
title_generation: false
language: zh
herdr:
  enabled: false
%s`, extra)
	return os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0o600)
}

// writeMCPConfig points the app at the scenario's OAuth-protected MCP server.
//
// mcp.json rather than config.yaml, because that is where MCP servers live (config.yaml's
// mcp_servers field is ignored on purpose) — and the file is written per run because the
// fixture binds a random loopback port. No "oauth" block: the point of the scenario is the
// path a user actually takes, which is discovering that the server needs authorizing.
func (sb *sandbox) writeMCPConfig(srv *mcpauth.Server) error {
	cfg := map[string]any{
		"servers": []map[string]any{{
			"name": "oauth-fixture",
			"type": "http",
			"url":  srv.MCPURL,
			// A zero timeout would expire the connect context immediately.
			"timeout": "30s",
		}},
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	sb.mcpConfig = data
	return os.WriteFile(filepath.Join(sb.home, ".tachi", "mcp.json"), data, 0o600)
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

// openedByTheApp returns the argument lists the app handed to `open` / xdg-open (one per line,
// space-joined), i.e. the links and paths a driver's click actually asked the system for. Empty
// when nothing was launched — which is the normal case, and the reason most scenarios ignore it.
//
// It reads the artifact TACHI_DEMO_OPEN_LOG points the app at (demoEnv): the app records instead of
// launching, so this file is the only evidence that a click reached the launcher at all.
func (sb *sandbox) openedByTheApp() []string {
	body, err := os.ReadFile(filepath.Join(sb.dir, "opened.txt"))
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(body), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
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

// killAll stops any smoke instance. The executable name is unique to the sandbox, so this
// cannot touch the user's own app.
func killAll() {
	_ = exec.Command("pkill", "-f", smokeExecName).Run()
	// Wait for it to actually go away: the next scenario starts its own app, and a leaked one
	// would keep the sandbox's port/state alive (on macOS a second `open` on a live instance
	// activates it and passes NO environment, which would silently run the next scenario in the
	// previous app).
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
