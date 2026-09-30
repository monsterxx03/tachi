package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/monsterxx03/tachi/agent/permission"
	"github.com/monsterxx03/tachi/agent/permission/jevfiles"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/jev"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

// The file-tool judgment gate: every ReadFile / WriteFile / EditFile call that is
// not DENIED by a path rule is put in front of an evaluation endpoint, which
// answers whether a human should see it first. Design, threshold and the case set
// that produced it: docs/2026-09-30-files-ask-jev-design.md. Its twin for shell
// commands is agent_permission_jev.go, and it shares that gate's three
// properties:
//
//   - It CANNOT deny. A deny is decided by the policy before this file is
//     reached, and the judgment's only two outcomes are "run" and "raise the
//     ask". A probability is not a guarantee, so it never gets a veto.
//   - It acts only where a human can answer (PermissionModeSkip is excluded):
//     letting a probability approve unattended file access on the user's behalf
//     is a product decision, not this gate's.
//   - When it cannot answer, the RULES decide — a rule-matched call asks (and is
//     refused where nobody can answer), an unmatched one runs. That is why a
//     service outage cannot make anything worse than it already is.
//
// Two differences from the bash gate, both deliberate and both visible to the
// user:
//
//   - A refusal refuses THIS CALL and lets the turn continue, instead of aborting
//     it the way the bash ask does (errCancelled). A file call is a step the model
//     can replace in the very next request — read another file, write it
//     elsewhere, ask the user what they meant — so ending the conversation over
//     one of them buys nothing.
//   - It is reachable on a PARALLEL tool: Read and Edit group with other calls
//     (agent/tool_executor.go), so its park is serialized on
//     PermissionState.askMu — one answer channel serves the whole agent, and two
//     cards waiting at once would race for a single keystroke.
//
// The judgment is not asked "does this call write anything": reading is the
// cheapest way to hand a credential to the model and to the transcript. The
// criteria live in jevfiles, which is also what the case file was generated from.

// fileAskMemoPrefix namespaces this gate's session memo in the map the bash gate
// also uses (PermissionState.askVerdicts). The two keys never collide by
// construction — a bash key is a command, a file key is a path — but a prefix
// makes that a fact of the code rather than a coincidence of what commands and
// paths look like.
const fileAskMemoPrefix = "file\x00"

// fileCall is one file tool call as the gate carries it: the facts the judgment is
// asked about, plus the path as the CALLER spelled it.
//
// Two paths because two readers need different ones. The judgment (and the memo)
// works on the canonical path — the file's identity, immune to aliases. A human
// reading an ask, and the model reading a refusal, need the path the call actually
// named: a card showing `/private/var/...` for a call that said `./fixture.txt` is
// the same failure as a card naming a rule the user never wrote (issue #40).
type fileCall struct {
	facts   jevfiles.Call
	lexical string
}

// subject renders the call for a human: the tool and the path they recognise.
func (c fileCall) subject() string {
	return fmt.Sprintf("%s %s", c.facts.Tool, c.lexical)
}

