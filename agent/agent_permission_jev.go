package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/monsterxx03/tachi/agent/permission"
	"github.com/monsterxx03/tachi/agent/permission/jevask"
	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/httpx"
	"github.com/monsterxx03/tachi/pkg/jev"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

// The Bash judgment gate: every command that is NOT denied by a rule is put in
// front of an evaluation endpoint, which answers whether a human should see it
// first. Design, thresholds and the case set that produced them:
// docs/2026-09-30-bash-ask-jev-design.md.
//
// Three properties are the whole reason it is shaped this way:
//
//   - It CANNOT deny. A deny is decided by the policy before this file is
//     reached, and the judgment's only two outcomes are "run" and "raise the
//     ask". A probability is not a guarantee, so it never gets a veto.
//   - It acts only where a human can answer (PermissionModeSkip is excluded):
//     in channel / `tachi -p` / subagents nobody could approve anything, and
//     letting a probability approve instead would quietly widen what an
//     unattended run may do — a product decision, not this gate's.
//   - When it cannot answer, the RULES decide — exactly today's behaviour for
//     that command (a rule-matched command asks, an unmatched one runs). That
//     is why a service outage cannot make anything worse than it already is.
//
// The judgment is not asked about "does this modify something": a read-only
// command can be exactly what a user wants to see first (a recursive grep over
// / runs for minutes; reading ~/.ssh/id_rsa hands over a credential). The
// criteria live in jevask, which is also what the case file was generated from.

// askVerdict is one judgment's answer, memoized for the session. Only the
// probability is stored: the threshold is applied at READ time, so editing
// permissions.bash.jev_threshold re-decides the commands already judged instead
// of leaving a session's earlier verdicts frozen against an old number.
type askVerdict struct {
	p float64
}

// askJudged is one judgment's trip through a gate — the bash gate's or the file
// gate's; both ask one question about one subject and get one probability back.
type askJudged struct {
	// judged is false when there is nothing to act on: the gate is off, the
	// frontend has nobody to ask, the service failed, or the question came back
	// unanswered. The caller must then behave exactly as it did before this
	// existed.
	judged bool
	// need is the verdict (p >= threshold), meaningful only when judged.
	need bool
	// p and threshold are what the ask's preview quotes when the verdict is the
	// reason a human is being interrupted, and what the log line carries always.
	p         float64
	threshold float64
}

// judgeBashAsk runs the gate for one command. It never blocks longer than the
// configured budget, and never returns judged=true on anything it did not get
// an answer for.
func (a *AIAgent) judgeBashAsk(ctx context.Context, cmd, rule string) askJudged {
	threshold, budget, on := a.bashAskGate()
	if !on {
		return askJudged{}
	}

	key := cmd + "\x00" + rule
	if p, ok := a.lookupAskVerdict(key); ok {
		a.logAskJudgment(ctx, cmd, rule, p, threshold, "memo", nil)
		return askJudged{judged: true, need: p >= threshold, p: p, threshold: threshold}
	}

	judge := a.askJudgeClient()
	if judge == nil {
		return askJudged{}
	}

	evalCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	resp, err := judge.Evaluate(evalCtx,
		jevask.State(jevask.Cmd{Command: cmd, Rule: rule, CWD: wdctx.Dir(ctx), Trees: a.Config.PermissionTrees}),
		map[string]jev.Question{jevask.QuestionID: jevask.Question()})
	if err != nil {
		// No answer: the rules decide for this command (rules-degraded), and the
		// next command tries again. A latched failure would turn one hiccup into
		// "the rest of the session is unjudged".
		a.Config.Logger.Warn(ctx, "Agent: bash ask judgment unavailable; rules decide",
			"rule", ruleOrNone(rule), "command", strutil.Truncate(cmd, 200), "err", err)
		return askJudged{}
	}

	answer, ok := resp.Answers[jevask.QuestionID]
	if !ok || answer.Noul == nil {
		// Judged nothing: an unanswered question is not a low score, so this is
		// not an answer to act on either (and the log must not show it as 0.000).
		a.Config.Logger.Warn(ctx, "Agent: bash ask judgment returned no answer; rules decide",
			"rule", ruleOrNone(rule), "command", strutil.Truncate(cmd, 200), "model", resp.Model)
		return askJudged{}
	}

	a.recordAskJudgmentUsage(ctx, resp)
	p := *answer.Noul
	a.storeAskVerdict(key, p)
	a.logAskJudgment(ctx, cmd, rule, p, threshold, "judged", resp)
	return askJudged{judged: true, need: p >= threshold, p: p, threshold: threshold}
}

