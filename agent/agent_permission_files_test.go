package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/agent/permission"
	"github.com/monsterxx03/tachi/agent/permission/jevfiles"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testPathRules builds one file tool's rules from a deny/ask/allow triple of
// patterns — the shape a config file has, through the same expansion the agent
// applies to a real one. These patterns are absolute and free of `~`, so the
// expansion has no anchor to apply.
func testPathRules(deny, ask, allow []string) permission.PathRules {
	return permission.ExpandPathRules(permission.Rules{Deny: deny, Ask: ask, Allow: allow}, "", "")
}

// fileGateAgent builds an agent whose FILE judgment gate is on (the bash one is
// left as the config default), with a stub for each of the three file tools.
func fileGateAgent(t *testing.T, judge *fakeAskJudge, tune func(*config.Config)) *AIAgent {
	t.Helper()
	a := newTestAgent(t, &mockStreamProvider{name: "mock"})
	for _, name := range []string{tools.ToolNameRead, tools.ToolNameWrite, tools.ToolNameEdit} {
		n := name
		a.RegisterTool(&stubTool{
			name:     n,
			parallel: n != tools.ToolNameWrite,
			executeFn: func(context.Context, string) (string, error) {
				return n + ":done", nil
			},
		})
	}
	// newTestAgent defaults to Skip (nobody to ask); the gate only exists where a
	// human can answer.
	a.SetPermissionMode(PermissionModeTUI)
	cfg := config.DefaultConfig()
	cfg.Jev = config.JevConfig{Enabled: true, APIKey: "test-key"}
	cfg.Permissions.Files.Jev = config.FilesJevAuto
	cfg.Permissions.Files.JevThreshold = 0.70
	if tune != nil {
		tune(cfg)
	}
	a.Config.FullConfig = cfg
	a.SetAskJudgeForTest(judge)
	return a
}

// fileCall builds the tool call the gate sees, with the field each tool uses.
func newFileCall(t *testing.T, tool, path, payload string) llm.ToolCall {
	t.Helper()
	var args string
	switch tool {
	case tools.ToolNameWrite:
		args = fmt.Sprintf(`{"path":%q,"content":%q}`, path, payload)
	case tools.ToolNameEdit:
		args = fmt.Sprintf(`{"path":%q,"old_string":%q,"new_string":%q}`, path, payload, payload+"-new")
	default:
		args = fmt.Sprintf(`{"path":%q}`, path)
	}
	return llm.ToolCall{ID: "c1", Function: llm.ToolCallFunction{Name: tool, Arguments: args}}
}

// callForCheck is the whole gate for one call, as the executor drives it.
func callForCheck(t *testing.T, a *AIAgent, tc llm.ToolCall) (tools.ToolResult, bool, *askNote, error) {
	t.Helper()
	return a.checkFilePermission(t.Context(), tc, make(chan AgentEvent, 8))
}

// TestFileToolNameSpellings pins the two spellings of one tool name together:
// jevfiles' Tool constants are what a state the sweep measured was rendered with,
// and agent/tools' ToolName* constants are what a call is dispatched by. A rename
// on either side would otherwise stop the mapping silently — every file call would
// look like "not a file tool" and no rule would ever fire.
func TestFileToolNameSpellings(t *testing.T) {
	pairs := []struct {
		toolName string
		jevTool  jevfiles.Tool
	}{
		{tools.ToolNameRead, jevfiles.ToolRead},
		{tools.ToolNameWrite, jevfiles.ToolWrite},
		{tools.ToolNameEdit, jevfiles.ToolEdit},
	}
	for _, p := range pairs {
		assert.Equal(t, p.toolName, string(p.jevTool))
		permTool, jevTool, ok := fileToolNames(p.toolName)
		require.True(t, ok, "%s must be a file tool", p.toolName)
		assert.Equal(t, p.jevTool, jevTool)
		assert.Contains(t, []permission.FileTool{
			permission.FileToolRead, permission.FileToolWrite, permission.FileToolEdit,
		}, permTool)
	}
	// A tool that is not one of the three never reaches a file rule check.
	if _, _, ok := fileToolNames(tools.ToolNameBash); ok {
		t.Fatal("Bash is not a file tool")
	}
}

