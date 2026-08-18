package tui

import (
	"errors"
	"math"
	"strings"
	"syscall"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// processAlive reports whether a process with the given PID exists.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// ---- helpers ----

// testModel creates a minimal Model for testing state transitions.
// It has no real agent, so sendMessage / agent interactions won't work,
// but key handling and state manipulation are fully testable.
func testModel() *Model {
	m := &Model{
		statusbar: NewStatusBar("test/model", 128_000),
		chatview:  NewChatView(),
		input:     NewInputArea(10, "", nil),
		state:     stateIdle,
		width:     100,
		height:    40,
	}
	m.layout()
	return m
}

// testModelStreaming returns a model in the streaming state with a closed
// eventCh so that nextEvent() commands resolve to streamDoneMsg.
func testModelStreaming() *Model {
	m := testModel()
	m.setState(stateStreaming)
	m.streamGen = 1
	ch := make(chan agent.AgentEvent)
	close(ch)
	m.eventCh = ch
	return m
}

// assertState is a helper to check model state.
func assertState(t *testing.T, m *Model, want state) {
	t.Helper()
	if m.state != want {
		t.Errorf("state = %v, want %v", m.state, want)
	}
}

// ---- Construction ----

func TestModel_Defaults(t *testing.T) {
	m := testModel()
	if m.state != stateIdle {
		t.Errorf("new model should be idle, got %v", m.state)
	}
	if m.width != 100 || m.height != 40 {
		t.Errorf("default size = %dx%d, want 100x40", m.width, m.height)
	}
	if m.statusbar.ProviderInfo() != "test/model" {
		t.Errorf("provider info = %q, want %q", m.statusbar.ProviderInfo(), "test/model")
	}
}

// ---- setState ----

func TestSetState_Idle(t *testing.T) {
	m := testModel()
	m.setState(stateIdle)
	assertState(t, m, stateIdle)
	if !m.input.enabled {
		t.Error("input should be enabled in idle state")
	}
}

func TestSetState_Waiting(t *testing.T) {
	m := testModel()
	m.setState(stateWaiting)
	assertState(t, m, stateWaiting)
	if m.input.enabled {
		t.Error("input should be disabled in waiting state")
	}
}

func TestSetState_Streaming(t *testing.T) {
	m := testModel()
	m.setState(stateStreaming)
	assertState(t, m, stateStreaming)
	if !m.input.enabled {
		t.Error("input should be enabled in streaming state")
	}
}

func TestSetState_AwaitingConfirmation(t *testing.T) {
	m := testModel()
	m.setState(stateAwaitingConfirmation)
	assertState(t, m, stateAwaitingConfirmation)
	if m.input.enabled {
		t.Error("input should be disabled in awaiting confirmation state")
	}
}

func TestSetState_AskUserQuestion(t *testing.T) {
	m := testModel()
	m.setState(stateAskUserQuestion)
	assertState(t, m, stateAskUserQuestion)
	if m.input.enabled {
		t.Error("input should be disabled in ask user question state")
	}
}

// ---- Key handling: Ctrl+C ----

func TestCtrlC_Idle_Quits(t *testing.T) {
	m := testModel()
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if cmd == nil {
		t.Error("Ctrl+C in idle should return Quit command")
	}
}

func TestCtrlC_Streaming_Cancels(t *testing.T) {
	m := testModelStreaming()
	m.cancelFunc = func() {} // dummy
	_, _ = m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	// Should NOT return Quit; it should cancel the stream instead.
	// Note: handleCtrlC does not clear cancelFunc — that happens in streamDone/error.
	if m.cancelFunc == nil {
		t.Error("cancelFunc should still be set (cleared later by streamDone)")
	}
	// pending count should be cleared
	if m.statusbar.pendingCount != 0 {
		t.Error("pending count should be cleared after Ctrl+C")
	}
}

// TestCtrlC_Modal_CancelsAndDrainsEventChannel guards the regression where
// Ctrl+C during a modal state (confirmation prompt / AskUserQuestion form)
// cancelled the turn but returned a nil cmd, so the AgentEventError the
// agent emits on cancellation was never read: the UI stayed stuck in the
// modal forever (input appeared dead, "Ctrl+C can't interrupt").
//
// The fix: handleCtrlC queues nextEvent() after cancelling, so the terminal
// event reaches handleAgentEvent and the UI returns to stateIdle.
func TestCtrlC_Modal_CancelsAndDrainsEventChannel(t *testing.T) {
	for _, st := range []state{stateAwaitingConfirmation, stateAskUserQuestion} {
		m := testModel()
		m.setState(st)
		m.cancelFunc = func() {}
		m.streamGen = 1

		ch := make(chan agent.AgentEvent, 1)
		ch <- agent.AgentEvent{
			Type: agent.AgentEventError,
			Result: &agent.RunResult{
				ExitReason: agent.ExitReasonInterrupted,
			},
		}
		close(ch)
		m.eventCh = ch

		_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
		if cmd == nil {
			t.Fatalf("state %v: Ctrl+C should queue nextEvent to drain the terminal event", st)
		}
		if m.pendingConfirm != nil || m.askUserView != nil {
			t.Fatalf("state %v: modal state should be cleared on Ctrl+C", st)
		}

		// Run the queued cmd: it reads AgentEventError from the event channel.
		msg := cmd()
		if _, ok := msg.(agentEventMsg); !ok {
			t.Fatalf("state %v: queued cmd returned %T, want agentEventMsg", st, msg)
		}

		// Feed it back through Update: must transition to idle.
		m.Update(msg)
		assertState(t, m, stateIdle)
		if m.cancelFunc != nil {
			t.Errorf("state %v: cancelFunc should be cleared after terminal error", st)
		}
		if m.eventCh != nil {
			t.Errorf("state %v: eventCh should be cleared after terminal error", st)
		}
	}
}

// TestCtrlC_Cancels_KillsBackgroundProcesses guards the regression where
// Ctrl+C cancelled the turn but left background processes (started with
// background=true, e.g. an http server) running: the ProcessManager uses
// context.Background() by design, so only the turn-cancel path can stop them.
func TestCtrlC_Cancels_KillsBackgroundProcesses(t *testing.T) {
	m := testModelStreaming()
	m.cancelFunc = func() {}

	a := newTestAIAgent(t, nil, 10)
	defer a.Close()
	m.agent = a

	info, err := a.Config.ProcessManager.Start(t.Context(), "bg-http", "sleep 30")
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	pid := info.PID

	_, _ = m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return // background process killed with the turn
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("background process %d still alive after Ctrl+C", pid)
}

func TestCtrlC_SelectingModel_ExitsSelection(t *testing.T) {
	m := testModel()
	m.setState(stateSelectingModel)
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	assertState(t, m, stateIdle)
	_ = cmd
}

func TestCtrlC_SelectingSession_ExitsSelection(t *testing.T) {
	m := testModel()
	m.setState(stateSelectingSession)
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	assertState(t, m, stateIdle)
	_ = cmd
}

func TestCtrlC_ManagingMCP_ExitsOverlay(t *testing.T) {
	m := testModel()
	m.setState(stateManagingMCP)
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	assertState(t, m, stateIdle)
	_ = cmd
}

// ---- Key handling: Ctrl+O (thinking mode) ----

func TestCtrlO_TogglesThinkingOn(t *testing.T) {
	m := testModel()
	m.thinkingMode = false
	m.Update(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	if !m.thinkingMode {
		t.Error("Ctrl+O should enable thinking mode")
	}
}

func TestCtrlO_TogglesThinkingOff(t *testing.T) {
	m := testModel()
	m.thinkingMode = true
	m.Update(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	if m.thinkingMode {
		t.Error("Ctrl+O should disable thinking mode")
	}
}

// ---- Key handling: Ctrl+S (copy mode) ----

func TestCtrlS_TogglesCopyModeOn(t *testing.T) {
	m := testModel()
	m.copyMode = false
	m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	if !m.copyMode {
		t.Error("Ctrl+S should enable copy mode")
	}
	if !m.statusbar.copyMode {
		t.Error("statusbar copyMode should be synced")
	}
}

func TestCtrlS_TogglesCopyModeOff(t *testing.T) {
	m := testModel()
	m.copyMode = true
	m.statusbar.SetCopyMode(true)
	m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	if m.copyMode {
		t.Error("Ctrl+S should disable copy mode")
	}
	if m.statusbar.copyMode {
		t.Error("statusbar copyMode should be synced")
	}
}

// ---- Key handling: Esc (exit copy mode) ----

func TestEsc_ExitsCopyMode(t *testing.T) {
	m := testModel()
	m.copyMode = true
	m.statusbar.SetCopyMode(true)
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.copyMode {
		t.Error("Esc should exit copy mode")
	}
	if m.statusbar.copyMode {
		t.Error("statusbar copyMode should be synced")
	}
}

func TestEsc_NonCopyMode_NoOp(t *testing.T) {
	m := testModel()
	m.copyMode = false
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	// Should pass through to input — cmd may or may not be nil.
	_ = cmd
}

// ---- Agent event: text delta ----

func TestAgentEvent_TextDelta_EntersStreaming(t *testing.T) {
	m := testModel()
	m.setState(stateWaiting)
	m.streamGen = 1
	ch := make(chan agent.AgentEvent)
	close(ch) // nextEvent() will get streamDoneMsg
	m.eventCh = ch

	_, _ = m.Update(agentEventMsg{
		event: agent.AgentEvent{Type: agent.AgentEventTextDelta, TextDelta: "hello "},
		gen:   1,
	})

	assertState(t, m, stateStreaming)
}

func TestAgentEvent_TextDelta_IgnoresStaleGen(t *testing.T) {
	m := testModel()
	m.setState(stateStreaming)
	m.streamGen = 5 // current gen is 5

	_, _ = m.Update(agentEventMsg{
		event: agent.AgentEvent{Type: agent.AgentEventTextDelta, TextDelta: "stale"},
		gen:   3, // old gen — should be ignored
	})

	// State should remain streaming, no change
	assertState(t, m, stateStreaming)
}

// ---- Agent event: thinking delta ----

func TestAgentEvent_ThinkingDelta(t *testing.T) {
	m := testModel()
	m.setState(stateWaiting)
	m.streamGen = 1
	ch := make(chan agent.AgentEvent)
	close(ch)
	m.eventCh = ch

	_, _ = m.Update(agentEventMsg{
		event: agent.AgentEvent{Type: agent.AgentEventThinkingDelta, ThinkingDelta: "hmm..."},
		gen:   1,
	})

	assertState(t, m, stateStreaming)
}

// ---- Agent event: tool call start + args ----

func TestAgentEvent_ToolCallStart(t *testing.T) {
	m := testModel()
	m.setState(stateWaiting)
	m.streamGen = 1
	ch := make(chan agent.AgentEvent)
	close(ch)
	m.eventCh = ch

	_, _ = m.Update(agentEventMsg{
		event: agent.AgentEvent{Type: agent.AgentEventToolCallStart, ToolName: "Bash", ToolID: "tc_1"},
		gen:   1,
	})

	assertState(t, m, stateStreaming)
}

func TestAgentEvent_ToolCallArgs(t *testing.T) {
	m := testModel()
	m.setState(stateStreaming)
	m.streamGen = 1
	ch := make(chan agent.AgentEvent)
	close(ch)
	m.eventCh = ch

	_, _ = m.Update(agentEventMsg{
		event: agent.AgentEvent{Type: agent.AgentEventToolCallArgs, ToolID: "tc_1", ToolArgs: `{"cmd":"ls"}`},
		gen:   1,
	})

	assertState(t, m, stateStreaming)
}

// ---- Agent event: tool confirmation ----

func TestAgentEvent_ToolConfirmation(t *testing.T) {
	m := testModel()
	m.setState(stateStreaming)
	m.streamGen = 1
	ch := make(chan agent.AgentEvent)
	close(ch)
	m.eventCh = ch

	_, _ = m.Update(agentEventMsg{
		event: agent.AgentEvent{
			Type:     agent.AgentEventToolConfirmation,
			ToolName: "EditFile",
			ToolID:   "tc_2",
			ToolArgs: `{"path":"f.txt"}`,
			ToolDiff: "-old\n+new",
		},
		gen: 1,
	})

	assertState(t, m, stateAwaitingConfirmation)
	if m.pendingConfirm == nil {
		t.Fatal("pendingConfirm should be set")
	}
	if m.pendingConfirm.toolName != "EditFile" {
		t.Errorf("toolName = %q, want EditFile", m.pendingConfirm.toolName)
	}
	if m.pendingConfirm.diff != "-old\n+new" {
		t.Errorf("diff = %q", m.pendingConfirm.diff)
	}
}

// ---- Agent event: tool result ----

func TestAgentEvent_ToolResult(t *testing.T) {
	m := testModel()
	m.setState(stateStreaming)
	m.streamGen = 1
	ch := make(chan agent.AgentEvent)
	close(ch)
	m.eventCh = ch

	_, _ = m.Update(agentEventMsg{
		event: agent.AgentEvent{Type: agent.AgentEventToolResult, ToolID: "tc_1", ToolResult: "ok", ToolIsError: false},
		gen:   1,
	})

	assertState(t, m, stateStreaming)
}

// ---- Agent event: error ----

func TestAgentEvent_Error_ResetsState(t *testing.T) {
	m := testModel()
	m.setState(stateStreaming)
	m.streamGen = 1
	// No eventCh needed — error doesn't call nextEvent

	_, _ = m.Update(agentEventMsg{
		event: agent.AgentEvent{
			Type: agent.AgentEventError,
			Result: &agent.RunResult{
				Error:      nil,
				ExitReason: agent.ExitReasonInterrupted,
			},
		},
		gen: 1,
	})

	assertState(t, m, stateIdle)
	if m.cancelFunc != nil {
		t.Error("cancelFunc should be cleared on error")
	}
	if m.eventCh != nil {
		t.Error("eventCh should be cleared on error")
	}
}

func TestAgentEvent_Error_WithAPIError(t *testing.T) {
	m := testModel()
	m.setState(stateStreaming)
	m.streamGen = 1

	_, _ = m.Update(agentEventMsg{
		event: agent.AgentEvent{
			Type: agent.AgentEventError,
			Result: &agent.RunResult{
				Error:      errors.New("rate limit exceeded"),
				ExitReason: agent.ExitReasonError,
			},
		},
		gen: 1,
	})

	assertState(t, m, stateIdle)
}

// ---- Agent event: turn complete ----
//
// TurnComplete calls syncSessionInfo which requires a non-nil agent.
// Full turn-complete tests need an agent mock; see agent package for
// the agent-loop integration tests (agent_part1_test.go, agent_part2_test.go).

func TestWindowResize(t *testing.T) {
	m := testModel()
	_, cmd := m.Update(tea.WindowSizeMsg{Width: 120, Height: 50})
	if m.width != 120 || m.height != 50 {
		t.Errorf("resize: got %dx%d, want 120x50", m.width, m.height)
	}
	_ = cmd
}

// ---- streamDoneMsg ----

func TestStreamDone_ReturnsToIdle(t *testing.T) {
	m := testModelStreaming()

	_, _ = m.Update(streamDoneMsg{gen: 1})

	assertState(t, m, stateIdle)
	if m.eventCh != nil {
		t.Error("eventCh should be cleared")
	}
	if m.cancelFunc != nil {
		t.Error("cancelFunc should be cleared")
	}
}

func TestStreamDone_IgnoresStaleGen(t *testing.T) {
	m := testModelStreaming()
	m.streamGen = 5

	_, _ = m.Update(streamDoneMsg{gen: 3})

	// Should NOT transition to idle — gen is stale
	assertState(t, m, stateStreaming)
}

func TestStreamDone_ClearsPendingQueue(t *testing.T) {
	m := testModelStreaming()
	m.pendingQueue = []string{"msg1", "msg2"}
	m.statusbar.SetPendingCount(2)

	_, _ = m.Update(streamDoneMsg{gen: 1})

	if len(m.pendingQueue) != 0 {
		t.Error("pendingQueue should be cleared")
	}
	if m.statusbar.pendingCount != 0 {
		t.Errorf("pendingCount = %d, want 0", m.statusbar.pendingCount)
	}
}

// ---- Pending queue during streaming ----

func TestInputSubmit_QueuesDuringStreaming(t *testing.T) {
	m := testModelStreaming()

	_, _ = m.Update(InputSubmitMsg("next message"))

	if len(m.pendingQueue) != 1 {
		t.Fatalf("pendingQueue length = %d, want 1", len(m.pendingQueue))
	}
	if m.pendingQueue[0] != "next message" {
		t.Errorf("pendingQueue[0] = %q", m.pendingQueue[0])
	}
	if m.statusbar.pendingCount != 1 {
		t.Errorf("pendingCount = %d, want 1", m.statusbar.pendingCount)
	}
}

func TestInputSubmit_MultipleQueueDuringStreaming(t *testing.T) {
	m := testModelStreaming()

	_, _ = m.Update(InputSubmitMsg("first"))
	_, _ = m.Update(InputSubmitMsg("second"))
	_, _ = m.Update(InputSubmitMsg("third"))

	if len(m.pendingQueue) != 3 {
		t.Fatalf("pendingQueue length = %d, want 3", len(m.pendingQueue))
	}
	if m.statusbar.pendingCount != 3 {
		t.Errorf("pendingCount = %d, want 3", m.statusbar.pendingCount)
	}
}

func TestInputSubmit_EmptyDuringStreaming_NoOp(t *testing.T) {
	m := testModelStreaming()

	_, _ = m.Update(InputSubmitMsg(""))

	if len(m.pendingQueue) != 0 {
		t.Error("empty submit should not queue")
	}
}

// ---- Key routing by state ----

func TestKeyRouting_AwaitingConfirmation_IgnoresOtherKeys(t *testing.T) {
	m := testModel()
	m.setState(stateAwaitingConfirmation)
	m.pendingConfirm = &pendingConfirm{
		toolName: "EditFile",
		toolID:   "tc_1",
	}

	_, _ = m.Update(tea.KeyPressMsg{Code: 'x'})

	// Should remain in confirmation state, pendingConfirm unchanged
	assertState(t, m, stateAwaitingConfirmation)
	if m.pendingConfirm == nil {
		t.Error("pendingConfirm should still be set for unrecognized key")
	}
}

// ---- View ----

func TestView_Loading(t *testing.T) {
	m := testModel()
	m.width = 0
	v := m.View()
	if v.Content != "Loading..." {
		t.Errorf("View() with zero width = %q, want Loading...", v.Content)
	}
}

func TestView_IncludesStatusBar(t *testing.T) {
	m := testModel()
	v := m.View()
	if v.Content == "" {
		t.Error("View() should not be empty")
	}
	if v.Content == "Loading..." {
		t.Skip("zero width, can't check statusbar")
	}
}

func TestView_AltScreenEnabled(t *testing.T) {
	m := testModel()
	v := m.View()
	if !v.AltScreen {
		t.Error("View() should set AltScreen")
	}
}

func TestView_MouseMode_CellMotion(t *testing.T) {
	m := testModel()
	m.copyMode = false
	v := m.View()
	// In copyMode=false, mouse mode should be CellMotion
	// In copyMode=true, mouse mode is None (no mouse tracking)
	if v.MouseMode != tea.MouseModeCellMotion {
		t.Error("non-copy-mode should have CellMotion mouse mode")
	}
}

func TestView_MouseMode_CopyMode(t *testing.T) {
	m := testModel()
	m.copyMode = true
	v := m.View()
	// In copy mode, mouse mode is off so terminal can handle selection natively
	if v.MouseMode != tea.MouseModeNone {
		t.Error("copy mode should have mouse mode None")
	}
}

// TestStatusBar_LiveContextFraction guards the implicit dependency where the
// statusbar's NN% (percentage-only) display is fed by the LIVE event path:
// handleAgentEvent fills totalUsage.LastInputTokens from the agent's local
// estimate on AgentEventUsage. If a future refactor removes that assignment,
// the statusbar would silently stop showing context usage in a fresh session
// (no persisted session to restore it from).
func TestStatusBar_LiveContextFraction(t *testing.T) {
	a := newTestAIAgent(t, nil, 10)
	defer a.Close()
	// Seed the agent's local token estimate (~16k tokens for 64k chars).
	a.EstimateAndUpdateTokens(&agent.RunState{}, []llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: strings.Repeat("x", 64_000)},
	})

	m := testModel()
	m.agent = a
	m.statusbar.SetContextWindow(128_000)

	// Simulate a live usage update arriving mid-turn (fresh session, no
	// persisted history). handleAgentEvent must populate LastInputTokens
	// from the estimate and render the statusbar.
	m.handleAgentEvent(agent.AgentEvent{
		Type:  agent.AgentEventUsage,
		Usage: &llm.Usage{InputTokens: 64_000},
	})

	view := m.statusbar.View()
	// 16_000 / 128_000 = 12.5% → "12%" (round-half-to-even) or "13%".
	if !strings.Contains(view, "12%") && !strings.Contains(view, "13%") {
		t.Errorf("ctx percentage should be ~12%%: got %q", view)
	}
	// Percentage-only display: no n/m token breakdown.
	if strings.Contains(view, "/128") {
		t.Errorf("statusbar should NOT show the n/m token breakdown: %q", view)
	}
}

// ---- thinking-level statusbar indicator ----

// TestSyncThinkingBadge verifies the statusbar thinking indicator shows the
// USER-SELECTED level (raw, not model-normalized): session override wins,
// then the provider's configured thinking_level, else "default".
func TestSyncThinkingBadge(t *testing.T) {
	provider, err := llm.NewProvider("openai", "sk", "", "deepseek-v4-flash")
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	a := newTestAIAgent(t, provider, 0)
	m := testModel()
	m.agent = a

	// No session, no config → "default".
	m.syncThinkingBadge()
	if got := m.statusbar.thinkingLevel; got != "default" {
		t.Errorf("thinkingLevel = %q, want %q", got, "default")
	}

	// Provider configured thinking_level=max (raw — even though the model
	// only supports low/high, the statusbar reflects the config).
	cfg := &config.Config{
		Provider: "deepseek",
		Providers: []config.ProviderConfig{
			{Name: "deepseek", Type: "openai", Model: "deepseek-v4-flash", Spec: config.ModelSpec{ThinkingLevel: "max"}},
		},
	}
	m.cfg = cfg
	m.syncThinkingBadge()
	if got := m.statusbar.thinkingLevel; got != "max" {
		t.Errorf("thinkingLevel = %q, want %q (provider config)", got, "max")
	}

	// Session override (set via /thinking) wins over provider config and is
	// shown raw (medium — not normalized down to low).
	store, err := session.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("session store: %v", err)
	}
	sm := session.NewManagerWithStore(store, nil)
	if _, err := sm.New("deepseek", "/tmp"); err != nil {
		t.Fatalf("create session: %v", err)
	}
	a.SetSessionManager(sm)
	cur := sm.Current()
	cur.ThinkingLevel = "medium"
	if err := sm.UpdateMeta(cur); err != nil {
		t.Fatalf("update meta: %v", err)
	}
	m.syncThinkingBadge()
	if got := m.statusbar.thinkingLevel; got != "medium" {
		t.Errorf("thinkingLevel = %q, want %q (session override)", got, "medium")
	}

	// Clearing the override falls back to the provider config again.
	cur.ThinkingLevel = ""
	if err := sm.UpdateMeta(cur); err != nil {
		t.Fatalf("update meta: %v", err)
	}
	m.syncThinkingBadge()
	if got := m.statusbar.thinkingLevel; got != "max" {
		t.Errorf("thinkingLevel = %q, want %q (fallback to provider)", got, "max")
	}
}

