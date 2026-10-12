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
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/itest/mcpauth"
	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/monsterxx03/tachi/session"
)

// driverContactWindow is how long the runner waits for the driver to show ANY sign of life before
// calling the run dead (see sink.contact). Longer than the app's own injection window (10s,
// desktop/demo.go) so a page that takes its time still counts as alive, and far shorter than the
// per-scenario budget it stands in for.
const driverContactWindow = 15 * time.Second

// mcpConsentDelay is how long the MCP OAuth fixture plays at being a consent screen before it
// redirects back (see mcpauth.WithAuthorizeDelay). It exists so the scenario can observe the
// state the flow spends its real time in — a panel saying "open this URL" — and it is short
// enough to be invisible next to the app's own startup cost.
const mcpConsentDelay = 4 * time.Second

func main() {
	var (
		appPath = flag.String("app", defaultAppPath(), "the built app (bundle or binary) to test")
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
	start := time.Now()
	fmt.Printf("── %s\n", sc.name)
	dir := filepath.Join(root, sc.name)
	sb, err := newSandbox(dir, srcApp)
	if err != nil {
		return report(sc.name, nil, []Line{{Label: "sandbox", OK: false, Detail: err.Error()}}, time.Since(start), verbose)
	}
	if err := sb.seedFiles(sc.files); err != nil {
		return report(sc.name, nil, []Line{{Label: "fixtures", OK: false, Detail: err.Error()}}, time.Since(start), verbose)
	}
	if sc.gitInit {
		if err := sb.gitInit(); err != nil {
			return report(sc.name, nil, []Line{{Label: "git init", OK: false, Detail: err.Error()}}, time.Since(start), verbose)
		}
	}
	if err := sb.seedExtraRoots(sc.extraRoots); err != nil {
		return report(sc.name, nil, []Line{{Label: "extra roots", OK: false, Detail: err.Error()}}, time.Since(start), verbose)
	}
	// The project fixture comes BEFORE the sessions: binding one of them needs its id.
	projectID := ""
	if sc.project != nil {
		id, err := sb.seedProject(sc.project)
		if err != nil {
			return report(sc.name, nil, []Line{{Label: "project fixture", OK: false, Detail: err.Error()}}, time.Since(start), verbose)
		}
		projectID = id
		// The UNBOUND session first, so the member below is the newest one: a fixture has no
		// "current session", and the app opens the newest (ListSessions sorts by CreatedAt).
		// Seeding them the other way round left the driver asserting on the wrong session —
		// which looked exactly like "the project does not drive its member".
		if err := sb.seedSession("无项目会话", nil, nil, "", "", "", false); err != nil {
			return report(sc.name, nil, []Line{{Label: "unbound session fixture", OK: false, Detail: err.Error()}}, time.Since(start), verbose)
		}
	}
	// A LONG list, for the sidebar's row window (see scenario.seedLoose): the window only does
	// anything with more rows than it shows, and no driver can create a hundred sessions one ⌘N at a
	// time. Seeded here — before 冒烟会话 below — so the app still opens that one rather than the last
	// bulk row.
	//
	// Titled in seed order, which is also age order (each New stamps CreatedAt/UpdatedAt now): the
	// driver needs to name one that the window cannot possibly be showing, and 「批量会话 01」 — the
	// oldest, so the LAST row of the bucket — is it.
	if sc.seedLoose > 0 {
		if err := sb.seedSessions(sc.seedLoose, func(i int) string { return fmt.Sprintf("批量会话 %02d", i) }); err != nil {
			return report(sc.name, nil, []Line{{Label: "bulk sessions fixture", OK: false, Detail: err.Error()}}, time.Since(start), verbose)
		}
	}
	// An EXTRA session, seeded BEFORE the restored one so the app still opens the newest (it is
	// the one a driver gets by clicking a row it has never opened — see seedSecond).
	if len(sc.seedSecond) > 0 {
		if err := sb.seedSession("另一个会话", nil, sc.seedSecond, "", "", "", false); err != nil {
			return report(sc.name, nil, []Line{{Label: "second session fixture", OK: false, Detail: err.Error()}}, time.Since(start), verbose)
		}
	}
	// A compacted conversation whose pre-compaction half is GONE, seeded before the session the app
	// opens (see seedOrphanChain). Nothing a driver can do produces this shape: it needs a session
	// record whose link points outside the list.
	if sc.orphanChain {
		if err := sb.seedOrphanChain(); err != nil {
			return report(sc.name, nil, []Line{{Label: "orphan chain fixture", OK: false, Detail: err.Error()}}, time.Since(start), verbose)
		}
	}
	if err := sb.seedSession("冒烟会话", sc.extraRoots, sc.seedMessages, projectID, sc.sessionKind, sc.sessionSourceNodeID, sc.sessionHeldByTask); err != nil {
		return report(sc.name, nil, []Line{{Label: "session fixture", OK: false, Detail: err.Error()}}, time.Since(start), verbose)
	}

	mock := mockllm.NewServer(mockllm.WithProtocol(mockllm.ProtocolOpenAI))
	mock.Script(sc.steps...)
	// The peers a scenario declares: the answering one is served by the runner itself, so its URL is
	// only known here — which is why the `peers:` block is rendered now and appended to the
	// scenario's own config rather than hand-written in it.
	peersYAML, peerFixtures, err := startPeerFixtures(sc.peers)
	if err != nil {
		return report(sc.name, nil, []Line{{Label: "peer fixture", OK: false, Detail: err.Error()}}, time.Since(start), verbose)
	}
	for _, f := range peerFixtures {
		defer f.Close()
	}
	if err := sb.writeConfig(mock.BaseURL(), sc.pricing, sc.config+peersYAML); err != nil {
		return report(sc.name, nil, []Line{{Label: "config", OK: false, Detail: err.Error()}}, time.Since(start), verbose)
	}

	// The MCP OAuth fixture lives in the RUNNER, not in the sandbox: it is an HTTP server
	// the app talks to over loopback, and its port is only known once it is up — which is
	// why the sandbox's mcp.json is written here rather than checked in.
	var mcpFixture *mcpauth.Server
	if sc.mcpOAuth {
		// The consent delay is what makes the WAITING state observable: a flow that
		// completes in microseconds can only be asserted on after the fact, so the driver
		// would never see the card, the URL to open, or the paste box that the real flow
		// spends most of its time showing.
		mcpFixture, err = mcpauth.New(mcpauth.WithAuthorizeDelay(mcpConsentDelay))
		if err != nil {
			return report(sc.name, nil, []Line{{Label: "mcp fixture", OK: false, Detail: err.Error()}}, time.Since(start), verbose)
		}
		defer mcpFixture.Close()
		if err := sb.writeMCPConfig(mcpFixture); err != nil {
			return report(sc.name, nil, []Line{{Label: "mcp.json", OK: false, Detail: err.Error()}}, time.Since(start), verbose)
		}
		sb.browserFetch = true
	}
	// A plain mcp.json for the scenarios that drive the settings page's MCP editor. Written here
	// rather than in sandbox.go because it is the scenario's own fixture text, and recorded on the
	// sandbox so the Go half can compare against what was seeded.
	if sc.mcpServersJSON != "" {
		sb.mcpConfig = []byte(sc.mcpServersJSON)
		if err := os.WriteFile(filepath.Join(sb.home, ".tachi", "mcp.json"), sb.mcpConfig, 0o600); err != nil {
			return report(sc.name, nil, []Line{{Label: "mcp.json", OK: false, Detail: err.Error()}}, time.Since(start), verbose)
		}
	}
	if sc.voiceText != "" {
		sb.voiceText = sc.voiceText
	}

	snk, err := newSink()
	if err != nil {
		return report(sc.name, nil, []Line{{Label: "sink", OK: false, Detail: err.Error()}}, time.Since(start), verbose)
	}
	defer snk.close()
	if _, err := sb.writeDriver(driversDir, sc.name, snk.url); err != nil {
		return report(sc.name, nil, []Line{{Label: "driver", OK: false, Detail: err.Error()}}, time.Since(start), verbose)
	}

	if err := sb.launch(); err != nil {
		return report(sc.name, nil, []Line{{Label: "launch", OK: false, Detail: err.Error()}}, time.Since(start), verbose)
	}

	// The driver's own budget only starts once it is known to be running: a sink that has heard
	// NOTHING after this window means the driver never ran at all — the injection was lost while
	// the page was still loading (its payload is re-issued for 10s, see desktop/demo.go), or the
	// window never came up. Both are already decided, and waiting out the whole per-scenario
	// budget for them is a minute and a half of silence, so the wait ends here and the report
	// names it. Measured twice on a machine that was busy compiling: the app was up (its session
	// directory created) and not one assertion ever arrived.
	// A launch that produced NOTHING AT ALL is retried once before the scenario is called dead.
	// `open` on a bundle LaunchServices still counts as running is a no-op that reports success —
	// and the harness kills the previous scenario's app moments earlier — so the app simply never
	// appears: measured as the fourth scenario of a run, its sandbox left without even a state
	// directory while the three before it were fine. One retry (logged) is the whole recovery; a
	// real startup crash still fails, with the same named line as before.
	const launchAttempts = 2
	started := false
	for attempt := 1; attempt <= launchAttempts && !started; attempt++ {
		if snk.contact(driverContactWindow) {
			started = true
			break
		}
		if attempt == launchAttempts {
			break
		}
		fmt.Printf("   · driver 未出现（第 %d 次启动）：重新启动 app\n", attempt)
		killAll()
		if err := sb.launch(); err != nil {
			break
		}
	}
	if !started {
		if dom := snk.domSnapshot(); len(dom) > 0 {
			_ = os.WriteFile(filepath.Join(dir, "dom.html"), dom, 0o644)
		}
		killAll()
		return report(sc.name, nil, []Line{{Label: "driver 载入", OK: false,
			Detail: fmt.Sprintf("%s 内没有任何断言到达：驱动没有跑起来（注入丢失，或窗口没起来）；"+
				"app 到底起没起，看 home/.tachi 与 debug.log", driverContactWindow)}},
			time.Since(start), verbose)
	}
	rest := timeout - driverContactWindow
	if rest < time.Second {
		rest = time.Second
	}
	res, reported := snk.wait(rest)
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
		mcp:              mcpFixture,
		mcpConfigBefore:  string(sb.mcpConfig),
		peerCardRequests: peerCardReads(peerFixtures),
		opened:           sb.openedByTheApp(),
	}
	if sc.after != nil {
		sc.after(ctx)
	}
	return report(sc.name, &res, lines, time.Since(start), verbose)
}