// judgeFileAsk runs the gate for one file call. It never blocks longer than the
// configured budget, and never returns judged=true on anything it did not get an
// answer for.
func (a *AIAgent) judgeFileAsk(ctx context.Context, call jevfiles.Call) askJudged {
	threshold, budget, on := a.fileAskGate()
	if !on {
		return askJudged{}
	}

	key := fileAskMemoPrefix + string(call.Tool) + "\x00" + call.Path + "\x00" + call.Rule
	if p, ok := a.lookupAskVerdict(key); ok {
		a.logFileAskJudgment(ctx, call, p, threshold, "memo", nil)
		return askJudged{judged: true, need: p >= threshold, p: p, threshold: threshold}
	}

	judge := a.askJudgeClient()
	if judge == nil {
		return askJudged{}
	}

	evalCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	resp, err := judge.Evaluate(evalCtx, jevfiles.State(call),
		map[string]jev.Question{jevfiles.QuestionID: jevfiles.Question()})
	if err != nil {
		// No answer: the rules decide for this call (rules-degraded), and the next
		// one tries again. A latched failure would turn one hiccup into "the rest
		// of the session is unjudged".
		a.Config.Logger.Warn(ctx, "Agent: file ask judgment unavailable; rules decide",
			"tool", call.Tool, "path", strutil.Truncate(call.Path, 200), "rule", ruleOrNone(call.Rule), "err", err)
		return askJudged{}
	}

	answer, ok := resp.Answers[jevfiles.QuestionID]
	if !ok || answer.Noul == nil {
		// Judged nothing: an unanswered question is not a low score, so this is not
		// an answer to act on either (and the log must not show it as 0.000).
		a.Config.Logger.Warn(ctx, "Agent: file ask judgment returned no answer; rules decide",
			"tool", call.Tool, "path", strutil.Truncate(call.Path, 200), "rule", ruleOrNone(call.Rule), "model", resp.Model)
		return askJudged{}
	}

	a.recordAskJudgmentUsage(ctx, resp)
	p := *answer.Noul
	a.storeAskVerdict(key, p)
	a.logFileAskJudgment(ctx, call, p, threshold, "judged", resp)
	return askJudged{judged: true, need: p >= threshold, p: p, threshold: threshold}
}

// fileAskGate reports the live gate settings: the threshold, the wait budget, and
// whether the gate runs at all. All three are read here rather than captured at
// Configure time, so a settings save reaches the next call.
func (a *AIAgent) fileAskGate() (threshold float64, budget time.Duration, on bool) {
	// Nobody to ask: an ask would either be refused (Skip) or auto-approved, and
	// auto-approving someone else's unattended file access is not this gate's call.
	if a.Config.PermissionMode == PermissionModeSkip {
		return 0, 0, false
	}
	cfg := a.Config.FullConfig
	if cfg == nil || cfg.Permissions.Files.Jev != config.FilesJevAuto {
		return 0, 0, false
	}
	if !cfg.Jev.Configured() {
		return 0, 0, false
	}
	budget = cfg.Permissions.Files.JevBudget
	if budget <= 0 {
		budget = time.Duration(config.DefaultFilesJevBudget)
	}
	return cfg.Permissions.Files.JevThreshold, budget, true
}

// logFileAskJudgment is the one line per judgment — the only record of what it
// decided and how close it was. `p` and `threshold` are both written because a
// verdict alone cannot be reviewed (and under the shadow setting, threshold=1,
// the probability is the whole signal).
func (a *AIAgent) logFileAskJudgment(ctx context.Context, call jevfiles.Call, p, threshold float64, how string, resp *jev.Response) {
	fields := []any{
		"tool", call.Tool,
		"path", strutil.Truncate(call.Path, 200),
		"rule", ruleOrNone(call.Rule),
		"verdict", verdictWord(p >= threshold),
		"p", p,
		"threshold", threshold,
		"how", how,
	}
	if resp != nil {
		fields = append(fields, "model", resp.Model,
			"input_tokens", resp.Usage.InputTokens,
			"output_tokens", resp.Usage.OutputTokens)
	}
	a.Config.Logger.Info(ctx, "Agent: file ask judged", fields...)
}

