package main

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/systemreminder"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// convertedHistory runs the real session → llm conversion the desktop reloads a session
// with, so a test cannot hand-write a history shape the product never produces.
func convertedHistory(t *testing.T, msgs []session.Message) []llm.Message {
	t.Helper()
	out, err := agent.ConvertSessionToLLMMessages(msgs, config.ProviderTypeOpenAI, nil)
	if err != nil {
		t.Fatalf("ConvertSessionToLLMMessages: %v", err)
	}
	return out
}

// TestMergeTrailingUserMessageDropsTheStaleReminder pins the fix for a bug that only
// shows up across turns: an interrupted (or killed) session can leave the history ending
// on a user message, and merging that message into the next turn must not carry its
// <system-reminder> block along. The block describes the turn it was built for — an old
// plan id, old git state — and because the merged text is recorded as the new message, a
// carried block would walk forward through the session turn after turn.
func TestMergeTrailingUserMessageDropsTheStaleReminder(t *testing.T) {
	block := systemreminder.RenderPieces([]systemreminder.Piece{
		{Name: "plan", Lines: []string{"Active plan: plan-OLD step 1 pending"}},
	})
	history := convertedHistory(t, []session.Message{
		{Type: session.MessageTypeUser, Content: "第一件事"},
		{Type: session.MessageTypeAssistant, Content: "好"},
		{Type: session.MessageTypeReminder, Content: block},
		{Type: session.MessageTypeUser, Content: "被打断的那条"},
	})

	// The premise this whole function exists for: conversion re-attaches the block to the
	// user message it was stored with (deliberately — historyHasReminder reads the
	// prefix), so the desktop's merge is where it has to come off again. The fixture's
	// four recorded messages convert to three: the block rides on the trailing message.
	if len(history) != 3 {
		t.Fatalf("fixture: expected the block folded into the trailing message, got %d messages", len(history))
	}
	if trailing := history[len(history)-1]; !strings.HasPrefix(trailing.Content, systemreminder.ReminderBlockOpen) {
		t.Fatalf("fixture: the converted history should carry the block, got %q", trailing.Content)
	}

	got, text := mergeTrailingUserMessage(history, "接着来")

	if len(got) != 2 {
		t.Errorf("the trailing message must leave the history, got %d messages", len(got))
	}
	if !strings.Contains(text, "被打断的那条") || !strings.Contains(text, "接着来") {
		t.Errorf("both the old and the new text must survive the merge, got %q", text)
	}
	if strings.Contains(text, "plan-OLD") || strings.Contains(text, systemreminder.ReminderBlockOpen) {
		t.Errorf("the previous turn's reminder block must not be carried over, got %q", text)
	}
}

// TestMergeTrailingUserMessageKeepsOrderingAndPlainText covers the rest of the contract:
// only a trailing USER message is merged (an assistant reply or an empty history is left
// alone), and a message with no block is merged unchanged.
func TestMergeTrailingUserMessageKeepsOrderingAndPlainText(t *testing.T) {
	tests := []struct {
		name        string
		history     []llm.Message
		wantLen     int
		wantContain []string
	}{
		{
			name:        "a reply ends the history — nothing to merge",
			history:     []llm.Message{{Role: "user", Content: "旧的"}, {Role: "assistant", Content: "回复"}},
			wantLen:     2,
			wantContain: []string{"接着来"},
		},
		{
			name:        "empty history",
			history:     nil,
			wantLen:     0,
			wantContain: []string{"接着来"},
		},
		{
			name:        "plain trailing user message",
			history:     []llm.Message{{Role: "user", Content: "被打断的那条"}},
			wantLen:     0,
			wantContain: []string{"被打断的那条", "接着来"},
		},
		{
			// No user text to preserve and no block to strip down to: the message keeps
			// its content rather than being dropped silently.
			name:        "trailing message is only a reminder block",
			history:     []llm.Message{{Role: "user", Content: systemreminder.ReminderBlockOpen + "\n工件提示\n" + systemreminder.ReminderBlockClose + "\n"}},
			wantLen:     0,
			wantContain: []string{"工件提示", "接着来"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, text := mergeTrailingUserMessage(tc.history, "接着来")
			if len(got) != tc.wantLen {
				t.Errorf("history = %d messages, want %d", len(got), tc.wantLen)
			}
			for _, want := range tc.wantContain {
				if !strings.Contains(text, want) {
					t.Errorf("merged text %q is missing %q", text, want)
				}
			}
		})
	}
}