// TestHandleThinkingCommand_NoSession verifies /thinking works without an
// active session (right after startup): it records a pending per-session
// override that the next auto-created session inherits, updates the agent
// immediately, and reflects the state in the statusbar badge.
func TestHandleThinkingCommand_NoSession(t *testing.T) {
	provider, err := llm.NewProvider("openai", "sk", "", "deepseek-v4-flash")
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	a := newTestAIAgent(t, provider, 0)
	m := testModel()
	m.agent = a
	m.cfg = &config.Config{
		Provider: "deepseek",
		Providers: []config.ProviderConfig{
			{Name: "deepseek", Type: "openai", Model: "deepseek-v4-flash", APIKey: "sk-test", Spec: config.ModelSpec{ThinkingLevel: "max"}},
		},
	}
	lastMsg := func() string {
		if n := len(m.chatview.items); n > 0 {
			return m.chatview.items[n-1].msg.Content
		}
		return ""
	}

	// Show level without a session → no pending override → "default".
	m.subcommandInput = "/thinking"
	m.handleThinkingCommand()
	if got := lastMsg(); !strings.Contains(got, "**default**") {
		t.Errorf("show output = %q, want it to mention default", got)
	}
	// The badge reflects the provider's configured level (not the override).
	if got := m.currentThinkingLevel(); got != "max" {
		t.Errorf("currentThinkingLevel = %q, want %q (provider default)", got, "max")
	}

	// Set a level without a session → pending override recorded, agent
	// updated immediately, badge reflects it.
	m.subcommandInput = "/thinking high"
	m.handleThinkingCommand()
	if got := a.PendingSessionThinking(); got != "high" {
		t.Errorf("PendingSessionThinking = %q, want %q", got, "high")
	}
	if got := a.Config.Resolved.ThinkingEffort; got != "high" {
		t.Errorf("Config.ThinkingEffort = %q, want %q (applied immediately)", got, "high")
	}
	if got := m.statusbar.thinkingLevel; got != "high" {
		t.Errorf("thinkingLevel = %q, want %q (pending override)", got, "high")
	}
	if got := lastMsg(); !strings.Contains(got, "将在创建首个会话时生效") {
		t.Errorf("set output = %q, want it to mention first-session effect", got)
	}

	// Show level again → reflects the pending override.
	m.subcommandInput = "/thinking"
	m.handleThinkingCommand()
	if got := lastMsg(); !strings.Contains(got, "**high**") {
		t.Errorf("show output = %q, want it to mention high", got)
	}

	// Reset via "default" → pending cleared, badge back to provider default.
	m.subcommandInput = "/thinking default"
	m.handleThinkingCommand()
	if got := a.PendingSessionThinking(); got != "" {
		t.Errorf("PendingSessionThinking = %q, want %q after /thinking default", got, "")
	}
	if got := m.statusbar.thinkingLevel; got != "max" {
		t.Errorf("thinkingLevel = %q, want %q (provider default)", got, "max")
	}
}

