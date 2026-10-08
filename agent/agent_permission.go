package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/monsterxx03/tachi/agent/permission"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// NewPermissionPolicyFromConfig builds a permission.Policy from the global
// config permissions plus the project-level rules of EVERY tree in projectRoots
// (.tachi/permissions.yaml under each; config.ProjectRootsFrom resolves and
// de-duplicates them). Built-in absolutely-dangerous deny rules are always
// included unless disabled via permissions.bash.disable_builtin_deny in the
// GLOBAL config (a project file cannot disable them). Project-level allow rules
// are ignored — projects can only tighten, never exempt (see NewPolicy).
// Returns nil only when no rules exist at all AND builtins are disabled.
//
// Every tree contributes: a session spanning two repositories has to honour
// both repositories' tripwires, and taking only the first one silently dropped
// the rest (see ProjectRootsFrom).
func NewPermissionPolicyFromConfig(cfg *config.Config, projectRoots []string, lg *logger.Logger) *permission.Policy {
	if cfg == nil {
		return nil
	}
	globalDeny := cfg.Permissions.Bash.Deny
	if !cfg.Permissions.Bash.DisableBuiltinDeny {
		// Built-ins first — same precedence tier as user deny rules.
		globalDeny = append(append([]string{}, permission.BuiltinDenyRules...), cfg.Permissions.Bash.Deny...)
	}
	global := permission.Rules{
		Deny:  globalDeny,
		Ask:   cfg.Permissions.Bash.Ask,
		Allow: cfg.Permissions.Bash.Allow,
	}
	// The file tools' path rules of the same layer. The GLOBAL layer's relative
	// patterns are anchored to the user's home, which is what makes
	// `ask: ["secrets/*"]` there read as "$HOME/secrets/*" rather than as a rule
	// about wherever the process happens to have been started.
	home := homeDir(lg)
	globalFiles := expandFileRules(cfg.Permissions.Files, home, home)

	// The trees' rules are UNIONED into one project Rules: deny and ask can only
	// be added to (NewPolicy takes one project side), which is exactly the
	// "projects can only tighten" rule, now applied per tree instead of to one.
	// Every tree is expanded against ITSELF, so a relative pattern in a project
	// file means "inside this repository" rather than "inside whichever tree
	// happened to be read first".
	var project permission.Rules
	var projectFiles permission.FileRules
	for _, root := range projectRoots {
		pp, err := config.LoadProjectPermissions(root)
		if err != nil {
			if lg != nil {
				lg.Warn(context.Background(), "agent: ignoring invalid project permissions file", "error", err)
			}
			continue
		}
		if (len(pp.Bash.Allow) > 0 || projectFileAllowCount(pp) > 0) && lg != nil {
			lg.Warn(context.Background(), "agent: ignoring project-level allow rules (allow is global-only)",
				"tree", root, "count", len(pp.Bash.Allow)+projectFileAllowCount(pp))
		}
		project.Deny = append(project.Deny, pp.Bash.Deny...)
		project.Ask = append(project.Ask, pp.Bash.Ask...)
		// Allow intentionally omitted — dropped by NewPolicy anyway.
		projectFiles = unionFileRules(projectFiles, expandFileRules(pp.Files, root, home))
	}
	if cfg.Permissions.Bash.DisableBuiltinDeny {
		// User opted out of ALL built-in protection: neither the disk/shutdown
		// glob rules (BuiltinDenyRules) nor the structured rm guard apply. There
		// are no built-in PATH denials to opt out of — the file rules are the
		// user's alone (see docs/2026-09-30-files-ask-jev-design.md).
		pol := permission.NewPolicyNoBuiltins(global, project, permission.WithFileRules(globalFiles, projectFiles))
		if pol.Empty() {
			return nil
		}
		return pol
	}
	pol := permission.NewPolicy(global, project, permission.WithFileRules(globalFiles, projectFiles))
	if pol.Empty() {
		return nil
	}
	return pol
}

// homeDir resolves the anchor for the global config's relative path patterns.
// An unresolvable home (no $HOME, no passwd entry) leaves `~` and relative
// patterns as written — i.e. matching nothing — which is why it is warned about
// rather than papered over with a guess: a deny rule silently pointed at the wrong
// directory is worse than one that does not fire.
func homeDir(lg *logger.Logger) string {
	home, err := os.UserHomeDir()
	if err != nil {
		if lg != nil {
			lg.Warn(context.Background(),
				"agent: cannot resolve the home directory; path rules written with ~ or relative to it will not match",
				"error", err)
		}
		return ""
	}
	return home
}