// TestFileGateJudgesUnmatchedCalls is the point of the gate: a call no rule covers
// is judged instead of happening silently. Reading a key is the real case — no
// rule is watching it today, and it hands a credential to the transcript.
//
// The judgment is asked about the facts the gate assembles, with no policy at all:
// the installation with no tripwires.
func TestFileGateJudgesUnmatchedCalls(t *testing.T) {
	judge := &fakeAskJudge{p: 0.94}
	a := fileGateAgent(t, judge, nil)
	const keyPath = "/Users/someone/.ssh/id_rsa"
	_, canonical := filePaths(t.Context(), keyPath)
	call := a.fileCallFacts(t.Context(), jevfiles.ToolRead, canonical)

	v := a.judgeFileAsk(t.Context(), call)
	require.True(t, v.judged, "an unmatched call must still be judged")
	assert.True(t, v.need, "reading a private key should be raised to the human")
	assert.Equal(t, 1, judge.callCount())
}

// TestFileGateRunsWhatTheJudgmentReadAsRoutine: the other verdict — a call inside
// the tree is not raised, and the note says a judgment decided it.
func TestFileGateRunsWhatTheJudgmentReadAsRoutine(t *testing.T) {
	judge := &fakeAskJudge{p: 0.05}
	a := fileGateAgent(t, judge, nil)
	_, handled, note, err := callForCheck(t, a, newFileCall(t, tools.ToolNameEdit, "/repo/src/main.go", "func x()"))
	require.NoError(t, err)
	assert.False(t, handled, "a routine call runs")
	require.NotNil(t, note)
	assert.Equal(t, noteByJudgment, note.By)
	assert.Equal(t, 0.05, note.Score)
}

// TestFileGateStopsAskingAfterSessionAllowAll: 「本会话全部允许」 covers the FILE half's judgment
// too, and by the same rule — the approval is read after deny and before the judgment, so the gate
// is never consulted (the call is not even judged, let alone raised). Reading a key is the case a
// user is most likely to have just waved through, and it is the one that would otherwise keep
// producing cards in a session that already said stop.
func TestFileGateStopsAskingAfterSessionAllowAll(t *testing.T) {
	judge := &fakeAskJudge{p: 0.94} // over this agent's 0.70 threshold
	a := fileGateAgent(t, judge, nil)
	a.AllowAllAsksForSession()
	// Answered before the call, so a REGRESSION fails on the assertions below instead of parking
	// the ask and hanging the run until the test timeout.
	a.ConfirmTool(ConfirmAllowOnce)

	_, handled, note, err := callForCheck(t, a, newFileCall(t, tools.ToolNameRead, "/Users/someone/.ssh/id_rsa", ""))
	require.NoError(t, err)
	assert.False(t, handled, "an approved session runs the call")
	assert.Nil(t, note, "no ask was raised, so there is no decision to report")
	assert.Equal(t, 0, judge.callCount(), "the gate must not even be asked")
}

