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

// toolView describes the tool set visible to a single run. A view is either
// subtractive (restrict the registry), additive (attach run-scoped tools that
// the registry never sees), or both.
//
// A nil *toolView means "no restriction, no extras" — the full registry is
// used. This is the common case (regular conversation turns) and costs nothing.
//
// The restrict flag, rather than "allow == nil", is what distinguishes
// unrestricted from restricted-to-nothing. WithNoTools sets restrict with an
// empty allow set; inferring restriction from a nil map would make it
// impossible to express "full registry plus these extras", and worse, a bug
// that dropped the map would silently grant full tool access to a run that
// asked to be restricted.
type toolView struct {
	// restrict limits the visible registry tools to allow. When false, the
	// whole registry is visible (extras still apply).
	restrict bool
	allow    map[string]bool

	// extra holds run-scoped tools that are not in the agent's registry, e.g.
	// channel mode's per-turn SendFileTool, whose callback closure captures a
	// fresh attachment sink and so cannot be shared across turns.
	//
	// It is a Registry rather than a plain map so that invocation reuses the
	// registry's argument validation, confirmation protocol, image side
	// channel and subagent carriers. Reimplementing those here would be a
	// second, subtly different execution path.
	extra *tools.Registry
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
		v.restrict = true
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
		v.restrict = true
		if v.allow == nil {
			v.allow = make(map[string]bool)
		}
	}
}

// WithExtraTools attaches run-scoped tools that are not in the agent's
// registry. They are visible to the LLM and invocable for this run only.
//
// Use this for tools whose instance is meaningful for exactly one run — e.g. a
// tool whose callback closes over per-turn state. The agent's registry is never
// touched, so concurrent runs on other threads are unaffected and there is no
// unregister step to forget.
//
// Extras shadow same-named registry tools, and are exempt from WithToolSet
// filtering: attaching a tool is itself the statement that this run may use it.
func WithExtraTools(ts ...tools.Tool) RunOption {
	return func(v *toolView) {
		for _, t := range ts {
			if t == nil {
				continue
			}
			if v.extra == nil {
				v.extra = tools.NewRegistry()
			}
			v.extra.Register(t)
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
	// A restricted view must carry a non-nil allow set so lookups don't panic;
	// an empty set correctly means "no registry tools".
	if v.restrict && v.allow == nil {
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

// hasExtra reports whether the named tool is attached for this run only.
func (r toolResolver) hasExtra(name string) bool {
	return r.view != nil && r.view.extra != nil && r.view.extra.GetTool(name) != nil
}

// visible reports whether the named tool is usable in this run. Run-scoped
// extras are always visible; registry tools are subject to the view's filter.
//
// Names the agent knows nothing about report false rather than deferring to the
// registry's own UnknownToolError, so that visible() answers "usable in this
// run" honestly — a run must never see another run's ephemeral extras.
//
// This is checked after lazyRegisterMCPTool on the execution paths, so a
// deferred MCP tool is already in the registry by the time we look.
func (r toolResolver) visible(name string) bool {
	if r.hasExtra(name) {
		return true
	}
	if r.view != nil && r.view.restrict && !r.view.allow[name] {
		return false
	}
	return r.reg.GetTool(name) != nil
}

// schemas returns the tool schemas visible to this run: registry tools (filtered
// by the view) in the registry's deterministic order — built-ins alphabetically,
// then MCP tools in registration order, so prompt caching stays stable —
// followed by any run-scoped extras.
//
// Extras go last so that attaching one doesn't shift the positions of the
// registry tools ahead of it, keeping the cacheable prefix intact.
func (r toolResolver) schemas() []tools.Schema {
	all := r.reg.GetSchemas()
	if r.view == nil {
		return all
	}

	var out []tools.Schema
	switch {
	case !r.view.restrict:
		out = make([]tools.Schema, 0, len(all)+1)
		out = append(out, all...)
	case len(r.view.allow) > 0:
		out = make([]tools.Schema, 0, len(r.view.allow)+1)
		for _, s := range all {
			if r.view.allow[s.Name] {
				out = append(out, s)
			}
		}
	}

	if r.view.extra != nil {
		// Drop registry entries an extra shadows: a duplicate tool name is a
		// protocol error with some providers, and the extra is the instance
		// that would actually run.
		kept := out[:0]
		for _, s := range out {
			if r.view.extra.GetTool(s.Name) == nil {
				kept = append(kept, s)
			}
		}
		out = append(kept, r.view.extra.GetSchemas()...)
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
	if r.hasExtra(name) {
		return r.view.extra.IsParallel(name)
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
	if r.hasExtra(name) {
		return r.view.extra.Invoke(ctx, name, args)
	}
	return r.reg.Invoke(ctx, name, args)
}

// executeConfirmed runs the confirmed phase of a ConfirmationTool, refusing
// tools hidden by the current view.
func (r toolResolver) executeConfirmed(ctx context.Context, name, args string) (string, error) {
	if !r.visible(name) {
		return "", &tools.UnknownToolError{Name: name}
	}
	if r.hasExtra(name) {
		return r.view.extra.ExecuteConfirmed(ctx, name, args)
	}
	return r.reg.ExecuteConfirmed(ctx, name, args)
}
