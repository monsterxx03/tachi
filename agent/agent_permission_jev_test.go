package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/agent/permission"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/jev"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAskJudge answers every judgment with the same probability, and counts the
// calls (the memo's whole point is that this number stays small).
type fakeAskJudge struct {
	mu         sync.Mutex
	calls      int
	p          float64
	err        error
	unanswered bool
}

func (f *fakeAskJudge) Evaluate(ctx context.Context, state any, questions map[string]jev.Question) (*jev.Response, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	p := f.p
	answer := jev.Answer{Type: jev.QuestionNoul}
	if !f.unanswered {
		answer.Noul = &p
	}
	return &jev.Response{
		Model:   "fake",
		Answers: map[string]jev.Answer{"need_human": answer},
		Usage:   jev.Usage{InputTokens: 100, OutputTokens: 0},
	}, nil
}

func (f *fakeAskJudge) Model() string { return "fake" }

func (f *fakeAskJudge) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// askGateAgent builds an agent whose judgment gate is ON, with the given
// probability for every judgment.
func askGateAgent(t *testing.T, judge *fakeAskJudge, tune func(*config.Config)) *AIAgent {
	t.Helper()
	a := newTestAgent(t, &mockStreamProvider{name: "mock"})
	a.RegisterTool(bashStub())
	// newTestAgent defaults to Skip (nobody to ask); the gate only exists where a
	// human can answer.
	a.SetPermissionMode(PermissionModeTUI)
	cfg := config.DefaultConfig()
	cfg.Jev = config.JevConfig{Enabled: true, APIKey: "test-key"}
	cfg.Permissions.Bash.Jev = config.BashJevAuto
	cfg.Permissions.Bash.JevThreshold = 0.70
	if tune != nil {
		tune(cfg)
	}
	a.Config.FullConfig = cfg
	a.SetAskJudgeForTest(judge)
	return a
}

// TestAskGateJudgesUnmatchedCommands is the whole point of the gate: a command
// no rule covers is judged instead of being run silently. `git push origin
// +main` is the real case — a force push that `git push --force *` does not
// match, so today it just runs.
func TestAskGateJudgesUnmatchedCommands(t *testing.T) {
	judge := &fakeAskJudge{p: 0.94}
	a := askGateAgent(t, judge, nil)
	// No policy at all: the installation with no tripwires.
	v := a.judgeBashAsk(t.Context(), "git push origin +main", "")
	require.True(t, v.judged, "an unmatched command must still be judged")
	assert.True(t, v.need, "a force push should be raised to the human")
	assert.Equal(t, 1, judge.callCount())
}

// TestAskGateOverridesAnAskRule: with the gate on, a rule-matched command is
// judged too — a verdict of "run" lets it through. That is what "auto approval"
// means, and it is the one behaviour a user has to know about (the design doc
// states it; the config comment points at it).
func TestAskGateOverridesAnAskRule(t *testing.T) {
	judge := &fakeAskJudge{p: 0.10}
	a := askGateAgent(t, judge, nil)
	p := permission.NewPolicy(permission.Rules{Ask: []string{"git push*"}}, permission.Rules{})
	a.SetPermissionPolicy(p)

	v := a.judgeBashAsk(t.Context(), "git push origin main", "git push*")
	require.True(t, v.judged)
	assert.False(t, v.need, "the judgment said this one is routine")
}

// TestAskGateFallsBackToTheRules: no answer (failure, timeout, unanswered) must
// leave this command's behaviour EXACTLY as it is today — a rule-matched one
// asks, an unmatched one runs. That is what keeps a service outage from changing
// anything.
func TestAskGateFallsBackToTheRules(t *testing.T) {
	for _, tc := range []struct {
		name  string
		judge *fakeAskJudge
	}{
		{"service failure", &fakeAskJudge{err: errors.New("boom")}},
		{"question left unanswered", &fakeAskJudge{unanswered: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := askGateAgent(t, tc.judge, nil)
			if v := a.judgeBashAsk(t.Context(), "go test ./...", ""); v.judged {
				t.Errorf("an answerless judgment must not be acted on: %+v", v)
			}
			if v := a.judgeBashAsk(t.Context(), "git push origin main", "git push*"); v.judged {
				t.Errorf("an answerless judgment must not be acted on: %+v", v)
			}
		})
	}
}