// TestFileGateOffIsTodaysBehaviour: with jev off (or the block unusable), nothing
// is judged and the service is never called — the state of the world before this
// feature existed.
func TestFileGateOffIsTodaysBehaviour(t *testing.T) {
	for _, tc := range []struct {
		name string
		tune func(*config.Config)
	}{
		{"jev: off", func(c *config.Config) { c.Permissions.Files.Jev = config.FilesJevOff }},
		{"block not configured", func(c *config.Config) { c.Jev = config.JevConfig{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			judge := &fakeAskJudge{p: 0.99}
			a := fileGateAgent(t, judge, tc.tune)
			_, handled, note, err := callForCheck(t, a, newFileCall(t, tools.ToolNameWrite, "/etc/hosts", "x"))
			require.NoError(t, err)
			assert.False(t, handled)
			assert.Nil(t, note, "no judgment and no ask means no note at all")
			assert.Equal(t, 0, judge.callCount())
		})
	}
}

// TestFileGateNeedsSomeoneToAsk: in Skip mode (channel, -p, subagents, dream)
// there is nobody to approve a file access, so the gate must not even be called.
func TestFileGateNeedsSomeoneToAsk(t *testing.T) {
	judge := &fakeAskJudge{p: 0.99}
	a := fileGateAgent(t, judge, nil)
	a.SetPermissionMode(PermissionModeSkip)

	_, handled, _, err := callForCheck(t, a, newFileCall(t, tools.ToolNameRead, "/etc/shadow", ""))
	require.NoError(t, err)
	assert.False(t, handled)
	assert.Equal(t, 0, judge.callCount(), "the service must not even be called")
}

// TestFileGateFallsBackToTheRules: no answer (failure, budget, unanswered) must
// leave this call's behaviour EXACTLY as it is today — a rule-matched one asks, an
// unmatched one runs. That is what keeps an outage from changing anything.
func TestFileGateFallsBackToTheRules(t *testing.T) {
	for _, tc := range []struct {
		name  string
		judge *fakeAskJudge
	}{
		{"service failure", &fakeAskJudge{err: errors.New("boom")}},
		{"question left unanswered", &fakeAskJudge{unanswered: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := fileGateAgent(t, tc.judge, nil)
			// Nothing matched: it runs, exactly as it would with the gate off.
			_, handled, note, err := callForCheck(t, a, newFileCall(t, tools.ToolNameRead, "/repo/main.go", ""))
			require.NoError(t, err)
			assert.False(t, handled)
			assert.Nil(t, note)

			// A matched ask rule still asks — but with nobody able to answer here
			// (Skip) the ask is approved by the mode's own branch, so the gate hands
			// the call back to the executor (handled=false).
			a.SetPermissionMode(PermissionModeSkip)
			a.SetPermissionPolicy(permission.NewPolicy(permission.Rules{}, permission.Rules{},
				permission.WithFileRules(permission.FileRules{
					Read: testPathRules(nil, []string{"/repo/secrets/*"}, nil),
				}, permission.FileRules{})))
			_, handled, note, err = callForCheck(t, a, newFileCall(t, tools.ToolNameRead, "/repo/secrets/token", ""))
			require.NoError(t, err)
			assert.False(t, handled, "an unattended ask is approved, so the call proceeds")
			assert.Nil(t, note, "approved without an answer to report: no note")
		})
	}
}

// TestFileGateMemoizesTheSameCall: a judgment costs a round trip and money, and a
// session reads the same file over and over. The key is the tool, the canonical
// path and the matched rule — nothing is inferred about a path that was not itself
// judged.
func TestFileGateMemoizesTheSameCall(t *testing.T) {
	judge := &fakeAskJudge{p: 0.94}
	a := fileGateAgent(t, judge, nil)
	call := newFileCall(t, tools.ToolNameRead, "/repo/main.go", "")

	// p=0.94 is above the threshold, so each judged call raises the ask — the memo
	// saves the JUDGMENT, not the question. Every call here is answered.
	for range 2 {
		a.ConfirmTool(ConfirmAllowOnce)
		callForCheck(t, a, call)
	}
	assert.Equal(t, 1, judge.callCount(), "the same call must not be judged twice")

	// A different path, a different tool and a different rule are all different
	// judgments.
	a.ConfirmTool(ConfirmAllowOnce)
	callForCheck(t, a, newFileCall(t, tools.ToolNameRead, "/repo/other.go", ""))
	a.ConfirmTool(ConfirmAllowOnce)
	callForCheck(t, a, newFileCall(t, tools.ToolNameWrite, "/repo/main.go", "x"))
	assert.Equal(t, 3, judge.callCount())
	a.SetPermissionPolicy(permission.NewPolicy(permission.Rules{}, permission.Rules{},
		permission.WithFileRules(permission.FileRules{
			Read: testPathRules(nil, []string{"/repo/main.go"}, nil),
		}, permission.FileRules{})))
	a.ConfirmTool(ConfirmAllowOnce)
	callForCheck(t, a, call)
	assert.Equal(t, 4, judge.callCount(), "the matched rule is part of the key")

	// And a new conversation forgets: the memo is session-scoped.
	a.ResetAskVerdicts()
	a.ConfirmTool(ConfirmAllowOnce)
	callForCheck(t, a, call)
	assert.Equal(t, 5, judge.callCount())
}

// TestFileGateShadowSetting: threshold 1.0 is the shadow setting — the gate judges
// (and logs) every call but can never act, because a noul answer is at most 1.0.
// It is how the distribution is watched on real traffic before anything changes,
// and it is the shipped default until the case set has been swept.
func TestFileGateShadowSetting(t *testing.T) {
	judge := &fakeAskJudge{p: 0.99}
	a := fileGateAgent(t, judge, func(c *config.Config) {
		c.Permissions.Files.JevThreshold = config.FilesJevShadowThreshold
	})

	_, handled, note, err := callForCheck(t, a, newFileCall(t, tools.ToolNameWrite, "/etc/hosts", "x"))
	require.NoError(t, err)
	assert.False(t, handled, "shadow must never act")
	require.NotNil(t, note)
	assert.Equal(t, noteByJudgment, note.By, "the judgment still ran and is recorded")
	assert.Equal(t, 0.99, note.Score, "the probability is the whole signal in shadow mode")
	assert.Equal(t, 1, judge.callCount())
}

// TestFileGateThresholdIsReadAtReadTime: the memo stores the probability, not the
// verdict, so editing the threshold re-decides calls already judged instead of
// leaving a session frozen against an old number.
func TestFileGateThresholdIsReadAtReadTime(t *testing.T) {
	judge := &fakeAskJudge{p: 0.80}
	a := fileGateAgent(t, judge, func(c *config.Config) {
		c.Permissions.Files.JevThreshold = 0.90
	})

	_, handled, _, err := callForCheck(t, a, newFileCall(t, tools.ToolNameRead, "/repo/main.go", ""))
	require.NoError(t, err)
	require.False(t, handled, "0.80 < τ=0.90")

	a.Config.FullConfig.Permissions.Files.JevThreshold = 0.50
	a.ConfirmTool(ConfirmAllowOnce)
	_, handled, note, err := callForCheck(t, a, newFileCall(t, tools.ToolNameRead, "/repo/main.go", ""))
	require.NoError(t, err)
	assert.False(t, handled, "0.80 >= τ=0.50 from the same memo entry: the human is asked, and approved")
	require.NotNil(t, note)
	assert.Equal(t, noteByApproval, note.By)
	assert.Equal(t, 1, judge.callCount(), "re-deciding must not re-ask the service")
}

// TestFileGateBudgetBoundsTheWait: the gate sits in front of calls that happen
// several times a turn, so a slow service must not hold the turn. A client that
// ignores the deadline is what this pins.
func TestFileGateBudgetBoundsTheWait(t *testing.T) {
	a := fileGateAgent(t, &fakeAskJudge{p: 0.9}, func(c *config.Config) {
		c.Permissions.Files.JevBudget = 20 * time.Millisecond
	})
	a.SetAskJudgeForTest(&blockingJudge{done: make(chan struct{})})

	start := time.Now()
	_, handled, _, err := callForCheck(t, a, newFileCall(t, tools.ToolNameRead, "/repo/main.go", ""))
	require.NoError(t, err)
	assert.False(t, handled, "a timed-out judgment must not be acted on")
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the gate waited %v — the budget did not bound it", elapsed)
	}
}