// ---- Session cost (statusbar) ----

// testModelWithAgent returns a minimal Model whose agent has a real provider,
// so costForUsage can resolve prices.
func testModelWithAgent(t *testing.T, provider llm.Provider) *Model {
	m := testModel()
	m.agent = newTestAIAgent(t, provider, 10)
	return m
}

// TestModel_CostForUsage_OpenAIFamily verifies the OpenAI-family billing
// scale: input_tokens INCLUDE cache reads, so they are subtracted before
// billing (mirroring the usage ledger). Flat pricing is pinned via cfg
// (input 1.0, output 2.0, cache read 0.02 CNY per 1M tokens) so the test is
// deterministic and independent of the built-in table's time-of-use bands.
func TestModel_CostForUsage_OpenAIFamily(t *testing.T) {
	provider, err := llm.NewProvider("openai", "sk", "", "deepseek-v4-flash")
	if err != nil {
		t.Fatal(err)
	}
	m := testModelWithAgent(t, provider)
	defer m.agent.Close()
	m.cfg = &config.Config{
		Providers: []config.ProviderConfig{
			{
				Name:  "openai",
				Type:  "openai",
				Model: "deepseek-v4-flash",
				Spec: config.ModelSpec{
					Pricing: &config.PricingConfig{
						InputPrice:          new(1.0),
						OutputPrice:         new(2.0),
						CacheReadInputPrice: new(0.02),
					},
				},
			},
		},
	}

	cost := m.costForUsage(&llm.Usage{
		InputTokens:          1_000_000,
		CacheReadInputTokens: 200_000,
		OutputTokens:         500_000,
	})
	// input = 1M - 200K = 800K → 0.8; cache read 200K → 0.004; output 500K → 1.0
	want := 0.8 + 0.004 + 1.0
	if math.Abs(cost-want) > 1e-9 {
		t.Errorf("costForUsage(openai) = %v, want %v", cost, want)
	}
}