// TestAskGateNeedsSomeoneToAsk: in Skip mode (channel, -p, subagents, dream)
// there is nobody to approve anything, so the gate must not even be called —
// letting a probability approve would quietly widen what an unattended run may
// do.
func TestAskGateNeedsSomeoneToAsk(t *testing.T) {
	judge := &fakeAskJudge{p: 0.99}
	a := askGateAgent(t, judge, nil)
	a.SetPermissionMode(PermissionModeSkip)

	if v := a.judgeBashAsk(t.Context(), "rm -rf /tmp/x", ""); v.judged {
		t.Errorf("Skip mode must not be judged: %+v", v)
	}
	assert.Equal(t, 0, judge.callCount(), "the service must not even be called")
}

// TestAskGateOffIsTodaysBehaviour: with jev off (or the block unusable), nothing
// is judged and nothing is called.
func TestAskGateOffIsTodaysBehaviour(t *testing.T) {
	for _, tc := range []struct {
		name string
		tune func(*config.Config)
	}{
		{"jev: off", func(c *config.Config) { c.Permissions.Bash.Jev = config.BashJevOff }},
		{"block not configured", func(c *config.Config) { c.Jev = config.JevConfig{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			judge := &fakeAskJudge{p: 0.99}
			a := askGateAgent(t, judge, tc.tune)
			if v := a.judgeBashAsk(t.Context(), "git push origin +main", ""); v.judged {
				t.Errorf("the gate must be off: %+v", v)
			}
			assert.Equal(t, 0, judge.callCount())
		})
	}
}

// TestAskGateMemoizesTheSameCommand: a judgment costs ~600 input tokens and
// ~0.5s, and a build loop runs the same command text over and over within one
// session. The key is the command text, so nothing is inferred about a command
// that was not itself judged.
func TestAskGateMemoizesTheSameCommand(t *testing.T) {
	judge := &fakeAskJudge{p: 0.94}
	a := askGateAgent(t, judge, nil)

	first := a.judgeBashAsk(t.Context(), "go test ./...", "")
	second := a.judgeBashAsk(t.Context(), "go test ./...", "")
	assert.Equal(t, 1, judge.callCount(), "the same command must not be judged twice")
	assert.Equal(t, first, second)

	// A different command is a different judgment.
	a.judgeBashAsk(t.Context(), "go build ./...", "")
	assert.Equal(t, 2, judge.callCount())

	// The rule is part of the key: the same command can arrive with and without a
	// matched rule, and those are different inputs to the judgment.
	a.judgeBashAsk(t.Context(), "go test ./...", "go test*")
	assert.Equal(t, 3, judge.callCount())

	// And a new conversation forgets: the memo is session-scoped.
	a.ResetAskVerdicts()
	a.judgeBashAsk(t.Context(), "go test ./...", "")
	assert.Equal(t, 4, judge.callCount())
}

// TestAskGateShadowSetting: threshold 1.0 is the shadow setting — the gate judges
// (and logs) every command but can never act, because a noul answer is at most
// 1.0. It is how the distribution is watched on real traffic before anything
// changes.
func TestAskGateShadowSetting(t *testing.T) {
	judge := &fakeAskJudge{p: 0.99}
	a := askGateAgent(t, judge, func(c *config.Config) {
		c.Permissions.Bash.JevThreshold = 1.0
	})

	v := a.judgeBashAsk(t.Context(), "git push origin +main", "")
	require.True(t, v.judged, "shadow still judges")
	assert.False(t, v.need, "shadow must never act")
	assert.Equal(t, 1, judge.callCount())
	assert.Equal(t, 0.99, v.p, "the probability is the whole signal in shadow mode")
}

// TestAskGateThresholdIsReadAtReadTime: the memo stores the probability, not the
// verdict, so editing the threshold re-decides commands already judged instead of
// leaving a session frozen against an old number.
func TestAskGateThresholdIsReadAtReadTime(t *testing.T) {
	judge := &fakeAskJudge{p: 0.80}
	a := askGateAgent(t, judge, func(c *config.Config) {
		c.Permissions.Bash.JevThreshold = 0.90
	})

	require.False(t, a.judgeBashAsk(t.Context(), "npm ci", "").need, "0.80 < τ=0.90")
	a.Config.FullConfig.Permissions.Bash.JevThreshold = 0.50
	assert.True(t, a.judgeBashAsk(t.Context(), "npm ci", "").need, "0.80 >= τ=0.50, same memo")
	assert.Equal(t, 1, judge.callCount(), "re-deciding must not re-ask the service")
}

// TestAskGateBudgetBoundsTheWait: the gate sits in front of every command, so a
// slow service must not hold a turn. A client that ignores the deadline is what
// this pins — the deadline is what the caller relies on.
func TestAskGateBudgetBoundsTheWait(t *testing.T) {
	a := askGateAgent(t, &fakeAskJudge{p: 0.9}, func(c *config.Config) {
		c.Permissions.Bash.JevBudget = 20 * time.Millisecond
	})
	slow := &blockingJudge{done: make(chan struct{})}
	a.SetAskJudgeForTest(slow)

	start := time.Now()
	if v := a.judgeBashAsk(t.Context(), "go test ./...", ""); v.judged {
		t.Errorf("a timed-out judgment must not be acted on: %+v", v)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("the gate waited %v — the budget did not bound it", elapsed)
	}
	close(slow.done)
}

// blockingJudge waits until its deadline (or done) instead of answering.
type blockingJudge struct{ done chan struct{} }

func (b *blockingJudge) Evaluate(ctx context.Context, _ any, _ map[string]jev.Question) (*jev.Response, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-b.done:
		return nil, errors.New("released")
	}
}