// checkFilePermission evaluates the installed policy's path rules for one file
// tool call, and runs the judgment gate over it. It returns (zero, false, nil,
// nil) to let the call proceed normally, or a tool result plus true when it has
// already decided the call's outcome — the shape checkBashPermission and
// checkMCPSearchBudget have, so the executor can call all three the same way.
//
// The note it returns is how the call was decided, when a decision happened
// beyond the rules: it travels to the EVENT only (the transcript), never into the
// model's message (see askNote).
func (a *AIAgent) checkFilePermission(ctx context.Context, tc llm.ToolCall, ch chan<- AgentEvent) (tools.ToolResult, bool, *askNote, error) {
	permTool, jevTool, ok := fileToolNames(tc.Function.Name)
	if !ok {
		return tools.ToolResult{}, false, nil, nil
	}
	// Nothing configured: no path rule for this tool AND no judgment gate. The
	// call is let through WITHOUT being resolved — a stock config pays nothing for
	// this feature (see permission.Policy.PathRulesArmed), and "the gate is off
	// means today's behaviour" is then true of the work done as well as of the
	// result.
	if !a.fileGateArmed(permTool) {
		return tools.ToolResult{}, false, nil, nil
	}
	path := fileCallPath(tc.Function.Arguments)
	if path == "" {
		// Nothing to check: the tool itself answers an empty path, with a message
		// written for the model.
		return tools.ToolResult{}, false, nil, nil
	}
	lexical, canonical := filePaths(ctx, path)
	call := fileCall{facts: a.fileCallFacts(ctx, jevTool, canonical), lexical: lexical}

	// The policy is consulted even when it is absent (fileRuleDecision): "no rules
	// at all" is exactly the installation the judgment gate exists for, so the
	// gate must not be skipped along with the policy.
	decision, rule := a.fileRuleDecision(permTool, canonical)
	call.facts.Rule = rule

	if decision == permission.DecisionDeny {
		a.Config.Logger.Info(ctx, "Agent: file blocked by permission rule",
			"tool", tc.Function.Name, "path", call.facts.Path, "rule", rule)
		return tools.ToolResult{
			Status: tools.ToolResultError,
			Err: fmt.Errorf("blocked by permission rule %q (deny) — the call was NOT executed, so nothing was "+
				"read or written. Only the user can change this rule, so tell them what was blocked and which "+
				"rule blocked it, then carry on without it: do not attempt the same thing in another form "+
				"(another path, another tool), and do not read the permission configuration looking for a way "+
				"around it", rule),
		}, true, nil, nil
	}

	// Judgment gate: every file call a rule did not DENY is judged, and one the
	// judgment says a human should see is raised through the same path a
	// rule-matched ask uses. judged == false means there is no answer to act on —
	// then the rules decide, which is this call's behaviour today.
	v := a.judgeFileAsk(ctx, call.facts)
	if v.judged && v.need {
		res, handled, err := a.resolveFileAsk(ctx, tc, call, askCause{
			rule: rule, judged: true, score: v.p, threshold: v.threshold,
		}, ch)
		if handled || err != nil {
			return res, handled, nil, err
		}
		// Approved: the judgment's number is why a human was asked at all.
		return tools.ToolResult{}, false, &askNote{
			By: noteByApproval, Score: v.p, Threshold: v.threshold, HasScore: true,
		}, nil
	}

	if v.judged {
		// The judgment read it as routine, so it runs unasked. This is the line
		// that says the decision was made by a judgment rather than by a rule.
		return tools.ToolResult{}, false, &askNote{
			By: noteByJudgment, Score: v.p, Threshold: v.threshold, HasScore: true,
		}, nil
	}

	if decision == permission.DecisionAsk {
		res, handled, err := a.resolveFileAsk(ctx, tc, call, askCause{rule: rule}, ch)
		if handled || err != nil {
			return res, handled, nil, err
		}
		// A rule asked and it was approved. No judgment number exists here (the
		// gate was off, or answered nothing), so the note carries none.
		return tools.ToolResult{}, false, &askNote{By: noteByApproval}, nil
	}
	return tools.ToolResult{}, false, nil, nil
}

