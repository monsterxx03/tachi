// Command desktop-smoke runs the desktop app's UI regression suite.
//
//	go run ./itest/desktop                      # every scenario
//	go run ./itest/desktop -run send-now        # one of them
//
// Why this exists: unit tests and the headless itest suites cannot see whether a Wails
// binding's result reaches a card, whether a queue interruption lands its reply, or
// whether a plan's step statuses move on screen. Those are exactly the bugs this app has
// produced, and the only way to catch them is to drive the real window.
//
// Each scenario runs in a throwaway sandbox: a private copy of the app bundle (renamed so
// no pkill can reach the user's own Tachi), an isolated HOME, a scripted mock model, and
// a driver that does what a user would. The driver asserts what the UI shows and posts its
// verdict to a loopback sink; the runner asserts what actually happened (the prompt the
// model received, the files written) and turns both into an exit code.
//
// Artifacts per scenario stay in the sandbox (the page as the verdict found it, the mock's
// transcript, the driver source); on failure the path is printed for inspection.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/monsterxx03/tachi/session"
)

func main() {
	var (
		appPath = flag.String("app", "desktop/bin/Tachi.app", "the built app bundle to test")
		run     = flag.String("run", "", "comma-separated scenario names (default: all)")
		keep    = flag.Bool("keep", false, "keep the sandbox after a successful run")
		timeout = flag.Duration("timeout", 90*time.Second, "per-scenario budget for the driver")
		verbose = flag.Bool("v", false, "print every assertion line, not just failures")
	)
	flag.Parse()

	driversDir, err := driverDir()
	if err != nil {
		fatal("%v", err)
	}

	// Preflight. A live instance is the one failure mode that silently corrupts a run:
	// `open` would merely activate it, passing no env — so the scenario would appear to
	// pass while actually driving the previous app (or worse, the user's).
	killAll()
	if smokeRunning() {
		fatal("a %s instance is still running; kill it first (pkill -f %s)", smokeExecName, smokeExecName)
	}

	sandboxDir, err := os.MkdirTemp("", "tachi-desktop-smoke-")
	if err != nil {
		fatal("sandbox: %v", err)
	}
	selected := selectScenarios(scenarios(), *run)
	if len(selected) == 0 {
		fatal("no scenario matched -run=%q", *run)
	}

	start := time.Now()
	failed := 0
	fmt.Printf("desktop smoke: %d scenario(s), sandbox %s\n\n", len(selected), sandboxDir)
	for _, sc := range selected {
		if runScenario(sc, sandboxDir, *appPath, driversDir, *timeout, *verbose) {
			continue
		}
		failed++
	}

	fmt.Printf("\n%s (%s)\n", verdict(failed, len(selected)), time.Since(start).Round(time.Second))
	if failed > 0 {
		fmt.Printf("artifacts: %s\n", sandboxDir)
		os.Exit(1)
	}
	if !*keep {
		_ = os.RemoveAll(sandboxDir)
	}
}

// runScenario sets up one isolated app run and reports whether it passed.
func runScenario(sc scenario, root, srcApp, driversDir string, timeout time.Duration, verbose bool) bool {
	fmt.Printf("── %s\n", sc.name)
	dir := filepath.Join(root, sc.name)
	sb, err := newSandbox(dir, srcApp)
	if err != nil {
		return report(sc.name, nil, []Line{{Label: "sandbox", OK: false, Detail: err.Error()}}, verbose)
	}
	if err := sb.seedFiles(sc.files); err != nil {
		return report(sc.name, nil, []Line{{Label: "fixtures", OK: false, Detail: err.Error()}}, verbose)
	}
	if sc.gitInit {
		if err := sb.gitInit(); err != nil {
			return report(sc.name, nil, []Line{{Label: "git init", OK: false, Detail: err.Error()}}, verbose)
		}
	}
	if err := sb.seedSession("冒烟会话"); err != nil {
		return report(sc.name, nil, []Line{{Label: "session fixture", OK: false, Detail: err.Error()}}, verbose)
	}

	mock := mockllm.NewServer(mockllm.WithProtocol(mockllm.ProtocolOpenAI))
	mock.Script(sc.steps...)
	if err := sb.writeConfig(mock.BaseURL()); err != nil {
		return report(sc.name, nil, []Line{{Label: "config", OK: false, Detail: err.Error()}}, verbose)
	}

	snk, err := newSink()
	if err != nil {
		return report(sc.name, nil, []Line{{Label: "sink", OK: false, Detail: err.Error()}}, verbose)
	}
	defer snk.close()
	if _, err := sb.writeDriver(driversDir, sc.name, snk.url); err != nil {
		return report(sc.name, nil, []Line{{Label: "driver", OK: false, Detail: err.Error()}}, verbose)
	}

	if err := sb.launch(); err != nil {
		return report(sc.name, nil, []Line{{Label: "launch", OK: false, Detail: err.Error()}}, verbose)
	}

	res, reported := snk.wait(timeout)
	// The page as the verdict found it: the artifact that always exists, and the one that
	// carries the driver's own lines plus everything it asserted about.
	if dom := snk.domSnapshot(); len(dom) > 0 {
		_ = os.WriteFile(filepath.Join(dir, "dom.html"), dom, 0o644)
	}
	killAll()

	lines := res.Lines
	if !reported {
		// The driver reported nothing: keep whatever it asserted before going quiet —
		// "how far did it get" is the first question, and the answer is usually the last
		// line in front of the one that hung.
		lines = snk.lines()
		lines = append(lines, Line{Label: "driver 在预算内完成", OK: false,
			Detail: fmt.Sprintf("等了 %s 没收到结果（下面是它走到的地方）", timeout)})
	} else if res.Error != "" {
		lines = append(lines, Line{Label: "driver 抛异常", OK: false, Detail: res.Error})
	}
	// Everything the page logged, in BOTH cases: a console error while a driver is still
	// making its way through the assertions is usually the "why" of a strange failure
	// (a binding that rejected, a state that never arrived) — and dropping it unless the
	// driver timed out kept exactly that answer out of the report.
	for _, c := range snk.consoleLines() {
		lines = append(lines, Line{Label: "console", OK: true, Detail: c})
	}
	if !res.Done && reported {
		lines = append(lines, Line{Label: "driver 跑到了结尾", OK: false,
			Detail: "结果是在收尾之前发出来的，后面的断言没有跑"})
	}

	// The Go side of the evidence: what the model was asked and what landed on disk.
	reqDump := filepath.Join(dir, "mock-requests.txt")
	_ = dumpMockRequests(reqDump, mock.Requests())
	ctx := &checkCtx{
		scenario: sc.name, dir: dir, home: sb.home, work: sb.work,
		requests: mock.Requests(), mockErr: mock.Error(), lines: &lines,
	}
	if sc.after != nil {
		sc.after(ctx)
	}
	return report(sc.name, &res, lines, verbose)
}