func (b *blockingJudge) Model() string { return "blocking" }

// TestAskNoteLine: the pair, never a bare number, and the number only when a
// judgment produced one — an approval a RULE raised has no probability, and a
// zero there would read as "the judgment was sure".
func TestAskNoteLine(t *testing.T) {
	var absent *askNote
	if got := absent.Line(); got != "" {
		t.Errorf("a nil note renders nothing, got %q", got)
	}
	judged := &askNote{By: noteByJudgment, Score: 0.1836, Threshold: 0.72, HasScore: true}
	if got, want := judged.Line(), "[permission] decided_by=judgment score=0.184 threshold=0.720"; got != want {
		t.Errorf("line = %q, want %q", got, want)
	}
	approved := &askNote{By: noteByApproval}
	if got, want := approved.Line(), "[permission] decided_by=approval"; got != want {
		t.Errorf("a rule-raised approval carries no score: got %q, want %q", got, want)
	}
	approvedJudged := &askNote{By: noteByApproval, Score: 0.91, Threshold: 0.72, HasScore: true}
	if got, want := approvedJudged.Line(), "[permission] decided_by=approval score=0.910 threshold=0.720"; got != want {
		t.Errorf("line = %q, want %q", got, want)
	}
}

// TestCheckBashPermissionNote covers which of the four paths produces which note:
// judged-and-ran, judged-and-approved, rule-asked-and-approved, and deny (which
// runs nothing, so it carries no note — its error already names the rule).
func TestCheckBashPermissionNote(t *testing.T) {
	args := `{"command":"git push origin main"}`
	tc := llm.ToolCall{ID: "c1", Function: llm.ToolCallFunction{Name: "Bash", Arguments: args}}

	t.Run("judged and ran", func(t *testing.T) {
		judge := &fakeAskJudge{p: 0.18}
		a := askGateAgent(t, judge, nil)
		_, handled, note, err := a.checkBashPermission(t.Context(), tc, make(chan AgentEvent, 4))
		require.NoError(t, err)
		require.False(t, handled)
		require.NotNil(t, note)
		assert.Equal(t, noteByJudgment, note.By)
		assert.True(t, note.HasScore)
		assert.InDelta(t, 0.18, note.Score, 0.001)
	})

	t.Run("judged, asked, approved", func(t *testing.T) {
		judge := &fakeAskJudge{p: 0.94}
		a := askGateAgent(t, judge, nil)
		// Answered before the call: ConfirmResp is buffered with capacity 1, so the
		// parked ask picks the decision up as soon as it waits.
		a.ConfirmTool(ConfirmAllowOnce)
		_, handled, note, err := a.checkBashPermission(t.Context(), tc, make(chan AgentEvent, 4))
		require.NoError(t, err)
		require.False(t, handled, "an approved ask lets the command run")
		require.NotNil(t, note)
		assert.Equal(t, noteByApproval, note.By)
		assert.True(t, note.HasScore, "the judgment's number is why the human was asked")
	})

	t.Run("rule asked, approved, no judgment", func(t *testing.T) {
		judge := &fakeAskJudge{p: 0.94}
		a := askGateAgent(t, judge, func(c *config.Config) {
			c.Permissions.Bash.Jev = config.BashJevOff
		})
		a.SetPermissionPolicy(permission.NewPolicy(
			permission.Rules{Ask: []string{"git push*"}}, permission.Rules{}))
		a.ConfirmTool(ConfirmAllowOnce)
		_, handled, note, err := a.checkBashPermission(t.Context(), tc, make(chan AgentEvent, 4))
		require.NoError(t, err)
		require.False(t, handled)
		require.NotNil(t, note)
		assert.Equal(t, noteByApproval, note.By)
		assert.False(t, note.HasScore, "no judgment answered, so there is no number")
		assert.Equal(t, 0, judge.callCount())
	})

	t.Run("deny carries no note", func(t *testing.T) {
		judge := &fakeAskJudge{p: 0.94}
		a := askGateAgent(t, judge, nil)
		a.SetPermissionPolicy(permission.NewPolicy(
			permission.Rules{Deny: []string{"git push*"}}, permission.Rules{}))
		res, handled, note, err := a.checkBashPermission(t.Context(), tc, make(chan AgentEvent, 4))
		require.NoError(t, err)
		require.True(t, handled)
		assert.Nil(t, note, "nothing ran, and the error already names the rule")
		assert.Contains(t, res.Err.Error(), "deny")
	})
}

