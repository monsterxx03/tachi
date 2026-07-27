package agent

import (
	"context"

	"github.com/monsterxx03/tachi/agent/tools"
)

// ---------------------------------------------------------------------------
// Per-run tool views
//
// Some runs need a restricted tool set: /commit only needs Bash, /compact
// must not call tools at all. Historically this was implemented by mutating
// the agent's registry (SaveToolRegistry → UnregisterTool/ClearToolRegistry →
// RestoreToolRegistry), which had three problems:
//
//  1. Missing a restore path silently left the agent without tools — it
//     doesn't error, it just gets dumber, which is very hard to diagnose.
//     The TUI needed five separate restore sites because bubbletea's Update
//     is asynchronous and a run can terminate through several branches.
//  2. RestoreToolRegistry re-registered from a map, and Registry rebuilds its
//     MCP ordering from Register() call order. Go map iteration is random, so
//     the MCP tool order (and therefore the tools array sent to the provider)
//     changed after every restore, invalidating prompt caches.
//  3. Mutable per-turn state on a long-lived agent that channel mode caches
//     and shares across goroutines is a data race waiting to happen.
//
// Instead, a run may carry an immutable *toolView* on its context. The
// registry is never modified; the view filters what the LLM sees and what the
// executor is willing to invoke. The view's lifetime is the run's context, so
// it expires on its own — there is no restore step to forget.
//
// This mirrors two existing patterns in the codebase: wdctx.Dir(ctx) for
// working-directory isolation and tools.WithPathPolicy for write restrictions.
// ---------------------------------------------------------------------------

// toolView describes the tool set visible to a single run.
//
// A nil *toolView means "no restriction" — the full registry is used. This is
// the common case (regular conversation turns) and costs nothing.
//
// Note the distinction between a nil view and a view with an empty allow set:
// the former means unrestricted, the latter means no tools at all
// (WithNoTools). buildToolView guarantees allow is non-nil whenever any
// RunOption was supplied, so an empty set is never mistaken for "unrestricted".
//
// Additive views (temporarily attaching a tool that is not in the registry,
// e.g. channel mode's per-turn SendFileTool) are not supported yet; they need
// the resolver to fall back to a per-view tool map on lookup.
type toolView struct {
	allow map[string]bool
}

// RunOption customises a single RunConversationStream / RunOneOffStream call.
type RunOption func(*toolView)

// WithToolSet restricts the run to the named tools. Tools outside the set stay
// in the registry but are hidden from the LLM and refused by the executor.
//
// Example: /commit only needs Bash.
//
//	agent.RunOneOffStream(ctx, p, sys, msg, opts, meta, agent.WithToolSet(tools.ToolNameBash))
func WithToolSet(names ...string) RunOption {
	return func(v *toolView) {
		if v.allow == nil {
			v.allow = make(map[string]bool, len(names))
		}
		for _, n := range names {
			v.allow[n] = true
		}
	}
}

// WithNoTools hides every tool for the duration of the run. Used by /compact,
// where the LLM should only summarise the conversation. The compact prompt
// also asks it not to call tools; this makes that guarantee structural.
func WithNoTools() RunOption {
	return func(v *toolView) {
		if v.allow == nil {
			v.allow = make(map[string]bool)
		}
	}
}

// buildToolView folds RunOptions into a view. Returns nil when no options were
// given, so the unrestricted path stays allocation-free.
func buildToolView(opts []RunOption) *toolView {
	if len(opts) == 0 {
		return nil
	}
	v := &toolView{}
	for _, opt := range opts {
		if opt != nil {
			opt(v)
		}
	}
	// A view must never carry a nil allow set: nil is the sentinel for
	// "unrestricted", and silently granting full tool access to a run that
	// asked to be restricted is exactly the failure we're eliminating.
	if v.allow == nil {
		v.allow = make(map[string]bool)
	}
	return v
}

type toolViewKey struct{}

// withToolView attaches a tool view to the context. Passing a nil view returns
// the context unchanged.
func withToolView(ctx context.Context, v *toolView) context.Context {
	if v == nil {
		return ctx
	}
	return context.WithValue(ctx, toolViewKey{}, v)
}

// toolViewFrom extracts the tool view from the context, or nil when the run is
// unrestricted.
func toolViewFrom(ctx context.Context) *toolView {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(toolViewKey{}).(*toolView)
	return v
}

// toolResolver is the single access point to the agent's tools during a run.
// It applies the context's tool view (if any) on top of the registry.
//
// It is a value type holding only two pointers, so constructing one per call
// site is free — there is no need to thread it through function signatures.
type toolResolver struct {
	reg  *tools.Registry
	view *toolView // nil → pass through to reg
}

// resolve returns the tool resolver for the current run. Always prefer this
// over touching a.toolRegistry directly on execution paths, so that per-run
// tool restrictions are honoured.
func (a *AIAgent) resolve(ctx context.Context) toolResolver {
	return toolResolver{reg: a.toolRegistry, view: toolViewFrom(ctx)}
}

// visible reports whether the named tool is usable in this run.
func (r toolResolver) visible(name string) bool {
	if r.view == nil {
		return true
	}
	return r.view.allow[name]
}

// schemas returns the tool schemas visible to this run, preserving the
// registry's deterministic order (built-ins alphabetically, then MCP tools in
// registration order) so prompt caching stays stable.
func (r toolResolver) schemas() []tools.Schema {
	all := r.reg.GetSchemas()
	if r.view == nil {
		return all
	}
	if len(r.view.allow) == 0 {
		return nil
	}
	out := make([]tools.Schema, 0, len(r.view.allow))
	for _, s := range all {
		if r.view.allow[s.Name] {
			out = append(out, s)
		}
	}
	return out
}

// isParallel reports whether the tool may run concurrently with its
// neighbours. Hidden tools report false: they are about to be refused, and
// grouping them with genuinely parallel calls would only confuse the ordering.
func (r toolResolver) isParallel(name string) bool {
	if !r.visible(name) {
		return false
	}
	return r.reg.IsParallel(name)
}

// invoke executes a tool call, refusing tools hidden by the current view.
//
// A hidden tool yields UnknownToolError, the same result the LLM would get for
// a genuinely nonexistent tool. That is deliberate: the tool is not in the
// schemas we sent, so from the model's perspective it does not exist, and the
// error text it reads back is consistent with that.
func (r toolResolver) invoke(ctx context.Context, name, args string) tools.ToolResult {
	if !r.visible(name) {
		return tools.ToolResult{Status: tools.ToolResultError, Err: &tools.UnknownToolError{Name: name}}
	}
	return r.reg.Invoke(ctx, name, args)
}

// executeConfirmed runs the confirmed phase of a ConfirmationTool, refusing
// tools hidden by the current view.
func (r toolResolver) executeConfirmed(ctx context.Context, name, args string) (string, error) {
	if !r.visible(name) {
		return "", &tools.UnknownToolError{Name: name}
	}
	return r.reg.ExecuteConfirmed(ctx, name, args)
}