// expandFileRules reads one layer's path rules into the rules the policy matches:
// every pattern anchored (home for the global layer, the tree for a project file)
// and cleaned, so that matching is a plain string comparison against a canonical
// path.
func expandFileRules(f config.FilesPermissions, anchor, home string) permission.FileRules {
	return permission.FileRules{
		Read:  permission.ExpandPathRules(pathRules(f.Read), anchor, home),
		Write: permission.ExpandPathRules(pathRules(f.Write), anchor, home),
		Edit:  permission.ExpandPathRules(pathRules(f.Edit), anchor, home),
	}
}

func pathRules(p config.PathPermissions) permission.Rules {
	return permission.Rules{Deny: p.Deny, Ask: p.Ask, Allow: p.Allow}
}

// unionFileRules adds one layer's file rules to another: the per-tree project
// files accumulate, since every tree the session works in contributes.
func unionFileRules(dst, src permission.FileRules) permission.FileRules {
	return permission.FileRules{
		Read:  unionPathRules(dst.Read, src.Read),
		Write: unionPathRules(dst.Write, src.Write),
		Edit:  unionPathRules(dst.Edit, src.Edit),
	}
}

func unionPathRules(dst, src permission.PathRules) permission.PathRules {
	return permission.PathRules{
		Deny:  append(dst.Deny, src.Deny...),
		Ask:   append(dst.Ask, src.Ask...),
		Allow: append(dst.Allow, src.Allow...),
	}
}

// projectFileAllowCount counts the path-allow rules a project file declares —
// they are dropped (allow is global-only), and the caller warns about them the
// way it does for the bash half.
func projectFileAllowCount(pp config.PermissionsConfig) int {
	return len(pp.Files.Read.Allow) + len(pp.Files.Write.Allow) + len(pp.Files.Edit.Allow)
}

// bashCommand extracts the "command" field from Bash tool args JSON.
func bashCommand(args string) string {
	var a struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return ""
	}
	return a.Command
}