// TestAFinishedTurnIsNotReportedAsRunning pins the ORDER the frontend depends on between two
// things it learns a turn's end from: the turn's own terminal event (which is what makes it
// re-read the running set — the only read there is) and the running set itself.
//
// `sessionRun.running` is the guard that keeps a second turn of the same session from starting,
// and `endTurn` clears it. But `endTurn` runs only after the agent's event loop has exited, while
// turn_complete is published from INSIDE that loop: answering RunningSessions from the flag told
// the frontend "the turn is over" (its transcript concludes, its queue becomes flushable) in the
// same breath as "this session is still running", and nothing later corrects it — the sidebar's
// spinner, the stop control and the composer's queue all stayed stuck behind a turn that had
// already finished. It is timing, so it shows up as an intermittent failure in a driver that
// waits for the stop control to clear (the full suite caught it in `rewind` / `transcript-fold`).
func TestAFinishedTurnIsNotReportedAsRunning(t *testing.T) {
	const id = "s1"
	cases := []struct {
		name string
		ev   agent.AgentEvent
	}{
		// A natural end: the session goes back to idle. Result is left nil — the payload's
		// other fields (cost, checkpoint, notification) are not what this test is about.
		{name: "turn_complete", ev: agent.AgentEvent{Type: agent.AgentEventTurnComplete}},
		// A user-initiated stop is a normal conclusion, and any other failure is an error
		// state; neither of them is running.
		{name: "stopped", ev: agent.AgentEvent{Type: agent.AgentEventError,
			Result: &agent.RunResult{ExitReason: agent.ExitReasonInterrupted}}},
		{name: "error", ev: agent.AgentEvent{Type: agent.AgentEventError,
			Result: &agent.RunResult{ExitReason: agent.ExitReasonError}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestApp()
			d.runs[id] = &sessionRun{running: true, state: AgentState{Status: StatusThinking, Label: "思考"}}
			svc := &AgentService{desk: d}

			// The premise: a session with a turn in flight is listed.
			if got := svc.RunningSessions(); !slices.Contains(got, id) {
				t.Fatalf("fixture: a busy session must be listed, got %v", got)
			}

			// Whatever the frontend does when it is told the turn ended, it does after this
			// returns — including re-reading this list.
			d.handleEvent(id, tc.ev)

			if got := svc.RunningSessions(); slices.Contains(got, id) {
				t.Errorf("the session is still reported as running after its turn ended: %v", got)
			}
		})
	}
}

