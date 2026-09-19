package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/monsterxx03/tachi/agent/hooks"
	"github.com/monsterxx03/tachi/agent/permission"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// NewPermissionPolicyFromConfig builds a permission.Policy from the global
// config permissions plus project-level rules (.tachi/permissions.yaml under
// projectRoot). Built-in absolutely-dangerous deny rules are always included
// unless disabled via permissions.bash.disable_builtin_deny in the GLOBAL
// config (the project file cannot disable them). Project-level allow rules
// are ignored — projects can only tighten, never exempt (see NewPolicy).
// Returns nil only when no rules exist at all AND builtins are disabled.
func NewPermissionPolicyFromConfig(cfg *config.Config, projectRoot string, lg *logger.Logger) *permission.Policy {
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
	var project permission.Rules
	pp, err := config.LoadProjectPermissions(projectRoot)
	if err != nil {
		if lg != nil {
			lg.Warn(context.Background(), "agent: ignoring invalid project permissions file", "error", err)
		}
	} else {
		if len(pp.Bash.Allow) > 0 && lg != nil {
			lg.Warn(context.Background(), "agent: ignoring project-level allow rules (allow is global-only)",
				"count", len(pp.Bash.Allow))
		}
		project = permission.Rules{
			Deny: pp.Bash.Deny,
			Ask:  pp.Bash.Ask,
			// Allow intentionally omitted — dropped by NewPolicy anyway.
		}
	}
	if cfg.Permissions.Bash.DisableBuiltinDeny {
		// User opted out of ALL built-in protection: neither the disk/shutdown
		// glob rules (BuiltinDenyRules) nor the structured rm guard apply.
		pol := permission.NewPolicyNoBuiltins(global, project)
		if pol.Empty() {
			return nil
		}
		return pol
	}
	pol := permission.NewPolicy(global, project)
	if pol.Empty() {
		return nil
	}
	return pol
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
//   - (zero, false, nil)  — policy allows; caller proceeds with normal Invoke
//   - (result, true, nil) — policy deny, or an ask resolved without execution
//     (non-interactive denial / external rejection / ctx cancel); result is
//     a ToolResultError fed back to the LLM
//   - (_, _, errCancelled) — user explicitly denied at the TUI prompt;
//     aborts the turn, consistent with EditFile confirmation denial
func (a *AIAgent) checkBashPermission(ctx context.Context, tc llm.ToolCall, ch chan<- AgentEvent) (tools.ToolResult, bool, error) {
	if tc.Function.Name != tools.ToolNameBash {
		return tools.ToolResult{}, false, nil
	}
	p := a.Config.PermissionPolicy
	if p == nil || p.Empty() {
		return tools.ToolResult{}, false, nil
	}
	cmd := bashCommand(tc.Function.Arguments)
	if cmd == "" {
		return tools.ToolResult{}, false, nil
	}

	decision, rule := p.CheckBash(cmd)
	switch decision {
	case permission.DecisionAllow:
		return tools.ToolResult{}, false, nil

	case permission.DecisionDeny:
		a.Config.Logger.Info(ctx, "Agent: bash blocked by permission rule", "rule", rule, "command", cmd)
		return tools.ToolResult{
			Status: tools.ToolResultError,
			Err: fmt.Errorf("blocked by permission rule %q (deny) — the command was NOT executed. "+
				"If this should be allowed, ask the user to adjust permissions.bash rules in ~/.tachi/config.yaml or .tachi/permissions.yaml", rule),
		}, true, nil

	default: // permission.DecisionAsk
		return a.resolveBashAsk(ctx, tc, cmd, rule, ch)
	}
}

// resolveBashAsk dispatches a policy "ask" decision according to the agent's
// permission mode: interactive prompt (TUI), external handler (ACP), or a
// non-interactive fallback (Skip: deny unless the user chose "allow all").
func (a *AIAgent) resolveBashAsk(ctx context.Context, tc llm.ToolCall, cmd, rule string, ch chan<- AgentEvent) (tools.ToolResult, bool, error) {
	preview := bashAskPreview(cmd, rule)

	switch a.Config.PermissionMode {
	case PermissionModeSkip:
		// Channel, subagent, one-off runs: no human at the console.
		if a.PermState.AutoApprovePolicyAsks {
			a.Config.Logger.Info(ctx, "Agent: bash ask auto-approved (allow-all session)", "rule", rule)
			return tools.ToolResult{}, false, nil
		}
		a.Config.Logger.Info(ctx, "Agent: bash ask denied in non-interactive mode", "rule", rule, "command", cmd)
		if rule == permission.AskNoRule {
			// Not a rule the user could allow: the policy never got far enough to consult one.
			// Fixing the command is the only way through, so say that instead of naming a rule
			// that does not exist.
			return tools.ToolResult{
				Status: tools.ToolResultError,
				Err: fmt.Errorf("the command could not be parsed, so it needs approval that is unavailable " +
					"in this non-interactive context — the command was NOT executed. Fix the syntax (an " +
					"unclosed quote/bracket, or a trailing &&)"),
			}, true, nil
		}
		return tools.ToolResult{
			Status: tools.ToolResultError,
			Err: fmt.Errorf("command requires interactive approval (matched ask rule %q), which is unavailable "+
				"in this non-interactive context — the command was NOT executed. "+
				"Ask the user to add an allow rule to permissions.bash", rule),
		}, true, nil

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
		a.dispatchEvent(ctx, hooks.EventPermissionRequest, hooks.Payload{
			ToolName: tc.Function.Name,
			ToolID:   tc.ID,
			ToolArgs: tc.Function.Arguments,
		})
		ch <- AgentEvent{
			Type:     AgentEventToolConfirmation,
			ToolName: tc.Function.Name,
			ToolID:   tc.ID,
			ToolArgs: tc.Function.Arguments,
			ToolDiff: preview,
		}

		select {
		case resp := <-a.Channels.ConfirmResp:
			switch resp {
			case ConfirmAllowSession, ConfirmAllowOnce:
				if resp == ConfirmAllowSession {
					// 「本会话全部允许」: stop asking about this session's ask
					// rules. Deny rules are checked before an ask is raised, so
					// this cannot widen into "run what the rules forbid".
					a.Config.PermissionPolicy.AllowAllAsksForSession()
				}
				a.dispatchPermissionResult(ctx, tc, true)
				return tools.ToolResult{}, false, nil
			default: // ConfirmDeny
				a.dispatchPermissionResult(ctx, tc, false)
				return tools.ToolResult{}, true, errCancelled
			}
		case <-ctx.Done():
			return tools.ToolResult{Status: tools.ToolResultError, Err: ctx.Err()}, true, nil
		}
	}
}

// bashAskPreview renders the confirmation prompt content for a bash ask: the command, a blank
// line, then WHY it is being asked about.
//
// The reason is worded from the rule slot, and the slot has two different kinds of content — a rule
// that matched, or permission.AskNoRule for a command the policy could not parse (nothing matched;
// there is no rule to name). Saying "matched ask rule" for the second kind is what made the desktop
// card claim a rule that does not exist, and made a malformed command look like a truncated one
// (issue #40): the reader saw `… | head &&` hanging in the air and a rule name they had never
// written.
func bashAskPreview(cmd, rule string) string {
	return fmt.Sprintf("$ %s\n\n%s", cmd, askReasonLine(rule))
}

// askReasonLine words one ask's cause for a HUMAN. AskNoRule gets a sentence that says what is
// actually wrong and what fixes it, because that case is the one a reader has to act on (a rule
// they wrote is a rule they can recognise).
func askReasonLine(rule string) string {
	if rule == permission.AskNoRule {
		return "这条命令没能解析出来（引号/括号没闭合、末尾挂着 && 之类），没有命中任何规则 —— " +
			"保守起见先问你一次；把语法写对就不会再问"
	}
	return fmt.Sprintf("命中 ask 规则: %q", rule)
}