// TestFileRuleDenyBlocksBeforeTheJudge: a deny rule is the user's hard constraint.
// Nothing runs, the service is not consulted, and the error names the rule — the
// judgment physically cannot reach a denied call.
func TestFileRuleDenyBlocksBeforeTheJudge(t *testing.T) {
	judge := &fakeAskJudge{p: 0.01}
	a := fileGateAgent(t, judge, nil)
	a.SetPermissionPolicy(permission.NewPolicy(permission.Rules{}, permission.Rules{},
		permission.WithFileRules(permission.FileRules{
			Write: testPathRules([]string{"/etc/*"}, nil, nil),
		}, permission.FileRules{})))

	res, handled, note, err := callForCheck(t, a, newFileCall(t, tools.ToolNameWrite, "/etc/hosts", "x"))
	require.NoError(t, err)
	require.True(t, handled)
	assert.Nil(t, note, "nothing ran, and the error already names the rule")
	assert.Contains(t, res.Err.Error(), "deny")
	assert.Contains(t, res.Err.Error(), "/etc/*")
	assert.Equal(t, 0, judge.callCount(), "deny is decided above the gate")
}

// TestFileRuleDenyStopsTraversalAndSymlinks: the rules are matched against the
// canonical path, so neither `..` nor a symlink is a way around one.
func TestFileRuleDenyStopsTraversalAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	secretDir := filepath.Join(dir, "secrets")
	require.NoError(t, os.MkdirAll(secretDir, 0o755))
	secret := filepath.Join(secretDir, "token")
	require.NoError(t, os.WriteFile(secret, []byte("s3cret"), 0o600))
	link := filepath.Join(dir, "shortcut")
	require.NoError(t, os.Symlink(secretDir, link))

	a := fileGateAgent(t, &fakeAskJudge{p: 0.01}, nil)
	a.SetPermissionPolicy(permission.NewPolicy(permission.Rules{}, permission.Rules{},
		permission.WithFileRules(permission.FileRules{
			Read: testPathRules([]string{filepath.Join(dir, "secrets", "*")}, nil, nil),
		}, permission.FileRules{})))

	for _, arg := range []string{
		secret,
		filepath.Join(dir, "sub", "..", "secrets", "token"),
		filepath.Join(link, "token"),
	} {
		res, handled, _, err := callForCheck(t, a, newFileCall(t, tools.ToolNameRead, arg, ""))
		require.NoError(t, err)
		require.True(t, handled, "%s must be blocked", arg)
		assert.Contains(t, res.Err.Error(), "deny")
	}
}