// resolveFileAsk dispatches an ask according to the agent's permission mode:
// interactive prompt (TUI), external handler (desktop, ACP), or the
// non-interactive refusal (Skip). Unlike the bash ask, a refusal here refuses the
// CALL and never aborts the turn (see the file comment).
func (a *AIAgent) resolveFileAsk(ctx context.Context, tc llm.ToolCall, call fileCall, cause askCause, ch chan<- AgentEvent) (tools.ToolResult, bool, error) {
	preview := askPreview(call.subject(), cause)
	toolName, path := call.facts.Tool, call.facts.Path

	switch a.Config.PermissionMode {
	case PermissionModeSkip:
		// Channel, subagent, one-off runs: no human at the console, and the
		// judgment gate is off here too — so this is a matched ask rule, which is
		// the user asking to be consulted. Consulting them is impossible, and
		// running it anyway would be this gate granting what the rule refused.
		if a.PermState.AutoApprovePolicyAsks {
			a.Config.Logger.Info(ctx, "Agent: file ask auto-approved (allow-all session)",
				"tool", toolName, "path", path, "rule", cause.rule)
			return tools.ToolResult{}, false, nil
		}
		a.Config.Logger.Info(ctx, "Agent: file ask denied in non-interactive mode",
			"tool", toolName, "path", path, "rule", cause.rule)
		return tools.ToolResult{
			Status: tools.ToolResultError,
			Err:    fileAskUnavailable(cause, call),
		}, true, nil

	case PermissionModeExternal:
		a.Config.Logger.Info(ctx, "Agent: file ask requesting external permission",
			"tool", toolName, "path", path, "rule", cause.rule)
		approved, err := a.PermState.PermissionHandler(ctx, tc.Function.Name, tc.ID, preview, tc.Function.Arguments)
		if err != nil || !approved {
			a.dispatchPermissionResult(ctx, tc, false)
			if err != nil {
				return tools.ToolResult{Status: tools.ToolResultError, Err: err}, true, nil
			}
			return tools.ToolResult{Status: tools.ToolResultError, Err: fileAskDenied(cause, call)}, true, nil
		}
		a.dispatchPermissionResult(ctx, tc, true)
		return tools.ToolResult{}, false, nil

	default: // PermissionModeTUI
		a.Config.Logger.Info(ctx, "Agent: file ask requesting user confirmation",
			"tool", toolName, "path", path, "rule", cause.rule)

		// Serialized: every card this agent raises goes through ONE answer channel
		// per frontend, and this gate can run inside a parallel group.
		a.PermState.askMu.Lock()
		resp, err := a.askFrontend(ctx, tc, preview, ch)
		a.PermState.askMu.Unlock()
		if err != nil {
			return tools.ToolResult{Status: tools.ToolResultError, Err: err}, true, nil
		}
		if resp == ConfirmDeny {
			a.dispatchPermissionResult(ctx, tc, false)
			return tools.ToolResult{Status: tools.ToolResultError, Err: fileAskDenied(cause, call)}, true, nil
		}
		if resp == ConfirmAllowSession {
			// 「本会话全部允许」: hand the fact to the policy, which is where this
			// session's ask rules are evaluated — so every later rule-matched call
			// skips the card. Deny rules were decided above, so this cannot widen
			// into "write what the rules forbid", and the judgment still runs.
			if p := a.Config.PermissionPolicy; p != nil {
				p.AllowAllAsksForSession()
			}
		}
		a.dispatchPermissionResult(ctx, tc, true)
		return tools.ToolResult{}, false, nil
	}
}

// fileGateArmed reports whether ANYTHING on this gate has something to say about
// a call to this tool: a path rule the user wrote, or the judgment gate being on.
// Both halves are needed — a config with rules and no jev block still has to
// apply them, and a config with no rules is exactly the installation the judgment
// exists for.
//
// It answers without touching the filesystem, which is the point: the caller uses
// it to skip resolving a path that could only come back "allow".
func (a *AIAgent) fileGateArmed(tool permission.FileTool) bool {
	if a.Config.PermissionPolicy.PathRulesArmed(tool) {
		return true
	}
	_, _, on := a.fileAskGate()
	return on
}

// fileRuleDecision is the policy check itself, split out for the same reason
// bashRuleDecision is: an installation with no policy at all is the one nothing
// watches today, so "no policy" must not mean "no judgment".
func (a *AIAgent) fileRuleDecision(tool permission.FileTool, path string) (permission.Decision, string) {
	p := a.Config.PermissionPolicy
	if p == nil {
		return permission.DecisionAllow, ""
	}
	return p.CheckPath(tool, path)
}

// fileCallPath reads the target path out of a file tool call's arguments — the
// only field of them this gate looks at. A malformed blob is not the gate's to
// report: the tool answers that with a message written for the model, and an
// unreadable path simply means "nothing to check" here.
func fileCallPath(raw string) string {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return ""
	}
	return a.Path
}