// TestSessionAllowAllReachesTheJudgmentGate — 「本会话全部允许」 has to stop BOTH halves from asking.
//
// The policy's copy of the answer silences a matched ask rule, which is all a policy can do: the
// judgment gate asks about commands no rule covered, and it runs AFTER every rule has spoken, so a
// switch the rules own cannot reach it. The button therefore promised 「本会话全部允许」 while the
// user kept answering cards — the ones the gate raised. This pins the half that was missing,
// including that the gate is not merely filtered but never consulted: a verdict nobody can act on
// is not worth a call (each one is ~600 input tokens), and the card it would raise is the one the
// user just dismissed.
func TestSessionAllowAllReachesTheJudgmentGate(t *testing.T) {
	tc := llm.ToolCall{ID: "c1", Function: llm.ToolCallFunction{
		Name: "Bash", Arguments: `{"command":"git push origin +main"}`,
	}}

	t.Run("judgment ask", func(t *testing.T) {
		judge := &fakeAskJudge{p: 0.94} // over this agent's 0.70 threshold
		a := askGateAgent(t, judge, nil)
		a.AllowAllAsksForSession()
		// Answered before the call, so a REGRESSION fails on the assertions below instead of
		// parking the ask and hanging the run until the test timeout.
		a.ConfirmTool(ConfirmAllowOnce)

		_, handled, note, err := a.checkBashPermission(t.Context(), tc, make(chan AgentEvent, 4))
		require.NoError(t, err)
		require.False(t, handled)
		assert.Nil(t, note, "no ask was raised, so there is no decision to report")
		assert.Equal(t, 0, judge.callCount(), "the gate must not even be asked")
	})

	t.Run("unparseable command", func(t *testing.T) {
		// The conservative ask (`AskNoRule`) is covered too — it is a policy ask like any other, and
		// every unattended frontend approves it as well (PermissionModeSkip's own branch approves a
		// policy ask before it special-cases the sentinel). Worth pinning because
		// it is the one ask where NOTHING could be evaluated: the command did not parse, so no rule —
		// deny included — sees any part of it, and the user's blanket answer is the only thing
		// carrying the command. The population is benign in practice: an unclosed quote, or the
		// trailing `&&` — both refused by bash itself.
		bad := llm.ToolCall{ID: "c2", Function: llm.ToolCallFunction{
			Name: "Bash", Arguments: `{"command":"echo \"unclosed"}`,
		}}
		judge := &fakeAskJudge{p: 0.94}
		a := askGateAgent(t, judge, nil)
		a.SetPermissionPolicy(permission.NewPolicy(
			permission.Rules{Ask: []string{"git push*"}}, permission.Rules{}))
		a.AllowAllAsksForSession()
		a.ConfirmTool(ConfirmAllowOnce) // see above: fail fast, never park

		_, handled, _, err := a.checkBashPermission(t.Context(), bad, make(chan AgentEvent, 4))
		require.NoError(t, err)
		require.False(t, handled, "an approved session does not ask about a command it cannot parse")
		assert.Equal(t, 0, judge.callCount())
	})
}