// TestModel_CostForUsage_Anthropic verifies Anthropic's input_tokens are NOT
// cache-read-normalized (they exclude cache reads to begin with) — same as
// the ledger. Custom pricing: input 3.0, output 15.0, cache read 0.3.
func TestModel_CostForUsage_Anthropic(t *testing.T) {
	provider, err := llm.NewProvider("anthropic", "sk", "", "claude-sonnet-4-5")
	if err != nil {
		t.Fatal(err)
	}
	m := testModelWithAgent(t, provider)
	defer m.agent.Close()
	m.cfg = &config.Config{
		Providers: []config.ProviderConfig{
			{
				Name:  "claude",
				Type:  "anthropic",
				Model: "claude-sonnet-4-5",
				Spec: config.ModelSpec{
					Pricing: &config.PricingConfig{
						InputPrice:          new(3.0),
						OutputPrice:         new(15.0),
						CacheReadInputPrice: new(0.3),
					},
				},
			},
		},
	}
	// No session yet → currentProviderName falls back to the single
	// configured provider ("claude"), which carries the custom pricing.
	cost := m.costForUsage(&llm.Usage{
		InputTokens:          1_000_000,
		CacheReadInputTokens: 200_000,
		OutputTokens:         500_000,
	})
	// input kept at 1M (no cache-read subtraction) → 3.0;
	// cache read 200K → 0.06; output 500K → 7.5. Total 10.56.
	want := 3.0 + 0.06 + 7.5
	if math.Abs(cost-want) > 1e-9 {
		t.Errorf("costForUsage(anthropic) = %v, want %v", cost, want)
	}
}