// TestFileRefusalsDoNotInviteInvestigation: a refusal is shaped to STOP the model,
// not to explain the configuration to it. Naming a permission file would send a
// helpful model off to read it, and the fix is the user's either way — the bash
// refusals have the same rule (TestBashRefusalsDoNotInviteInvestigation).
//
// Two refusals are left on this half: a `deny` rule, and a human answering a card
// with "no" (the file tools' ask has no unattended refusal — an unattended mode
// approves it, see TestFileGateFallsBackToTheRules).
func TestFileRefusalsDoNotInviteInvestigation(t *testing.T) {
	// Shared shape: no config path is ever named, and the fact is stated.
	shared := func(t *testing.T, text string) {
		t.Helper()
		msg := strings.ToLower(text)
		for _, bad := range []string{"permissions.yaml", "permissions.bash", "permissions.files", "/.tachi/", "config.yaml"} {
			assert.NotContains(t, msg, strings.ToLower(bad), "a refusal must not name a configuration path")
		}
		assert.Contains(t, msg, "not executed")
	}

	t.Run("deny rule", func(t *testing.T) {
		a := fileGateAgent(t, &fakeAskJudge{p: 0.95}, nil)
		a.SetPermissionPolicy(permission.NewPolicy(permission.Rules{}, permission.Rules{},
			permission.WithFileRules(permission.FileRules{
				Read: testPathRules([]string{"/repo/secrets/*"}, nil, nil),
			}, permission.FileRules{})))

		res, handled, _, err := callForCheck(t, a, newFileCall(t, tools.ToolNameRead, "/repo/secrets/token", ""))
		require.NoError(t, err)
		require.True(t, handled)
		require.Error(t, res.Err)
		shared(t, res.Err.Error())
		assert.Contains(t, res.Err.Error(), "/repo/secrets/*", "the refusal names the rule that stopped it")
		assert.Contains(t, res.Err.Error(), "Only the user can change this rule")
		assert.Contains(t, res.Err.Error(), "do not attempt the same thing in another form")
	})

	t.Run("human refused", func(t *testing.T) {
		a := fileGateAgent(t, &fakeAskJudge{p: 0.95}, nil)
		a.SetPermissionMode(PermissionModeExternal)
		a.SetPermissionHandler(func(context.Context, string, string, string, string) (bool, error) {
			return false, nil
		})
		a.SetPermissionPolicy(permission.NewPolicy(permission.Rules{}, permission.Rules{},
			permission.WithFileRules(permission.FileRules{
				Read: testPathRules(nil, []string{"/repo/secrets/*"}, nil),
			}, permission.FileRules{})))

		res, handled, _, err := callForCheck(t, a, newFileCall(t, tools.ToolNameRead, "/repo/secrets/token", ""))
		require.NoError(t, err)
		require.True(t, handled)
		require.Error(t, res.Err)
		shared(t, res.Err.Error())
		assert.Contains(t, res.Err.Error(), "/repo/secrets/*", "the refusal names what the user was consulted about")
		assert.Contains(t, res.Err.Error(), "only they can change that", "the refusal says who can change it")
		assert.Contains(t, res.Err.Error(), "do not reach the same file through another path or another tool")
	})
}

// TestFileAskDeniedRefusesTheCallAndNotTheTurn: this is the one place the file ask
// deliberately differs from the bash ask. A refusal here is a tool error the model
// can act on (read another file, write it elsewhere), not an aborted turn.
func TestFileAskDeniedRefusesTheCallAndNotTheTurn(t *testing.T) {
	judge := &fakeAskJudge{p: 0.95}
	a := fileGateAgent(t, judge, nil)
	a.ConfirmTool(ConfirmDeny)

	res, handled, note, err := callForCheck(t, a, newFileCall(t, tools.ToolNameWrite, "/etc/hosts", "x"))
	require.NoError(t, err, "a refusal is not a cancelled turn")
	require.True(t, handled)
	assert.Nil(t, note)
	require.Error(t, res.Err)
	assert.NotErrorIs(t, res.Err, errCancelled)
	assert.Contains(t, res.Err.Error(), "no rule matched",
		"the judgment raised this one, and the refusal has to say so")
	assert.Contains(t, res.Err.Error(), "0.95")
}

// TestFileAskApprovedRuns: the human said yes, so the call proceeds and the note
// records the judgment's number — why they were asked at all.
func TestFileAskApprovedRuns(t *testing.T) {
	judge := &fakeAskJudge{p: 0.95}
	a := fileGateAgent(t, judge, nil)
	a.ConfirmTool(ConfirmAllowOnce)

	_, handled, note, err := callForCheck(t, a, newFileCall(t, tools.ToolNameRead, "/etc/shadow", ""))
	require.NoError(t, err)
	assert.False(t, handled, "an approved ask lets the call run")
	require.NotNil(t, note)
	assert.Equal(t, noteByApproval, note.By)
	assert.True(t, note.HasScore)
}

