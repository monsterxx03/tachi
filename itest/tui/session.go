//go:build integration

// Package tui provides the in-process driver for the TUI integration suite
// (docs/2026-07-31-tui-integration-test.md §五): a REAL tui.Model + REAL
// AIAgent running inside a bubbletea Program with injected input/output —
// no tmux, no TTY, no subprocess. Key presses are delivered as semantic
// KeyPressMsg events (p.Send); the cursed renderer's ANSI output is replayed
// through a small virtual terminal (vt.go) so Screen() returns exactly the
// text a real terminal would display — including overwritten cells ("v"
// replacing "~" in a tool row) that a naive ansi.Strip of the diff stream
// could never reconstruct.
//
// The package name shadows the real tui package only inside this directory;
// production code is untouched (tui.Run — the binary's hardcoded
// tea.NewProgram — is deliberately NOT the entry point here).
package tui

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/itest/harness"
	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
	realtui "github.com/monsterxx03/tachi/tui"
)

// TB is the minimal testing surface the driver needs. Satisfied by
// ginkgo.GinkgoT() (which implements Cleanup but, unlike testing.TB, does
// not expose the private method).
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
	TempDir() string
	Cleanup(func())
}

// Option configures a TUI session launch.
type Option func(*options)

type options struct {
	seedFiles map[string]string
	resume    bool
	timeout   time.Duration // spec timeout wired into the program context
}

// WithSeedFiles seeds a fresh working directory (no .tachi.md / .tachi/)
// with the given files and points the Bash tool at it, so tool side effects
// land in an assertable, isolated location (the doc's `ls` → README.md
// pattern). Safe under ginkgo -p because each parallel node is its own
// process and specs run serially within a node.
func WithSeedFiles(files map[string]string) Option {
	return func(o *options) { o.seedFiles = files }
}

// WithResume mirrors `tachi --resume`: a fresh session manager lists the
// home's saved sessions and the model starts in the session-selection state
// (stateSelectingSession).
func WithResume() Option {
	return func(o *options) { o.resume = true }
}

// WithProgramContextTimeout bounds the program's lifetime (tea.WithContext).
// The driver's own waits are the primary timeouts; this is a backstop so a
// wedged program can never hang the suite.
func WithProgramContextTimeout(d time.Duration) Option {
	return func(o *options) { o.timeout = d }
}

// syncedBuffer is a thread-safe output sink: the tea renderer goroutine
// appends ANSI frames while the test goroutine reads Screen().
type syncedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Session drives one in-process TUI. Launch a session, inject keys with
// Type/Enter/Key, and assert on Screen() — either via the Expect/WaitIdle
// conveniences or directly with gomega.Eventually(s.Screen).
type Session struct {
	t      TB
	p      *tea.Program
	out    *syncedBuffer
	vt     *vtScreen
	fedLen int // bytes of out already replayed into vt

	done   chan struct{}
	runErr error

	mock  *mockllm.Server
	home  string
	agent *agent.AIAgent
}

// Launch assembles a session exactly like runTUI (main.go): harness.Config
// loads the --home config, harness.NewAgent builds the AIAgent (permission
// mode TUI), and tui.NewModel wires it into a real Model. The program runs
// with injected input/output/window size; every spec owns its home + mock +
// program, so ginkgo -p parallelism is safe.
//
// The mock must be started and its port baked into home/config.yaml before
// Launch (harness.NewHome renders it).
func Launch(t TB, home string, mock *mockllm.Server, opts ...Option) *Session {
	t.Helper()
	o := &options{}
	for _, opt := range opts {
		opt(o)
	}

	cfg := harness.Config(t, home)
	ai := harness.NewAgent(t, cfg)

	// ALWAYS pin a per-spec working directory so the Bash tool's cwd is a
	// live, isolated dir: with seeds it carries the fixture files, without
	// seeds it is a fresh empty dir. The process-global tools.workingDir
	// must never leak a PREVIOUS spec's t.TempDir — specs run serially in
	// one ginkgo node, and a stale (already-deleted) temp dir makes every
	// Bash exec fail with fork/exec ENOENT.
	tools.SetWorkingDir(harness.SeedWorkDir(t, o.seedFiles))

	var initialSessionList []*session.Session
	if o.resume {
		// runTUI's --resume branch: a fresh manager lists saved sessions and
		// replaces the default one on the agent.
		sm, err := session.NewManager(nil)
		if err != nil {
			t.Fatalf("tui: resume session manager: %v", err)
		}
		sm.SetMaxKeep(cfg.SessionCleanupMaxCount)
		sm.CleanupOldSessions()
		ai.SetSessionManager(sm)
		sessions, err := sm.List()
		if err != nil {
			t.Fatalf("tui: list sessions: %v", err)
		}
		initialSessionList = sessions
	}

	m := realtui.NewModel(realtui.ModelConfig{
		Agent:              ai,
		SystemPrompt:       agent.BuildSystemPrompt(cfg.Language, "", "", cfg.ExtraSystemPrompt),
		ChatOpts:           llm.ChatOptions{MaxTokens: cfg.MaxTokens},
		ProviderInfo:       fmt.Sprintf("%s (%s)", ai.Provider().Name(), ai.Model()),
		Config:             cfg,
		ContextWindow:      ai.ContextWindow(),
		InitialSessionList: initialSessionList,
	})

	out := &syncedBuffer{}
	ctx := context.Background()
	var cancel context.CancelFunc
	if o.timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, o.timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}

	p := tea.NewProgram(m,
		tea.WithInput(strings.NewReader("")), // input comes via p.Send, not the terminal
		tea.WithOutput(out),
		tea.WithWindowSize(120, 40),
		tea.WithEnvironment([]string{"TERM=xterm-256color"}),
		tea.WithoutSignalHandler(),
		tea.WithContext(ctx),
	)

	s := &Session{
		t:     t,
		p:     p,
		out:   out,
		vt:    newVTScreen(120, 40),
		done:  make(chan struct{}),
		mock:  mock,
		home:  home,
		agent: ai,
	}
	go func() {
		defer close(s.done)
		_, s.runErr = p.Run()
	}()

	// Teardown regardless of scenario outcome (doc §七): quit the program
	// (bounded wait), cancel the program context as a backstop, then close
	// the agent + its MCP manager. mock.Close is the caller's job (shared
	// with the harness helpers).
	t.Cleanup(func() {
		s.p.Quit()
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
		}
		cancel()
		ai.Close()
		if mm := ai.Config.MCPManager; mm != nil {
			mm.Close()
		}
	})
	return s
}

