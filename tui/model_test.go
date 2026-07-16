package tui

import (
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/monsterxx03/tachi/agent"
)

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
				ExitReason: "interrupted",
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
				ExitReason: "error",
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