// TestFileAskAllowSessionSilencesRulesAndTheJudgmentButNeverDeny: 「本会话全部允许」 is ONE answer with TWO
// readers. The policy's copy stops a rule-matched call from asking; the agent's copy stops the judgment gate —
// and stops it BEFORE the judgment, so the gate is not consulted again (a verdict nobody can act on is not
// worth a call) and the call carries no note at all. Deny is the exception: it is decided before either reader
// is reached, so the switch can never widen into "write what the rules forbid".
func TestFileAskAllowSessionSilencesRulesAndTheJudgmentButNeverDeny(t *testing.T) {
	judge := &fakeAskJudge{p: 0.95}
	a := fileGateAgent(t, judge, nil)
	a.SetPermissionPolicy(permission.NewPolicy(permission.Rules{}, permission.Rules{},
		permission.WithFileRules(permission.FileRules{
			Read:  testPathRules(nil, []string{"/repo/secrets/*"}, nil),
			Write: testPathRules([]string{"/etc/*"}, nil, nil),
		}, permission.FileRules{})))
	call := newFileCall(t, tools.ToolNameRead, "/repo/secrets/token", "")

	// The human answers 「本会话全部允许」: the call runs, and the note says a human
	// approved it. The judgment is what raised the ask in the first place.
	a.ConfirmTool(ConfirmAllowSession)
	_, handled, note, err := callForCheck(t, a, call)
	require.NoError(t, err)
	assert.False(t, handled, "an approved ask lets the call run")
	require.NotNil(t, note)
	assert.Equal(t, noteByApproval, note.By)
	assert.Equal(t, 1, judge.callCount())

	// The same call again: the rule AND the gate are both answered for this session, so the call simply
	// runs and there is nothing to report. Pre-answered as elsewhere: a regression then fails on the
	// assertions below instead of parking the ask until the test timeout.
	a.ConfirmTool(ConfirmAllowOnce)
	_, handled, note, err = callForCheck(t, a, call)
	require.NoError(t, err)
	assert.False(t, handled)
	assert.Nil(t, note, "nothing asked, so there is no decision to note")
	assert.Equal(t, 1, judge.callCount(), "the gate must not be consulted again")

	// The deny rule survives the switch, on the same policy.
	res, handled, _, err := callForCheck(t, a, newFileCall(t, tools.ToolNameWrite, "/etc/hosts", "x"))
	require.NoError(t, err)
	require.True(t, handled)
	assert.Contains(t, res.Err.Error(), "deny")
}

// TestFileAskPreviewNamesTheRealCause: the two ways a human can be interrupted
// must render differently. Saying "matched ask rule" for a judgment-raised ask
// sends the reader looking for a rule they never wrote (issue #40).
func TestFileAskPreviewNamesTheRealCause(t *testing.T) {
	call := fileCall{facts: jevfiles.Call{Tool: jevfiles.ToolWrite, Path: "/etc/hosts"}, lexical: "/etc/hosts"}

	matched := askPreview("WriteFile /etc/hosts", askCause{rule: "/etc/*"})
	assert.Contains(t, matched, "WriteFile /etc/hosts")
	assert.Contains(t, matched, `命中 ask 规则: "/etc/*"`)

	judged := askPreview("WriteFile /etc/hosts", askCause{judged: true, score: 0.91, threshold: 0.7})
	assert.Contains(t, judged, "没有任何 ask 规则命中")
	assert.Contains(t, judged, "0.91")
	assert.NotContains(t, judged, "命中 ask 规则:")

	assert.Contains(t, fileAskDenied(askCause{rule: "/etc/*"}, call).Error(), `matched ask rule "/etc/*"`)
	assert.Contains(t, fileAskDenied(askCause{judged: true, score: 0.91, threshold: 0.7}, call).Error(),
		"no rule matched")
}

// TestFileCallFactsCarryTheCanonicalPathAndTheStat: the judgment is asked about
// the same path string the rules were matched against, plus what is actually on
// disk — a fact, never a verdict.
func TestFileCallFactsCarryTheCanonicalPathAndTheStat(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "main.go")
	require.NoError(t, os.WriteFile(target, []byte("package main\n"), 0o644))

	a := fileGateAgent(t, &fakeAskJudge{p: 0.5}, nil)
	_, traversalCanonical := filePaths(t.Context(), filepath.Join(dir, "sub", "..", "main.go"))
	facts := a.fileCallFacts(t.Context(), jevfiles.ToolRead, traversalCanonical)
	canonical, err := fileutil.CanonicalPath(target)
	require.NoError(t, err)
	assert.Equal(t, canonical, facts.Path, "traversal is resolved before anything sees the path")
	assert.True(t, facts.Exists)
	assert.Equal(t, int64(len("package main\n")), facts.Size)

	_, missingCanonical := filePaths(t.Context(), filepath.Join(dir, "new.go"))
	missing := a.fileCallFacts(t.Context(), jevfiles.ToolWrite, missingCanonical)
	assert.False(t, missing.Exists, "a write that creates a file says so")

	// A directory is named as one, and carries no byte count to misread.
	_, dirCanonical := filePaths(t.Context(), dir)
	asDir := a.fileCallFacts(t.Context(), jevfiles.ToolRead, dirCanonical)
	assert.True(t, asDir.Exists)
	assert.True(t, asDir.Dir)
}