// bashAskGate reports the live gate settings: the threshold, the wait budget, and
// whether the gate runs at all. All three are read here rather than captured at
// Configure time, so a settings save reaches the next command.
func (a *AIAgent) bashAskGate() (threshold float64, budget time.Duration, on bool) {
	// Nobody to ask: the gate's one outcome beyond "run" is "raise the ask", and an
	// unattended mode answers that ask on its own — approving someone else's run on
	// a probability is not this gate's call.
	if a.Config.PermissionMode == PermissionModeSkip {
		return 0, 0, false
	}
	cfg := a.Config.FullConfig
	if cfg == nil || cfg.Permissions.Bash.Jev != config.BashJevAuto {
		return 0, 0, false
	}
	if !cfg.Jev.Configured() {
		return 0, 0, false
	}
	budget = cfg.Permissions.Bash.JevBudget
	if budget <= 0 {
		budget = time.Duration(config.DefaultJevAskBudget)
	}
	return cfg.Permissions.Bash.JevThreshold, budget, true
}

// askJudgeClient returns the evaluation client, building it on first use and
// rebuilding it when the block it was built from changes.
//
// Lazy rather than Configure-time so the settings rows stay live (no push
// plumbing), but KEPT across commands: the client owns an http.Client, and a
// fresh one per command would drop connection pooling and pay a new handshake
// every time — which is most of the latency this gate costs.
func (a *AIAgent) askJudgeClient() jev.Judge {
	if a.askJudgeOverride != nil {
		return a.askJudgeOverride
	}
	cfg := a.Config.FullConfig
	if cfg == nil || !cfg.Jev.Configured() {
		return nil
	}
	key := cfg.Jev.APIKey + "\x00" + cfg.Jev.Model + "\x00" + cfg.Jev.BaseURL + "\x00" +
		cfg.Jev.Proxy + "\x00" + cfg.Jev.Timeout.String()

	a.askJudgeMu.Lock()
	defer a.askJudgeMu.Unlock()
	if a.askJudge != nil && a.askJudgeKey == key {
		return a.askJudge
	}
	client, err := jev.NewClient(cfg.Jev.APIKey,
		jev.WithModel(cfg.Jev.Model),
		jev.WithBaseURL(cfg.Jev.BaseURL),
		jev.WithHTTPClient(httpx.NewClient(time.Duration(cfg.Jev.Timeout), cfg.Jev.Proxy)),
		jev.WithLogger(a.Config.Logger),
	)
	if err != nil {
		// A client that cannot be built is not fatal: the gate degrades to the
		// rules, exactly like a failed request. Logged once per rebuild attempt.
		a.Config.Logger.Warn(context.Background(),
			"Agent: bash ask judgment unavailable; rules decide", "err", err)
		a.askJudge, a.askJudgeKey = nil, ""
		return nil
	}
	a.askJudge, a.askJudgeKey = client, key
	return a.askJudge
}

// recordAskJudgmentUsage writes the ledger row for one judgment. The call is not
// a Provider call, so the billing decorator never sees it: a gate that skipped
// this would make its calls look free in /usage.
func (a *AIAgent) recordAskJudgmentUsage(ctx context.Context, resp *jev.Response) {
	cfg := a.Config.FullConfig
	if cfg == nil {
		return
	}
	err := llm.RecordJevUsage(a.usageRecorder(), cfg.Jev, resp.Model, a.currentSessionID(),
		resp.Usage.InputTokens, resp.Usage.OutputTokens, time.Now())
	if err != nil {
		a.Config.Logger.Warn(ctx, "Agent: failed to record jev ask usage", "err", err)
	}
}