// report prints one scenario's outcome; the boolean is "passed". elapsed is the whole
// scenario — sandbox prep, launch, the driver, the Go-side checks — because that is what
// the suite's wall clock is made of, and a scenario's cost is invisible without it.
func report(name string, res *Result, lines []Line, elapsed time.Duration, verbose bool) bool {
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
	fmt.Printf("   %s %s（%s）\n\n", mark(ok), summary(ok, lines), elapsed.Round(100*time.Millisecond))
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
//
// extraRoots become the session's additional workspace roots, through the SAME store API the
// app uses. They have to be seeded here because a root set is the one piece of scenario state a
// driver cannot build: the UI's only way in is a NATIVE directory picker, which a scripted run
// cannot click.
func (sb *sandbox) seedSession(title string, extraRoots map[string]map[string]string, seedMessages []session.Message, projectID, kind, sourceNodeID string, heldByTask bool) error {
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
	cur := mgr.Current()
	// A member session: bound by id, and its own WorkingDir stays the sandbox work dir — the
	// STALE snapshot the project's roots override on every read (design §3.2). Seeding the
	// project's own path here would make the fixture unable to tell the two apart.
	cur.ProjectID = projectID
	// A SUBSYSTEM's session (see scenario.sessionKind). Written on the record because that is
	// where the fact lives: the sidebar labels from it, and the agent's own entry refuses a local
	// turn on it whatever the frontend does.
	cur.Kind = kind
	// WHO opened it, when a subsystem did: the caller identity out of its certificate, which is what
	// the sidebar labels as the source (it resolves against this machine's pinned peers).
	cur.SourceNodeID = sourceNodeID
	if len(extraRoots) > 0 {
		for _, name := range sortedKeys(extraRoots) {
			cur.AdditionalDirs = append(cur.AdditionalDirs, filepath.Join(sb.dir, name))
		}
	}
	// Seeded records land BEFORE the app starts, so their first render is a read from disk —
	// the only way to stage a restart (see scenario.seedMessages).
	for i := range seedMessages {
		msg := seedMessages[i]
		if msg.Timestamp.IsZero() {
			msg.Timestamp = time.Now()
		}
		if err := store.AppendMessage(cur.ID, &msg); err != nil {
			return err
		}
	}
	if len(extraRoots) > 0 || len(seedMessages) > 0 || projectID != "" || kind != "" || sourceNodeID != "" {
		if err := mgr.UpdateMeta(cur); err != nil {
			return err
		}
	}
	// A running execution's hold, when the scenario asks for it: a retention record, written the
	// way the node writes it (owner "node", ReasonTask, no deadline — a running task has none).
	// The app reads it on the first render, which is the point: this is the state that refuses a
	// delete and offers the owner the escape hatch.
	if heldByTask {
		retention, err := session.NewRetention(filepath.Join(sb.home, ".tachi", "session"), nil)
		if err != nil {
			return err
		}
		if err := retention.Acquire(session.Hold{
			SessionID: cur.ID, Owner: "node", Reason: session.ReasonTask, Ref: "task-smoke",
		}); err != nil {
			return err
		}
	}
	return nil
}

// seedSessions seeds n project-less sessions in ONE pass: one store and one manager for the whole
// set, because the sidebar-window fixture needs a hundred of them and a per-session NewFileStore
// would pay that construction a hundred times. Titles are the driver's handle on a specific row
// (see scenario.seedLoose), so it hands in the naming.
func (sb *sandbox) seedSessions(n int, title func(i int) string) error {
	store, err := session.NewFileStore(filepath.Join(sb.home, ".tachi", "session"))
	if err != nil {
		return err
	}
	mgr := session.NewManagerWithStore(store, nil)
	for i := 1; i <= n; i++ {
		if _, err := mgr.New("smoke", sb.work); err != nil {
			return err
		}
		// SetTitle persists on its own (UpdateMeta inside), which is the whole record these
		// fixtures need: no project, no roots, no transcript.
		mgr.SetTitle(title(i))
	}
	return nil
}

// orphanChainParentID is the id of the pre-compaction session the orphan-chain fixture points at —
// never written to disk, so the fixture is exactly "a link whose other end is not in the list".
const orphanChainParentID = "2026-01-01-000000-gone"

// seedOrphanChain writes the one session shape a driver cannot reach: a conversation whose
// pre-compaction half is no longer in the list. The link and the parent's TITLE are both on the
// child's own record — that is what FinalizeCompact writes — so the fixture writes them through the
// same store API and does NOT create the parent. Deleted-by-hand and swept-by-max_keep both end up
// here, and the row has to keep saying the conversation was compacted either way.
//
// Seeded BEFORE the session the app opens (main.go's order), so the driver finds this row in the
// sidebar rather than staring at it.
func (sb *sandbox) seedOrphanChain() error {
	store, err := session.NewFileStore(filepath.Join(sb.home, ".tachi", "session"))
	if err != nil {
		return err
	}
	mgr := session.NewManagerWithStore(store, nil)
	if _, err := mgr.New("smoke", sb.work); err != nil {
		return err
	}
	mgr.SetTitle("压缩过的会话")
	cur := mgr.Current()
	cur.CompactedParentID = orphanChainParentID
	cur.CompactedParentTitle = "上一次对话"
	return mgr.UpdateMeta(cur)
}

// projectRootDir is the directory the project fixture's primary root lives in, under the
// sandbox. Named once, because the seeder and the probes that assert on the path must agree.
const projectRootDir = "project-root"

// seedProject writes the project fixture: the primary directory with its files, one directory
// per additional root, and projects.json — the same shape desktop/projects.go reads and writes
// (a slice under "projects", camelCase keys). It returns the project ID the session is bound by.
//
// Only the DIRECTORY under the sandbox is created here; the project's own validity rules (a
// primary that exists, no wide roots) are the desktop's, and this fixture deliberately sits
// inside them so the seeded project drives its member from the first render.
func (sb *sandbox) seedProject(p *projectSeed) (string, error) {
	primary := filepath.Join(sb.dir, projectRootDir)
	if err := os.MkdirAll(primary, 0o755); err != nil {
		return "", err
	}
	for rel, content := range p.files {
		full := filepath.Join(primary, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return "", err
		}
	}
	var additional []string
	for _, name := range sortedKeys(p.extraRoots) {
		dir := filepath.Join(sb.dir, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		for rel, content := range p.extraRoots[name] {
			full := filepath.Join(dir, rel)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
				return "", err
			}
		}
		additional = append(additional, dir)
	}

	// The sandbox's own shape of the file (the desktop's project/projectFile types are in another
	// module and cannot be imported here) — but the JSON is what matters, and it is the same.
	const id = "smoke-project"
	const emptyID = "smoke-project-empty"
	type fileProject struct {
		ID             string    `json:"id"`
		Name           string    `json:"name"`
		WorkingDir     string    `json:"workingDir"`
		AdditionalDirs []string  `json:"additionalDirs,omitempty"`
		CreatedAt      time.Time `json:"createdAt"`
		UpdatedAt      time.Time `json:"updatedAt"`
	}
	now := time.Now()
	projects := []fileProject{{
		ID: id, Name: p.name, WorkingDir: primary, AdditionalDirs: additional,
		CreatedAt: now, UpdatedAt: now,
	}}
	if p.emptyName != "" {
		// Older, so the sidebar (newest first) lists it after the member's project. Its directory
		// is the sandbox work dir: nothing has to be created for it, and an empty project's roots
		// are never resolved (no member session exists to resolve them for).
		projects = append(projects, fileProject{
			ID: emptyID, Name: p.emptyName, WorkingDir: sb.work,
			CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
		})
	}
	body, err := json.MarshalIndent(struct {
		Projects []fileProject `json:"projects"`
	}{Projects: projects}, "", "  ")
	if err != nil {
		return "", err
	}
	base := filepath.Join(sb.home, ".tachi")
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(base, "projects.json"), body, 0o644); err != nil {
		return "", err
	}
	return id, nil
}

// seedExtraRoots creates the additional workspace roots a scenario declares: one directory per
// entry under the sandbox, holding that entry's files. They live beside the primary work dir
// (not inside it), so a relative path cannot reach the other root by accident — and a scenario
// can still reach it from the work dir as "../<name>/…", which is how a scripted bash command
// touches a root whose absolute path it cannot know.
func (sb *sandbox) seedExtraRoots(roots map[string]map[string]string) error {
	for _, name := range sortedKeys(roots) {
		dir := filepath.Join(sb.dir, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		for rel, content := range roots[name] {
			p := filepath.Join(dir, rel)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

// sortedKeys keeps the seeded root ORDER stable (the primary is fixed; the additional ones
// follow in this order), so a scenario's assertions do not depend on map iteration.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