// TestFileResultEventCarriesTheNoteInBothPaths: the transcript IS the reader, and
// Read/Edit are parallel tools while Write is not — so both executors have to
// carry the decision line out. A note lost on the parallel path would lose it for
// most file calls.
func TestFileResultEventCarriesTheNoteInBothPaths(t *testing.T) {
	for _, tc := range []struct {
		name  string
		call  llm.ToolCall
		paral bool
	}{
		{"sequential (WriteFile)", newFileCall(t, tools.ToolNameWrite, "/repo/main.go", "x"), false},
		{"parallel (ReadFile)", newFileCall(t, tools.ToolNameRead, "/repo/main.go", ""), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := fileGateAgent(t, &fakeAskJudge{p: 0.18}, nil)
			ch := make(chan AgentEvent, 64)
			done := make(chan []AgentEvent, 1)
			go func() {
				var got []AgentEvent
				for e := range ch {
					got = append(got, e)
				}
				done <- got
			}()

			var err error
			if tc.paral {
				_, err = a.executeToolCallsParallel(t.Context(), &RunState{}, []llm.ToolCall{tc.call}, ch)
			} else {
				_, err = a.executeToolCallsSequential(t.Context(), &RunState{}, []llm.ToolCall{tc.call}, ch)
			}
			close(ch)
			require.NoError(t, err)

			var results []string
			for _, e := range <-done {
				if e.Type == AgentEventToolResult {
					results = append(results, e.ToolResult)
				}
			}
			require.Len(t, results, 1, "one tool result event per call")
			assert.Contains(t, results[0], "[permission] decided_by=judgment score=0.180 threshold=0.700")
		})
	}
}

// TestFileResultDoesNotCarryTheNoteToTheModel: the decision line is for the human
// reading the transcript and for nobody else — in the model's message it would be
// re-sent in every later request and read as a standing permission for calls like
// it. Without a judgment and without an ask, the result is byte-for-byte what it
// was before this feature existed.
func TestFileResultDoesNotCarryTheNoteToTheModel(t *testing.T) {
	a := fileGateAgent(t, &fakeAskJudge{p: 0.18}, func(c *config.Config) {
		c.Permissions.Files.Jev = config.FilesJevOff
	})
	tc := newFileCall(t, tools.ToolNameWrite, "/repo/main.go", "x")
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
	assert.Contains(t, msgs[0].Content, "WriteFile:done")
	assert.NotContains(t, msgs[0].Content, "[permission]")
}

// TestFileAskNamesThePathTheCallerWrote: two paths, two readers. The state and the
// memo work on the CANONICAL path — the file's identity, which two aliases of one
// file agree on — while the ask and the refusal name the path the call actually
// used. A card showing `/private/var/...` for a call that said `./fixture.txt` is
// issue #40 wearing a different hat: the reader is shown something they never
// wrote.
func TestFileAskNamesThePathTheCallerWrote(t *testing.T) {
	dir := t.TempDir()
	// The alias is MADE here rather than borrowed from the platform. What this test needs is a
	// spelling whose canonical form differs, and the temp path only provides that on macOS — where
	// /tmp and /var are symlinks into /private, so the two strings differ by accident. On a runner
	// where /tmp is a real directory the temp path IS its own canonical form, and the NotContains
	// assertions below would be checking that a string differs from itself: green on the author's
	// machine, red on Linux, about nothing either way.
	real := filepath.Join(dir, "real")
	require.NoError(t, os.MkdirAll(real, 0o755))
	link := filepath.Join(dir, "link")
	require.NoError(t, os.Symlink(real, link))

	// Spelled with a traversal, so the lexical form is visibly the caller's spelling
	// resolved — and not the canonical one either (see the doc comment).
	spelled := link + "/sub/../fixture.txt"
	lexical, canonical := filePaths(t.Context(), spelled)
	require.Equal(t, filepath.Join(link, "fixture.txt"), lexical)
	require.NotEqual(t, lexical, canonical, "fixture: the two spellings must differ")

	a := fileGateAgent(t, &fakeAskJudge{p: 0.95}, nil)
	var preview string
	a.SetPermissionMode(PermissionModeExternal)
	a.SetPermissionHandler(func(_ context.Context, _ /*tool*/, _ /*id*/, diff, _ /*args*/ string) (bool, error) {
		preview = diff
		return false, nil
	})
	// The rule names the RESOLVED directory, which is the one the file is really in: a rule's
	// literal prefix is canonicalised, and `*` does not cross a separator, so a prefix above the
	// link would sit one directory too high to match anything.
	a.SetPermissionPolicy(permission.NewPolicy(permission.Rules{}, permission.Rules{},
		permission.WithFileRules(permission.FileRules{
			Read: testPathRules(nil, []string{filepath.Join(real, "*")}, nil),
		}, permission.FileRules{})))

	res, handled, _, err := callForCheck(t, a, newFileCall(t, tools.ToolNameRead, spelled, ""))
	require.NoError(t, err)
	require.True(t, handled, "the handler refused, so this call is refused")

	assert.Contains(t, preview, lexical, "the ask names the path the caller wrote")
	assert.NotContains(t, preview, canonical, "the canonical path is internal")
	assert.Contains(t, res.Err.Error(), lexical, "the refusal names the same path")
	assert.NotContains(t, res.Err.Error(), canonical)

	// The state the judgment was asked about is the canonical one, and so is the
	// memo key: the same file reached by another alias is the same file.

	facts := a.fileCallFacts(t.Context(), jevfiles.ToolRead, canonical)
	assert.Equal(t, canonical, facts.Path)
	assert.Contains(t, jevfiles.State(facts), canonical)
}