// TestModel_CostForUsage_NoPrice ensures an unpriced model yields 0.
func TestModel_CostForUsage_NoPrice(t *testing.T) {
	provider, err := llm.NewProvider("openai", "sk", "", "some-unknown-model")
	if err != nil {
		t.Fatal(err)
	}
	m := testModelWithAgent(t, provider)
	defer m.agent.Close()

	if cost := m.costForUsage(&llm.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}); cost != 0 {
		t.Errorf("costForUsage(unpriced) = %v, want 0", cost)
	}
}

// TestModel_AccumulateUsage_Cost verifies accumulateUsage accumulates the
// per-call cost into sessionCost and pushes it to the statusbar.
func TestModel_AccumulateUsage_Cost(t *testing.T) {
	provider, err := llm.NewProvider("openai", "sk", "", "deepseek-v4-flash")
	if err != nil {
		t.Fatal(err)
	}
	m := testModelWithAgent(t, provider)
	defer m.agent.Close()
	// Pin flat pricing (1.0/2.0) via cfg for determinism — independent of the
	// built-in table's time-of-use bands.
	m.cfg = &config.Config{
		Providers: []config.ProviderConfig{
			{
				Name:  "openai",
				Type:  "openai",
				Model: "deepseek-v4-flash",
				Spec: config.ModelSpec{
					Pricing: &config.PricingConfig{
						InputPrice:  new(1.0),
						OutputPrice: new(2.0),
					},
				},
			},
		},
	}

	// 1M input + 1M output → 1.0 + 2.0 = 3.0.
	m.accumulateUsage(&llm.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000})
	if m.sessionCost != 3.0 {
		t.Errorf("sessionCost after first call = %v, want 3.0", m.sessionCost)
	}
	if m.statusbar.cost != 3.0 {
		t.Errorf("statusbar cost = %v, want 3.0", m.statusbar.cost)
	}

	// Second call (no input, 500K output) → +1.0, total 4.0.
	m.accumulateUsage(&llm.Usage{OutputTokens: 500_000})
	if m.sessionCost != 4.0 {
		t.Errorf("sessionCost after second call = %v, want 4.0", m.sessionCost)
	}
	if m.statusbar.cost != 4.0 {
		t.Errorf("statusbar cost = %v, want 4.0", m.statusbar.cost)
	}
}