// checkBashPermission evaluates the installed permission policy for a Bash
// tool call before it reaches the tool registry.
//
// Returns (result, handled, err):
//   - (zero, false, nil)  — allowed (no rule matched, or an allow rule did):
//     the caller proceeds with normal Invoke. A rule that ALLOWED the command
//     also skips the judgment gate, so this return covers both.
//   - (result, true, nil) — policy deny, or an ask resolved without execution
//     (non-interactive denial / external rejection / ctx cancel); result is
//     a ToolResultError fed back to the LLM
//   - (_, _, errCancelled) — user explicitly denied at the TUI prompt;
//     aborts the turn, consistent with EditFile confirmation denial
//
// It returns a note as well as the decision: how the call was decided travels
// with the result — to the EVENT only (the transcript), never into the model's
// message (see askNote).
func (a *AIAgent) checkBashPermission(ctx context.Context, tc llm.ToolCall, ch chan<- AgentEvent) (tools.ToolResult, bool, *askNote, error) {
	if tc.Function.Name != tools.ToolNameBash {
		return tools.ToolResult{}, false, nil, nil
	}
	cmd := bashCommand(tc.Function.Arguments)
	if cmd == "" {
		return tools.ToolResult{}, false, nil, nil
	}

	// The policy is consulted even when it is absent (bashRuleDecision): "no
	// rules at all" is exactly the installation the judgment gate exists for, so
	// the gate must not be skipped along with the policy.
	decision, rule := a.bashRuleDecision(cmd)

	if decision == permission.DecisionDeny {
		a.Config.Logger.Info(ctx, "Agent: bash blocked by permission rule", "rule", rule, "command", cmd)
		return tools.ToolResult{
			Status: tools.ToolResultError,
			Err: fmt.Errorf("blocked by permission rule %q (deny) — the command was NOT executed. "+
				"Only the user can change this rule, so tell them what was blocked and which rule blocked it, "+
				"then carry on without that command: do not attempt the same thing in another form, and do not "+
				"read the permission configuration looking for a way around it", rule),
		}, true, nil, nil
	}

	// An allow rule the user wrote: the judgment gate is not consulted at all.
	//
	// The rule IS the user's answer about this shape of command, and the gate's
	// two outcomes are "run" and "raise the ask" — so over a command the user
	// already exempted, the only thing it could ever do is raise the ask it was
	// told not to raise. Passing the rule to the judgment as another state fact
	// instead was measured and buys ~0.02 (docs/2026-09-30-bash-ask-jev-design.md):
	// the rule has to decide, not advise.
	//
	// The position matters: after deny (the one decision that outranks it, so an
	// allow rule can never widen into running what a deny forbids) and before the
	// gate. Nothing is asked and nothing is judged, so no `[permission]` note is
	// written either — a command the user exempted behaves exactly as it did
	// before this gate existed.
	//
	// Note the asymmetry with the ask path: an `ask` rule narrows the gate (the
	// command is still judged, and a verdict of "run" lets it through), while an
	// `allow` rule switches it off for that command.
	if decision == permission.DecisionAllowExempt {
		return tools.ToolResult{}, false, nil, nil
	}

	// 「本会话全部允许」 (the session-scoped approval): the user has already answered every ask this
	// session can raise, so nothing below has anyone left to ask. It sits AFTER the deny branch for
	// the reason the policy's own copy does — deny is the one decision the approval must never reach
	// — and BEFORE the judgment on purpose: a verdict nobody can act on is not worth a call, and the
	// card it would raise is the one the user just dismissed. That covers all three causes at once:
	// a matched ask rule (already turned into an allow by the policy's copy), a command the parser
	// could not read (`AskNoRule`), and the judgment's own ask.
	//
	// The unparseable cause is worth naming, because it is the one ask that stops being a JUDGMENT
	// about risk: nothing was parsed, so no rule — deny included — could be evaluated, and the
	// command runs on the strength of the user's blanket answer alone. That is deliberate and
	// matches every unattended frontend (PermissionModeSkip approves a policy ask of either kind
	// on its own, having nobody to consult), and the population is benign in practice: an unclosed
	// quote or a trailing `&&` — both of which bash refuses too.
	if a.PermState.AllowAllAsks {
		return tools.ToolResult{}, false, nil, nil
	}

	// Judgment gate (agent_permission_jev.go): every command a rule did not DENY is
	// judged, and one the judgment says a human should see is raised through the
	// same path a rule-matched ask uses. judged == false means there is no answer
	// to act on — then the rules decide, which is this command's behaviour today.
	v := a.judgeBashAsk(ctx, cmd, rule)
	if v.judged && v.need {
		res, handled, err := a.resolveBashAsk(ctx, tc, cmd, askCause{
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
		res, handled, err := a.resolveBashAsk(ctx, tc, cmd, askCause{rule: rule}, ch)
		if handled || err != nil {
			return res, handled, nil, err
		}
		// A rule asked and it was approved. No judgment number exists here (the
		// gate was off, or answered nothing), so the note carries none. The
		// unattended branch approves without asking anyone, and a note claiming an
		// approval nobody gave would be the transcript misreporting who decided.
		if a.Config.PermissionMode == PermissionModeSkip {
			return tools.ToolResult{}, false, nil, nil
		}
		return tools.ToolResult{}, false, &askNote{By: noteByApproval}, nil
	}
	return tools.ToolResult{}, false, nil, nil
}

// askCause is why a human is being asked. The kinds must render DIFFERENTLY: a
// preview that names a rule that did not match (issue #40) sends the reader
// looking for something they never wrote, and an ask raised by the judgment is a
// third kind again.
//
// It is shared by both gates — the Bash one and the file tools' — because the
// reason a human is interrupted is the same fact in both, and the previews are
// built from it by one renderer (askPreview). The unparseable sentinel is the
// bash gate's alone: a path always parses, so no file-side ask ever carries it.
type askCause struct {
	// rule is the ask rule the policy matched, permission.AskNoRule when the
	// command could not be parsed, or "" when nothing matched.
	rule string
	// judged marks an ask the judgment gate raised (as opposed to one a rule did).
	judged bool
	// score and threshold are quoted to the human when the judgment is the reason
	// they are being interrupted.
	score     float64
	threshold float64
}

// resolveBashAsk dispatches a policy "ask" decision according to the agent's
// permission mode: interactive prompt (TUI), external handler (ACP), or a
// non-interactive fallback (Skip: deny unless the user chose "allow all").
func (a *AIAgent) resolveBashAsk(ctx context.Context, tc llm.ToolCall, cmd string, cause askCause, ch chan<- AgentEvent) (tools.ToolResult, bool, error) {
	rule := cause.rule
	preview := askPreview("$ "+cmd, cause)

	switch a.Config.PermissionMode {
	case PermissionModeSkip:
		// Channel, subagent, dream, the one-off runs (`tachi -p`, `-c`): no human at the
		// console. An `ask` rule is the user asking to be consulted before the command
		// runs, and there is nobody to consult, so the only reading that lets an
		// unattended run finish is "run it" — refusing stalls the run on a question
		// that has no answer here (the desktop is the interactive frontend, and it
		// raises the same rule as a card: PermissionModeExternal).
		//
		// `deny` is untouched: it is decided before this branch is reached, so a deny
		// rule and the built-in protections keep meaning what they say in this mode.
		// The unparseable command ends up here too (AskNoRule): nothing was parsed, so
		// no rule — deny included — saw any part of it, and the command runs on this
		// mode's blanket answer alone. The population is benign in practice: an
		// unclosed quote, or a trailing `&&`, both of which bash refuses as well.
		a.Config.Logger.Info(ctx, "Agent: bash ask approved (non-interactive mode)",
			"rule", rule, "command", cmd)
		return tools.ToolResult{}, false, nil

	case PermissionModeExternal:
		a.Config.Logger.Info(ctx, "Agent: bash ask requesting external permission", "rule", rule)
		approved, err := a.PermState.PermissionHandler(ctx, tc.Function.Name, tc.ID, preview, tc.Function.Arguments)
		if err != nil || !approved {
			a.dispatchPermissionResult(ctx, tc, false)
			if err != nil {
				return tools.ToolResult{Status: tools.ToolResultError, Err: err}, true, nil
			}
			return tools.ToolResult{Status: tools.ToolResultError, Err: errors.New("permission denied by client")}, true, nil
		}
		a.dispatchPermissionResult(ctx, tc, true)
		return tools.ToolResult{}, false, nil

	default: // PermissionModeTUI
		a.Config.Logger.Info(ctx, "Agent: bash ask requesting user confirmation", "rule", rule)
		resp, err := a.askFrontend(ctx, tc, preview, ch)
		if err != nil {
			return tools.ToolResult{Status: tools.ToolResultError, Err: err}, true, nil
		}
		switch resp {
		case ConfirmAllowSession, ConfirmAllowOnce:
			if resp == ConfirmAllowSession {
				// 「本会话全部允许」: stop asking about this session's rules AND its
				// judgments (one setter flips both readers). Deny rules are checked
				// before an ask is raised, so this cannot widen into "run what the
				// rules forbid".
				a.AllowAllAsksForSession()
			}
			a.dispatchPermissionResult(ctx, tc, true)
			return tools.ToolResult{}, false, nil
		default: // ConfirmDeny
			a.dispatchPermissionResult(ctx, tc, false)
			return tools.ToolResult{}, true, errCancelled
		}
	}
}

// askPreview renders the confirmation prompt content for one ask: the subject
// (the command, or the tool and the path it would touch), a blank line, then WHY
// it is being asked about.
//
// The reason is worded from the rule slot, and the slot has two different kinds of content — a rule
// that matched, or permission.AskNoRule for a command the policy could not parse (nothing matched;
// there is no rule to name). Saying "matched ask rule" for the second kind is what made the desktop
// card claim a rule that does not exist, and made a malformed command look like a truncated one
// (issue #40): the reader saw `… | head &&` hanging in the air and a rule name they had never
// written.
//
// `subject` is written by the AGENT, not by the frontend: the desktop shows this text verbatim, and
// a second renderer of it would be free to drift from the reason line a human acts on.
func askPreview(subject string, cause askCause) string {
	return fmt.Sprintf("%s\n\n%s", subject, askReasonLine(cause))
}

// askReasonLine words one ask's cause for a HUMAN. AskNoRule gets a sentence that says what is
// actually wrong and what fixes it, because that case is the one a reader has to act on (a rule
// they wrote is a rule they can recognise).
func askReasonLine(cause askCause) string {
	switch {
	case cause.rule != "" && cause.rule != permission.AskNoRule:
		// A rule the user wrote: their own words are the recognisable reason, so
		// name it even when a judgment agreed.
		return fmt.Sprintf("命中 ask 规则: %q", cause.rule)

	case cause.judged:
		// No rule matched, and the judgment is why they are being interrupted.
		// Saying so is the point: otherwise the reader goes looking in the config
		// for a rule that does not exist.
		line := fmt.Sprintf("没有任何 ask 规则命中，是判定（jev）认为这条需要你先看一眼（%.2f ≥ %.2f）", cause.score, cause.threshold)
		if cause.rule == permission.AskNoRule {
			line += "；另外这条命令本身也没能解析出来"
		}
		return line

	default:
		return "这条命令没能解析出来（引号/括号没闭合、末尾挂着 && 之类），没有命中任何规则 —— " +
			"保守起见先问你一次；把语法写对就不会再问"
	}
}