// Type injects printable text one character at a time (each char is a
// KeyPressMsg with Text set — the input area appends them like typing).
func (s *Session) Type(text string) {
	for _, r := range text {
		s.p.Send(tea.KeyPressMsg{Text: string(r)})
	}
}

// Enter presses the Enter key (submits the input / confirms).
func (s *Session) Enter() {
	s.p.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
}

// Command types a slash command and submits it. Slash input opens the
// completion popup, whose Enter handler ACCEPTS the highlighted completion
// (production behavior, input.go) — a real user presses Enter once to accept
// the popup, once to send. Command() encodes exactly that two-Enter flow;
// when the popup never opened (no matching completion) the second Enter
// submits an empty input, which is a no-op — so the flow is always safe.
func (s *Session) Command(text string) {
	s.Type(text)
	s.Enter() // accept the completion popup (Enter selects the highlighted match)
	s.Enter() // submit
}

// Key presses a named key. Recognized names: enter, esc, tab, up, down,
// left, right, space, backspace, home, end, pgup, pgdown, ctrl+c, ctrl+o,
// ctrl+k/j/p/n/d/u — or a single printable character (same as Type).
// Unknown names fail the spec: a typo'd key must never silently no-op.
func (s *Session) Key(seq string) {
	k, ok := keyFor(seq)
	if !ok {
		s.t.Fatalf("tui: unknown key %q", seq)
	}
	s.p.Send(k)
}

func keyFor(seq string) (tea.KeyPressMsg, bool) {
	switch seq {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}, true
	case "esc", "escape":
		return tea.KeyPressMsg{Code: tea.KeyEsc}, true
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}, true
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}, true
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}, true
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}, true
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}, true
	case "space":
		return tea.KeyPressMsg{Text: " "}, true
	case "backspace":
		return tea.KeyPressMsg{Code: tea.KeyBackspace}, true
	case "home":
		return tea.KeyPressMsg{Code: tea.KeyHome}, true
	case "end":
		return tea.KeyPressMsg{Code: tea.KeyEnd}, true
	case "pgup":
		return tea.KeyPressMsg{Code: tea.KeyPgUp}, true
	case "pgdown":
		return tea.KeyPressMsg{Code: tea.KeyPgDown}, true
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}, true
	case "ctrl+o":
		return tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl}, true
	case "ctrl+k":
		return tea.KeyPressMsg{Code: 'k', Mod: tea.ModCtrl}, true
	case "ctrl+j":
		return tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl}, true
	case "ctrl+p":
		return tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl}, true
	case "ctrl+n":
		return tea.KeyPressMsg{Code: 'n', Mod: tea.ModCtrl}, true
	case "ctrl+d":
		return tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl}, true
	case "ctrl+u":
		return tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl}, true
	}
	if len(seq) == 1 {
		return tea.KeyPressMsg{Text: seq}, true
	}
	return tea.KeyPressMsg{}, false
}