// TestFileGateSerializesTwoParallelAsks: ReadFile is a PARALLEL tool, so one
// batch can park TWO cards at the same time while the agent owns ONE answer
// channel (ConfirmResp, capacity 1). That is what the ask path's askMu is for —
// without it the two parked cards would race for a single keystroke. This drives
// the real batch entry point and answers each card as it appears, which is what a
// frontend does.
func TestFileGateSerializesTwoParallelAsks(t *testing.T) {
	a := fileGateAgent(t, &fakeAskJudge{p: 0.95}, nil)
	ch := make(chan AgentEvent, 64)
	go func() {
		for e := range ch {
			if e.Type == AgentEventToolConfirmation {
				a.ConfirmTool(ConfirmAllowOnce)
			}
		}
	}()

	calls := []llm.ToolCall{
		{ID: "c1", Function: llm.ToolCallFunction{Name: tools.ToolNameRead, Arguments: `{"path":"/etc/hosts"}`}},
		{ID: "c2", Function: llm.ToolCallFunction{Name: tools.ToolNameRead, Arguments: `{"path":"/etc/passwd"}`}},
	}
	msgs, err := a.executeToolCallsParallel(t.Context(), &RunState{}, calls, ch)
	close(ch)

	require.NoError(t, err)
	require.Len(t, msgs, 2, "both calls have a result")
	for i, m := range msgs {
		assert.Contains(t, m.Content, "ReadFile:done", "call %d ran after its own card was answered", i)
		assert.NotContains(t, m.Content, "[permission]", "the note never reaches the model")
	}
}

// TestFileGateArmedIsTheCheapQuestion: the gate runs on EVERY file call, so what
// it costs when nothing is configured has to be nothing — no path resolution, no
// stat. fileGateArmed is that decision, and it reads only config.
func TestFileGateArmedIsTheCheapQuestion(t *testing.T) {
	a := fileGateAgent(t, &fakeAskJudge{p: 0.9}, func(c *config.Config) {
		c.Permissions.Files.Jev = config.FilesJevOff
	})
	// Stock shape: no rules, gate off.
	for _, tool := range []permission.FileTool{
		permission.FileToolRead, permission.FileToolWrite, permission.FileToolEdit,
	} {
		assert.False(t, a.fileGateArmed(tool), "%v: nothing is configured", tool)
	}

	// A rule for ONE tool arms that tool, and only it.
	a.SetPermissionPolicy(permission.NewPolicy(permission.Rules{}, permission.Rules{},
		permission.WithFileRules(permission.FileRules{
			Write: testPathRules([]string{"/etc/*"}, nil, nil),
		}, permission.FileRules{})))
	assert.True(t, a.fileGateArmed(permission.FileToolWrite))
	assert.False(t, a.fileGateArmed(permission.FileToolRead))

	// And the judgment gate alone is enough: with no rules at all, the gate is
	// exactly what the installation is missing.
	a.SetPermissionPolicy(nil)
	a.Config.FullConfig.Permissions.Files.Jev = config.FilesJevAuto
	assert.True(t, a.fileGateArmed(permission.FileToolRead))
}