// The approval is state of the SESSION on both halves: a new conversation asks again — for the
// rules (the policy's copy) and for the gates (the agent's). Without the second reset, 「/new」
// would keep running every risky command unjudged in a conversation that never gave that answer.
func TestClearSessionDropsTheGateApproval(t *testing.T) {
	tc := llm.ToolCall{ID: "c1", Function: llm.ToolCallFunction{
		Name: "Bash", Arguments: `{"command":"git push origin +main"}`,
	}}
	judge := &fakeAskJudge{p: 0.94}
	a := askGateAgent(t, judge, nil)
	a.AllowAllAsksForSession()
	a.ClearSession()

	// Answered before the call: the ask parks, and ConfirmResp is buffered with capacity 1, so it
	// picks the decision up as soon as it waits.
	a.ConfirmTool(ConfirmAllowOnce)
	_, handled, note, err := a.checkBashPermission(t.Context(), tc, make(chan AgentEvent, 4))
	require.NoError(t, err)
	require.False(t, handled)
	require.NotNil(t, note)
	assert.Equal(t, noteByApproval, note.By)
	assert.Equal(t, 1, judge.callCount(), "the next conversation must judge — and ask — again")
}

// TestBashResultDoesNotCarryTheNoteToTheModel: the decision line is for the human
// reading the transcript and for NOBODY else. In the model's message it would be
// re-sent in the history of every later request and would read as a standing
// permission for commands like it — so the message must be exactly what it was
// before the gate existed.
func TestBashResultDoesNotCarryTheNoteToTheModel(t *testing.T) {
	judge := &fakeAskJudge{p: 0.18}
	a := askGateAgent(t, judge, nil)
	tc := llm.ToolCall{ID: "c1", Function: llm.ToolCallFunction{Name: "Bash", Arguments: `{"command":"go test ./..."}`}}
	ch := make(chan AgentEvent, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range ch {
		}
	}()

	msgs, err := a.executeToolCallsSequential(t.Context(), &RunState{}, []llm.ToolCall{tc}, ch)
	close(ch)
	<-done
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Contains(t, msgs[0].Content, "ran: go test ./...")
	assert.NotContains(t, msgs[0].Content, "[permission]",
		"the model must not be told how the call was decided")
	assert.NotContains(t, msgs[0].Content, "0.180")
}

// TestBashResultWithoutTheGateCarriesNothing: with the gate off, a Bash result is
// byte-for-byte what it was before this feature — no line, no tokens, no change
// in the recorded history.
func TestBashResultWithoutTheGateCarriesNothing(t *testing.T) {
	a := askGateAgent(t, &fakeAskJudge{p: 0.9}, func(c *config.Config) {
		c.Permissions.Bash.Jev = config.BashJevOff
	})
	tc := llm.ToolCall{ID: "c1", Function: llm.ToolCallFunction{Name: "Bash", Arguments: `{"command":"go test ./..."}`}}
	ch := make(chan AgentEvent, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range ch {
		}
	}()

	msgs, err := a.executeToolCallsSequential(t.Context(), &RunState{}, []llm.ToolCall{tc}, ch)
	close(ch)
	<-done
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.NotContains(t, msgs[0].Content, "[permission]")
}