// report prints one scenario's outcome; the boolean is "passed".
func report(name string, res *Result, lines []Line, verbose bool) bool {
	ok := true
	for _, l := range lines {
		if !l.OK {
			ok = false
		}
		if verbose || !l.OK {
			fmt.Printf("   %s %s%s\n", mark(l.OK), l.Label, detail(l.Detail))
		}
	}
	if res != nil && res.Error != "" {
		ok = false
	}
	fmt.Printf("   %s %s\n\n", mark(ok), summary(ok, lines))
	return ok
}

func mark(ok bool) string {
	if ok {
		return "·"
	}
	return "✗"
}

func detail(d string) string {
	if strings.TrimSpace(d) == "" {
		return ""
	}
	return "  — " + d
}

func summary(ok bool, lines []Line) string {
	if ok {
		return fmt.Sprintf("PASS（%d 项断言）", len(lines))
	}
	n := 0
	for _, l := range lines {
		if !l.OK {
			n++
		}
	}
	return fmt.Sprintf("FAIL（%d/%d 项断言失败）", n, len(lines))
}

func verdict(failed, total int) string {
	if failed == 0 {
		return fmt.Sprintf("PASS: %d/%d 场景通过", total, total)
	}
	return fmt.Sprintf("FAIL: %d/%d 场景失败", failed, total)
}

func selectScenarios(all []scenario, only string) []scenario {
	if strings.TrimSpace(only) == "" {
		return all
	}
	want := map[string]bool{}
	for _, name := range strings.Split(only, ",") {
		want[strings.TrimSpace(name)] = true
	}
	var out []scenario
	for _, sc := range all {
		if want[sc.name] {
			out = append(out, sc)
		}
	}
	return out
}

// driverDir locates itest/desktop/drivers relative to the source file, so the command
// works from any working directory.
func driverDir() (string, error) {
	// The runner is always built from source in this repo; locate the dir next to it.
	exe, err := os.Executable()
	if err == nil {
		if dir := filepath.Join(filepath.Dir(exe), "drivers"); dirExists(dir) {
			return dir, nil
		}
	}
	for _, dir := range []string{
		filepath.Join("itest", "desktop", "drivers"),
		filepath.Join("..", "..", "itest", "desktop", "drivers"),
	} {
		if dirExists(dir) {
			return dir, nil
		}
	}
	return "", fmt.Errorf("drivers/ not found (run from the repo root: go run ./itest/desktop)")
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "desktop-smoke: "+format+"\n", args...)
	os.Exit(2)
}

// seedSession writes the session fixture the app restores on startup: an empty
// conversation with a known working directory. Without it a LaunchServices-launched app
// would create its own session with cwd "/", and every path a scenario asserts on
// (plans, @-references) would land somewhere else.
func (sb *sandbox) seedSession(title string) error {
	// The store's base dir IS the sessions directory (each session is <dir>/<id>/),
	// not the config dir — pointing it one level up would put the fixture where the app
	// never looks, and startup would quietly create its own session instead.
	store, err := session.NewFileStore(filepath.Join(sb.home, ".tachi", "session"))
	if err != nil {
		return err
	}
	mgr := session.NewManagerWithStore(store, nil)
	if _, err := mgr.New("smoke", sb.work); err != nil {
		return err
	}
	mgr.SetTitle(title)
	return nil
}
