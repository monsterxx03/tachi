package main

import (
	"slices"
	"strings"
	"testing"

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
	out, err := agent.ConvertSessionToLLMMessages(msgs, config.ProviderTypeOpenAI)
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