// logAskJudgment is the one line per judgment — the only record of what it
// decided and how close it was. `p` and `threshold` are both written because a
// verdict alone cannot be reviewed (and under the shadow setting, threshold=1,
// the probability is the whole signal).
func (a *AIAgent) logAskJudgment(ctx context.Context, cmd, rule string, p, threshold float64, how string, resp *jev.Response) {
	fields := []any{
		"rule", ruleOrNone(rule),
		"command", strutil.Truncate(cmd, 200),
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
	a.Config.Logger.Info(ctx, "Agent: bash ask judged", fields...)
}

func verdictWord(need bool) string {
	if need {
		return "ask"
	}
	return "run"
}

// ruleOrNone renders the rule slot for a log field: an empty rule is not a
// missing value, it is "no rule matched", which is a different fact from the
// sentinel that means the command could not be parsed.
func ruleOrNone(rule string) string {
	if rule == "" {
		return "(none)"
	}
	return rule
}

// lookupAskVerdict / storeAskVerdict are the session memo (see PermissionState).
// An agent built without going through NewAIAgent (a bare struct in a test) has
// no map, which just means nothing is memoized.
func (a *AIAgent) lookupAskVerdict(key string) (float64, bool) {
	a.PermState.askVerdictsMu.Lock()
	defer a.PermState.askVerdictsMu.Unlock()
	v, ok := a.PermState.askVerdicts[key]
	return v.p, ok
}

func (a *AIAgent) storeAskVerdict(key string, p float64) {
	a.PermState.askVerdictsMu.Lock()
	defer a.PermState.askVerdictsMu.Unlock()
	if a.PermState.askVerdicts == nil {
		a.PermState.askVerdicts = make(map[string]askVerdict)
	}
	a.PermState.askVerdicts[key] = askVerdict{p: p}
}

// SetAskJudgeForTest installs a judge in place of the real client, so a test can
// answer a judgment without a service. Passing nil restores the live client.
func (a *AIAgent) SetAskJudgeForTest(j jev.Judge) { a.askJudgeOverride = j }

// ResetAskVerdicts drops the session memo. Called where the session's own
// approvals are dropped (ClearSession): a new conversation is not the one whose
// commands were already judged.
func (a *AIAgent) ResetAskVerdicts() {
	a.PermState.askVerdictsMu.Lock()
	defer a.PermState.askVerdictsMu.Unlock()
	a.PermState.askVerdicts = nil
}

// askNote is how ONE Bash call's permission decision was made, carried into the
// tool result so the model — and anyone reading the transcript afterwards — can
// see it. It is the Bash counterpart of the MCP search result's `matched_by` +
// `score` pair, and the pair is the point for the same reason: `judgment`'s
// score is the probability that let a command run, `approval`'s is the
// probability that made the gate ASK, so a bare number would invite a
// comparison that means nothing.
//
// It is emitted only when a decision actually happened somewhere beyond the
// rules — no judgment and no ask, no line. A gate that is off, or a command
// nothing matched under it, produces exactly the output it produced before this
// existed.
type askNote struct {
	// By is noteByJudgment (the judgment read the command as routine, so it ran
	// without asking) or noteByApproval (something was asked and approved — by
	// the human, or by a session-wide allow).
	By string
	// Score and Threshold are the judgment's numbers, present only when a
	// judgment produced them: an approval a RULE raised (the gate degraded or
	// off) has none, and a zero there would read as "the judgment was sure this
	// was safe".
	Score     float64
	Threshold float64
	HasScore  bool
}

// The values askNote.By takes.
const (
	noteByJudgment = "judgment"
	noteByApproval = "approval"
)

// Line renders the note as the single line appended to a Bash result. Key=value
// rather than prose so both the model and a grep of the transcript can read it,
// and so the three fields stay distinguishable.
func (n *askNote) Line() string {
	if n == nil {
		return ""
	}
	line := "[permission] decided_by=" + n.By
	if n.HasScore {
		line += fmt.Sprintf(" score=%.3f threshold=%.3f", n.Score, n.Threshold)
	}
	return line
}

// permissionRuleFor is the policy check itself, split out so the gate can run
// even when there is no policy: an installation with no rules at all (and the
// builtins disabled) is the one nothing watches today, so "no policy" must not
// mean "no judgment".
func (a *AIAgent) bashRuleDecision(cmd string) (permission.Decision, string) {
	p := a.Config.PermissionPolicy
	if p == nil || p.Empty() {
		return permission.DecisionAllow, ""
	}
	return p.CheckBash(cmd)
}