// Screen returns the CURRENT screen text (rows joined by newlines, trailing
// blanks trimmed) — what a real terminal would show right now. The ANSI
// stream is replayed incrementally into the virtual terminal; calls are
// safe from the test goroutine only.
func (s *Session) Screen() string {
	raw := s.out.String()
	if len(raw) > s.fedLen {
		s.vt.feed(raw[s.fedLen:])
		s.fedLen = len(raw)
	}
	return s.vt.Text()
}

// DebugRaw returns the raw renderer output accumulated so far. Debugging aid
// only — assertions must go through Screen().
func (s *Session) DebugRaw() string { return s.out.String() }

// RunError returns the tea.Program's Run error, if any (e.g. a model panic
// surfaced as ErrProgramCrashed). Meaningful only after the program exits.
func (s *Session) RunError() error { return s.runErr }

// Expect polls Screen() until it contains the given substring or the
// timeout elapses, failing the spec with a screen + mock dump on timeout.
func (s *Session) Expect(text string, timeout time.Duration) {
	s.t.Helper()
	s.waitFor(timeout, func() bool { return strings.Contains(s.Screen(), text) },
		"screen never contained %q", text)
}

// WaitIdle waits until the mock has seen at least n requests, the TURN is over,
// and the screen shows the idle statusbar.
//
// The turn's own state (agent.TurnInFlight) is the anchor that matters, and it is
// not a formality: the SCREEN lags. bubbletea renders at ~60fps, and the mock's
// request counter rises on the mock's goroutine the moment a request arrives — so
// "request counted + screen still showing the last idle frame" is a real state, and
// a wait built on those two alone returns while the turn is still streaming. A spec
// that then types (its input goes into the pending queue instead of a new turn) or
// quits the program (Stop mid-turn: the turn's writes land after the scan that was
// supposed to see them) fails randomly, on a loaded machine only. TurnInFlight is
// false only once the run goroutine has left, i.e. after every write it makes.
//
// The screen check stays: it is what proves the UI came back to a state the spec can
// assert against (a modal — confirmation, ask-user — is not the same as idle).
func (s *Session) WaitIdle(n int, timeout time.Duration) {
	s.t.Helper()
	s.waitFor(timeout, func() bool {
		return s.mock.RequestCount() >= n &&
			!s.agent.TurnInFlight() &&
			strings.Contains(s.Screen(), "● tachi")
	}, "never reached idle (%d/%d requests)", s.mock.RequestCount(), n)
}

// Stop quits the program and waits for it to exit (bounded), then for the turn to
// drain (bounded). The cleanup registered in Launch does the same, so Stop is
// optional — call it when a scenario needs the renderer to finish before asserting
// something on the final screen.
//
// Draining matters for the scenario that closes the app and reopens it on the same
// --home (--resume): quitting the PROGRAM does not stop the agent's run goroutine —
// in the real binary the process exits and takes it along, here the process lives on.
// A turn still writing its session would race the reopen's session list, so a closed
// session is one whose turn is over. Best-effort: a spec mid-turn (a paused stream,
// say) is not an error, its own waits decide.
func (s *Session) Stop() {
	s.p.Quit()
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.agent.TurnInFlight() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
}

func (s *Session) waitFor(timeout time.Duration, cond func() bool, failMsg string, failArgs ...any) {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	raw := s.out.String()
	tail := raw
	const maxTail = 1500
	if len(tail) > maxTail {
		tail = tail[len(tail)-maxTail:]
	}
	s.t.Fatalf("tui: %s\n--- screen ---\n%s\n--- session dir ---\n%s\n--- mock requests: %d, mock error: %v\n--- raw tail ---\n%q",
		fmt.Sprintf(failMsg, failArgs...), s.Screen(), s.sessionDirSummary(), s.mock.RequestCount(), s.mock.Error(), tail)
}

// sessionDirSummary describes the home's session directory — one line per session,
// with the size of each file in it — for the failure dump. A session spec that fails
// (--resume found nothing, a turn recorded nothing) is asserting about disk state,
// and the screen cannot tell "the session is missing" from "the session is there but
// unparsable" or "the directory was never written". That distinction IS the
// diagnosis, so it belongs in the dump instead of in a re-run.
func (s *Session) sessionDirSummary() string {
	root := filepath.Join(s.home, "session")
	dirs, err := os.ReadDir(root)
	if err != nil {
		return fmt.Sprintf("%s: %v", root, err)
	}
	if len(dirs) == 0 {
		return root + ": EMPTY (no session dirs)"
	}
	var b strings.Builder
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		b.WriteString(d.Name() + ":")
		files, err := os.ReadDir(filepath.Join(root, d.Name()))
		if err != nil {
			b.WriteString(" " + err.Error() + "\n")
			continue
		}
		for _, f := range files {
			size := int64(-1)
			if info, err := f.Info(); err == nil {
				size = info.Size()
			}
			fmt.Fprintf(&b, " %s=%d", f.Name(), size)
		}
		b.WriteString("\n")
	}
	return b.String()
}