// TestBashResultEventCarriesTheNote: the transcript IS the reader. The event is
// what the desktop renders (and the message is wrapped for the model), so the
// note has to be here — assertively, in the unwrapped copy.
func TestBashResultEventCarriesTheNote(t *testing.T) {
	judge := &fakeAskJudge{p: 0.18}
	a := askGateAgent(t, judge, nil)
	tc := llm.ToolCall{ID: "c1", Function: llm.ToolCallFunction{Name: "Bash", Arguments: `{"command":"go test ./..."}`}}
	ch := make(chan AgentEvent, 64)

	_, err := a.executeToolCallsSequential(t.Context(), &RunState{}, []llm.ToolCall{tc}, ch)
	require.NoError(t, err)
	close(ch)

	var results []string
	for e := range ch {
		if e.Type == AgentEventToolResult {
			results = append(results, e.ToolResult)
		}
	}
	require.Len(t, results, 1, "one tool result event per call")
	assert.Contains(t, results[0], "ran: go test ./...")
	assert.Contains(t, results[0], "[permission] decided_by=judgment score=0.180 threshold=0.700")
	assert.NotContains(t, results[0], "BEGIN TOOL OUTPUT",
		"the event is what the transcript shows — the untrusted-content wrapper is for the model")
}

// An allow rule the user wrote is the ONE thing that keeps this gate off a
// command, and it is meant to: the rule is the user's own answer about that
// shape, while the gate's only outcomes are "run" and "ask" — so consulting it
// could only ever undo the rule. Nothing is judged, nobody is asked, and no
// `[permission]` note is written, i.e. the command behaves exactly as it did
// before a judgment gate existed (the same reading 「本会话全部允许」 gets).
//
// The matched allow rule is also what makes this different from a command that
// matched NOTHING: that one is judged, and this test's whole point is the pair.
func TestAskGateSkipsAnAllowRule(t *testing.T) {
	judge := &fakeAskJudge{p: 0.99}
	a := askGateAgent(t, judge, nil)
	a.SetPermissionPolicy(permission.NewPolicy(
		permission.Rules{Allow: []string{"ssh *"}}, permission.Rules{}))
	a.SetPermissionMode(PermissionModeExternal)
	cards := 0
	a.SetPermissionHandler(func(context.Context, string, string, string, string) (bool, error) {
		cards++
		return true, nil
	})

	tc := llm.ToolCall{ID: "c1", Function: llm.ToolCallFunction{
		Name: "Bash", Arguments: `{"command":"ssh ugreen 'ls /vol1'"}`}}

	_, handled, note, err := a.checkBashPermission(t.Context(), tc, make(chan AgentEvent, 4))
	require.NoError(t, err)
	assert.False(t, handled, "an allow rule lets the call run")
	assert.Equal(t, 0, judge.callCount(), "the judgment is not consulted for a rule the user wrote")
	assert.Equal(t, 0, cards, "and nothing is put to the human either")
	assert.Nil(t, note, "no decision happened beyond the rules, so there is nothing to note")

	// Control: the same command with no allow rule IS judged — otherwise this
	// test would pass for a gate that had simply stopped working.
	b := askGateAgent(t, judge, nil)
	b.SetPermissionMode(PermissionModeExternal)
	b.SetPermissionHandler(func(context.Context, string, string, string, string) (bool, error) { return true, nil })
	_, _, note, err = b.checkBashPermission(t.Context(), tc, make(chan AgentEvent, 4))
	require.NoError(t, err)
	assert.Equal(t, 1, judge.callCount(), "an unmatched command is still judged")
	require.NotNil(t, note)
	assert.Equal(t, noteByApproval, note.By)
}