// fileCallFacts assembles what the judgment is asked about: the canonical path
// (the file's identity — what two aliases of one file agree on, and what the memo
// is keyed by) and what the filesystem says about it. Nothing about the payload:
// the decision is about the path (see jevfiles.State).
//
// The stat is a fact, not a verdict: "this would overwrite a 400 KB file" and
// "this would create a file" are different actions, and which of them needs a
// human is the judgment's call (see jevfiles.Call).
func (a *AIAgent) fileCallFacts(ctx context.Context, tool jevfiles.Tool, path string) jevfiles.Call {
	call := jevfiles.Call{
		Tool:  tool,
		Path:  path,
		CWD:   wdctx.Dir(ctx),
		Trees: canonicalTrees(a.Config.PermissionTrees),
	}
	if info, err := os.Stat(path); err == nil {
		call.Exists = true
		call.Dir = info.IsDir()
		if !call.Dir {
			call.Size = info.Size()
		}
	}
	return call
}

// canonicalTrees resolves the session's trees the way the target path is resolved.
//
// The two are compared in the state ("is this call inside the trees?" is the
// judgment's reading, not ours), so a machine that reaches a workspace through a
// symlink — macOS's /var → /private/var is the everyday case — would otherwise
// show a target under /private/var against a tree listed as /var, and every call
// inside it would read as being outside. That is the asymmetric error the criteria
// cannot absorb: it talks a call under the threshold.
func canonicalTrees(trees []string) []string {
	out := make([]string, 0, len(trees))
	for _, t := range trees {
		out = append(out, permission.CanonicalizePath(t))
	}
	return out
}

// filePaths names the file this call touches, the two ways the gate needs it (see
// fileCall): the path as the argument resolved it — relative against the run's
// working directory, tools.ResolvePath, the rule every path-taking tool obeys —
// and that path canonicalised.
//
// The canonicalisation goes through permission.CanonicalizePath, the SAME
// function the policy applied to every rule's literal prefix: two sides resolved
// by two different functions is how a rule silently stops firing. A path that
// cannot be made absolute at all is passed through as written — the tool will
// report it, and the gate must not become the place a bad path is diagnosed.
func filePaths(ctx context.Context, raw string) (lexical, canonical string) {
	lexical = tools.ResolvePath(ctx, raw)
	if abs, err := filepath.Abs(lexical); err == nil {
		lexical = abs
	}
	lexical = filepath.Clean(lexical)
	return lexical, permission.CanonicalizePath(lexical)
}

// fileAskUnavailable is what the model reads when a rule-matched call needs
// approval nobody can give (PermissionModeSkip). Shaped like the bash refusals:
// the fact (not executed), the rule, who can change it, and an explicit "do not
// work around this, do not go reading the permission files" — no config path is
// named, because naming one is what sends a helpful model off to read it.
func fileAskUnavailable(cause askCause, call fileCall) error {
	return fmt.Errorf("this %s call requires interactive approval (matched ask rule %q), which is unavailable "+
		"in this non-interactive context — the call was NOT executed and %s was not touched. Only the user can "+
		"allow it (an allow rule of theirs, not something to work around), so say what was blocked and carry on "+
		"without it: do not reach the same file through another path or another tool", call.facts.Tool, cause.rule, call.lexical)
}

// fileAskDenied is what the model reads when a human refused this call. The two
// ways they can be interrupted are named differently — a rule they wrote, or a
// judgment that read the call as one they should see — because a refusal that
// claims a rule matched sends the reader hunting for one they never wrote
// (issue #40). The path is named for the reason a batch of calls needs: with
// several file calls in one turn, "which one was refused" is half the message.
func fileAskDenied(cause askCause, call fileCall) error {
	why := fmt.Sprintf("matched ask rule %q", cause.rule)
	if cause.rule == "" {
		why = fmt.Sprintf("no rule matched, and the judgment (jev) read it as one you should see first "+
			"(%.2f ≥ %.2f)", cause.score, cause.threshold)
	}
	return fmt.Errorf("the user refused this %s call on %s (%s) — it was NOT executed. They were asked "+
		"because the call matched what they asked to be consulted about, and only they can change that: carry "+
		"on without it, and do not reach the same file through another path or another tool",
		call.facts.Tool, call.lexical, why)
}