// TestAMidTurnFailureKeepsTheStepsTheTurnAlreadyRan pins issue #38. A stream that dies while the
// turn is running (the client's own timeout, a dropped connection) reaches the frontend as an
// AgentEventError with ExitReason "error" — NOT "interrupted" — and this handler used to keep the
// partial history only in the interrupted branch. The tool calls that had already run were
// therefore missing from the next turn's request (while the session FILE kept them, so the model
// and the transcript disagreed about what had happened), and the model re-did work it had done.
//
// The exit is deliberately the hard one: that is the whole point of the test.
func TestAMidTurnFailureKeepsTheStepsTheTurnAlreadyRan(t *testing.T) {
	const id = "s1"
	d := newTestApp()
	d.runs[id] = &sessionRun{history: []llm.Message{{Role: "user", Content: "旧的一轮"}}}
	// What the loop hands over after a failed stream: the history plus the user's message and the
	// tool call this turn already executed (with its result — pairs, never a dangling tool call).
	partial := []llm.Message{
		{Role: "user", Content: "旧的一轮"},
		{Role: "user", Content: "把 X 改掉"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function", Function: llm.ToolCallFunction{Name: "Bash", Arguments: "{}"}}}},
		{Role: "tool", ToolCallID: "c1", Content: "改完了"},
	}
	d.handleEvent(id, agent.AgentEvent{
		Type:     agent.AgentEventError,
		Messages: partial,
		Result:   &agent.RunResult{ExitReason: agent.ExitReasonError, Error: errors.New("context deadline exceeded")},
	})

	got := d.runHistory(id)
	if len(got) != len(partial) {
		t.Fatalf("history = %d 条, want %d（这一轮已经执行的步骤必须留着）: %+v", len(got), len(partial), got)
	}
	if got[3].Role != "tool" || got[3].Content != "改完了" {
		t.Errorf("最后一条 = %+v, want 这一轮那条工具结果", got[3])
	}
	// …and the turn is still a FAILURE: keeping the history is not the same as calling it a
	// success, so the state (which the window shows) must not have turned into 空闲.
	if st := d.runs[id].state; st.Status != StatusError {
		t.Errorf("state = %+v, want StatusError（失败还是失败）", st)
	}
}

// TestSendMessageNamesItsSessionAndRefusesInsteadOfDropping pins the binding's half of a bug that
// could only ever be silent: SendMessage read the session from d.activeID (which can be empty, or
// a different session than the one on screen — a restart or a stale window is enough), and both of
// startTurn's escape hatches returned without a word while SendMessage answered "ok". The frontend
// draws the user's bubble and a running placeholder BEFORE the call, so a dropped message left the
// reader looking at their own text under a 正在执行 that nothing could clear — with no turn
// started, nothing written, and no error anywhere.
func TestSendMessageNamesItsSessionAndRefusesInsteadOfDropping(t *testing.T) {
	d, svc, sid, _ := newDeleteApp(t)

	// No session named: a refusal, not silence.
	if got := svc.SendMessage("", "你好"); got != refuseNoSession {
		t.Errorf("SendMessage with no session = %q, want %q", got, refuseNoSession)
	}

	// A session whose turn is in flight: refused, and it says why. The frontend queues rather than
	// sends while it believes the session is running, so reaching this means the two sides disagree
	// — which is exactly when silence cost the most.
	d.getRun(sid).running = true
	if got := svc.SendMessage(sid, "你好"); got != refuseTurnRunning {
		t.Errorf("SendMessage on a running session = %q, want %q", got, refuseTurnRunning)
	}
	d.getRun(sid).running = false

	// The ordinary case still starts a turn (the simulated fallback: this app has no config), and
	// "ok" is what tells the caller the bubble it already drew has a turn behind it.
	if got := svc.SendMessage(sid, "你好"); got != "ok" {
		t.Errorf("SendMessage = %q, want ok", got)
	}
	d.stopSimulatedTurn()
}

// 菜单栏的文字：今日花费 + 上下文占用，两个都量不出来时退回状态词（那一格不该空着）。
func TestTrayLabelJoinsWhatIsMeasured(t *testing.T) {
	st := AgentState{Status: StatusThinking, Label: "思考"}

	if got := trayLabel(st, "¥0.420", "62%"); got != "¥0.420 · 62%" {
		t.Errorf("trayLabel = %q", got)
	}
	if got := trayLabel(st, "", "62%"); got != "62%" {
		t.Errorf("trayLabel = %q，只有上下文时不该留一个空的分隔符", got)
	}
	if got := trayLabel(st, "¥0.420", ""); got != "¥0.420" {
		t.Errorf("trayLabel = %q", got)
	}
	if got := trayLabel(st, "", ""); got != "思考" {
		t.Errorf("trayLabel = %q，应当退回状态词", got)
	}
}

// 上下文那格画百分比，量不出来就不画——不编一个数出来（与 formatContext 对「窗口未知」是
// 同一个态度）。
func TestTrayContextIsAPercentageOrNothing(t *testing.T) {
	if got := trayContext(42000, 100000); got != "42%" {
		t.Errorf("trayContext = %q，期望 42%%", got)
	}
	if got := trayContext(99500, 100000); got != "100%" {
		t.Errorf("trayContext = %q，四舍五入到 100%%", got)
	}
	for _, tc := range [][2]int64{{42000, 0}, {0, 100000}, {0, 0}} {
		if got := trayContext(tc[0], tc[1]); got != "" {
			t.Errorf("trayContext(%d, %d) = %q，应当为空", tc[0], tc[1], got)
		}
	}
}

// 今日花费只算今天：台账是**按天一个文件**的，所以「今天」就等于从今天那个文件开始扫——昨天
// 那几行在另一个文件里，不该被算进来。
func TestTrayTodayCostOnlyCountsToday(t *testing.T) {
	rec := llm.NewUsageRecorder(t.TempDir())
	now := time.Now()
	today := llm.UsageRow{TS: now, SessionID: "s1", Model: "test", InputTokens: 1000, InputPrice: 3000}
	yesterday := llm.UsageRow{TS: now.AddDate(0, 0, -1), SessionID: "s1", Model: "test",
		InputTokens: 1_000_000, InputPrice: 3000}
	for _, row := range []llm.UsageRow{today, yesterday} {
		if err := rec.Record(row); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	// 只加一行，所以这里是精确相等（没有累加的浮点误差）。
	if got, want := trayTodayCost(rec, now), today.Cost(); got != want {
		t.Errorf("trayTodayCost = %v，期望 %v（昨天那一行在另一个文件里）", got, want)
	}
	// 还没初始化台账时给 0，而不是崩。
	if got := trayTodayCost(nil, now); got != 0 {
		t.Errorf("trayTodayCost(nil) = %v", got)
	}
}
