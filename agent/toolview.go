package agent

import (
	"context"
	"time"

	"github.com/monsterxx03/tachi/agent/systemreminder"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/llm"
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
type toolView struct {
	// restrict limits the visible registry tools to allow. When false, the
	// whole registry is visible (extras still apply).
	restrict bool
	allow    map[string]bool

	// mcp admits this agent's MCP tools BY SHAPE rather than by name: the deferred tools
	// ("mcp__server__tool") and the search tool that loads them.
	//
	// A name set cannot express them, and that is the whole reason this field exists: what the
	// pool holds is a RUNTIME fact — servers connect in the background, a profile switch or a
	// toggle adds and removes them — so a run that wanted them would have to freeze a list that
	// is wrong a moment later. Off by default: a restricted run stays exactly as narrow as the
	// names it was given unless it asks for these (WithMCPTools).
	mcp bool

	// extra holds run-scoped tools that are not in the agent's registry, e.g.
	// channel mode's per-turn SendFileTool, whose callback closure captures a
	// fresh attachment sink and so cannot be shared across turns.
	extra *tools.Registry
}

// runParams is the parsed RunOption set for a single run.
// It embeds toolView for tool visibility control and adds fields
// for additional per-run configuration (pending images, steer channel,
// one-off recording).
type runParams struct {
	toolView
	pendingImages []llm.ContentPart // run 开始时附到首条用户消息的图片
	steerCh       chan SteerInput   // steer 输入（nil = 前端不支持 steer）
	steerTimeout  time.Duration     // 等待 TUI steer 响应的上限（0 = 默认 defaultSteerTimeout）
	oneoffMeta    *OneOffMeta       // one-off 转录（nil = 不录制）
	// displayUserMessage is the user's own text for this turn, when the caller TRANSFORMED
	// it before handing it over (@-file expansion). Empty = the message was sent as typed.
	// Session records keep it as DisplayContent so a reloaded transcript shows what the user
	// typed; the model still receives userMessage. See session.Message.DisplayContent.
	displayUserMessage string
	// noAutoCompact turns the loop's auto-compaction off for this run — set through
	// CompactRunOptions, which is the only thing that may set it.
	noAutoCompact bool
	// delegator is this machine's sending side, when the run was given one (WithDelegation).
	// It is the run's half of the delegation gate: the TOOLS are its extras, and the reminders
	// that describe nodes and outstanding work are generated from it — so a run without it can
	// neither call the tools nor be told about them.
	delegator tools.RemoteDelegator
}

// WithDelegation gives a run the delegation tools and the reminders that describe them.
//
// One option rather than two, on purpose: a run that can call DelegateTask but is never told
// which nodes exist — or one told about work it has no tools to manage — is a model with half a
// picture, and two options passed by hand at four call sites is how that drift starts.
//
// This IS the gate (§10.2.1): a turn that must not delegate (cron, an ambient or non-directed
// turn, a one-off command run) simply never passes it, and because the tools travel as run-scoped
// extras rather than registry entries, there is nothing for such a turn to un-register or
// accidentally inherit.
func WithDelegation(d tools.RemoteDelegator) RunOption {
	return func(p *runParams) {
		if d == nil {
			return
		}
		p.delegator = d
		WithExtraTools(tools.NewDelegateTaskTool(d), tools.NewRemoteTaskTool(d, 0))(p)
	}
}

// SteerInput represents pending user input to inject at the steer point,
// optionally with images.
type SteerInput struct {
	Text   string
	Images []llm.ContentPart
	// Display is the user's own text when it differs from Text (@-file expansion applied
	// to the steered text). Display only — see session.Message.DisplayContent.
	Display string
}

// RunOption customises a single RunConversationStream / RunOneOffStream call.
type RunOption func(*runParams)

// applyRunOptions folds RunOptions into a runParams. Returns nil when no
// options were given, so the unrestricted path stays allocation-free.
func applyRunOptions(opts []RunOption) *runParams {
	if len(opts) == 0 {
		return nil
	}
	p := &runParams{}
	for _, opt := range opts {
		if opt != nil {
			opt(p)
		}
	}
	// A restricted view must carry a non-nil allow set so lookups don't panic;
	// an empty set correctly means "no registry tools".
	if p.restrict && p.allow == nil {
		p.allow = make(map[string]bool)
	}
	return p
}

// WithToolSet restricts the run to the named tools. Tools outside the set stay
// in the registry but are hidden from the LLM and refused by the executor.
func WithToolSet(names ...string) RunOption {
	return func(p *runParams) {
		p.restrict = true
		if p.allow == nil {
			p.allow = make(map[string]bool, len(names))
		}
		for _, n := range names {
			p.allow[n] = true
		}
	}
}

// admits is the ONE answer to "may this run use this name", and it is what
// visible, schemas, isParallel and the MCP request filter all consult. Three
// copies of the rule is how they stop agreeing — and the copy that drifts is
// always the one nobody looks at.
//
// A nil view (the common, unrestricted run) admits everything. Names come from
// the run's own set; MCP names additionally come from the shape rule when the
// run asked for them.
func (v *toolView) admits(name string) bool {
	if v == nil || !v.restrict {
		return true
	}
	if v.allow[name] {
		return true
	}
	return v.mcp && (tools.IsMCPSchema(name) || tools.IsMCPSearchTool(name))
}

// admitsMCP reports whether this run can use MCP at all: it may search for a deferred
// tool AND call what the search hands back.
//
// Only the shape rule answers yes, and that is the point rather than strictness: the
// capability is the PAIR. A run that named the search tool alone would be allowed to
// search while its own view drops every loaded schema, so a reminder built from "it
// named the search tool" would advertise a mechanism with a missing second half. (An
// explicitly named MCP tool is usable without any of this — it just cannot be searched
// for, and there is nothing to advertise about the rest of the pool.)
//
// It is what a reminder asks before describing the pool (DeferredToolGate), and it is
// stated once here so the reminder and the request filter cannot disagree.
func (v *toolView) admitsMCP() bool {
	if v == nil || !v.restrict {
		return true
	}
	return v.mcp
}

// WithMCPTools admits this agent's MCP tools into a restricted run: the deferred
// tools of the pool, and the search tool that loads them.
//
// It is what a run that must DO work with this machine's integrations asks for —
// the a2a remote turn (node/runner.go) is the one that does. It composes with
// WithToolSet (the sets union), so a caller states the ordinary names it wants
// and adds this for the shape it cannot name. On a run with no restriction it
// changes nothing, and says so rather than pretending: every tool is already
// admitted there, and buildToolView drops the view entirely (see its rule).
func WithMCPTools() RunOption {
	return func(p *runParams) {
		p.mcp = true
	}
}

// WithNoTools hides every tool for the duration of the run.
func WithNoTools() RunOption {
	return func(p *runParams) {
		p.restrict = true
		if p.allow == nil {
			p.allow = make(map[string]bool)
		}
	}
}

// CompactRunOptions are the per-run options every COMPACT run must carry, and the only place that
// sets noAutoCompact. A compact run is an ordinary conversation turn whose user message IS the
// compact instruction (the model has to see the whole history to summarise it), so without the
// second half the loop is free to auto-compact at its own top — and it is precisely the history
// that crossed the threshold which brings a reader to /compact.
//
// When that happened, the instruction was GONE from the request the summarising call made
// (maybeAutoCompact had replaced rs.Messages with the summary) and the model saw 「请基于以上摘要
// 继续对话。」 instead: it answered a continuation, and the frontend — which collects that reply as
// its summary — compacted a SECOND time, into a child whose "summary" is the continuation. With
// every tool hidden that continuation also arrives as the model's native tool-call markup in
// plain text, a request with no tools having nothing to parse it against (the DSML leak).
//
// So both halves matter: WithNoTools keeps the summary text-only, and noAutoCompact makes it
// exactly one compaction per /compact, with a summary that is a summary.
func CompactRunOptions() []RunOption {
	return []RunOption{
		WithNoTools(),
		func(p *runParams) { p.noAutoCompact = true },
	}
}

// WithExtraTools attaches run-scoped tools that are not in the agent's registry.
func WithExtraTools(ts ...tools.Tool) RunOption {
	return func(p *runParams) {
		for _, t := range ts {
			if t == nil {
				continue
			}
			if p.extra == nil {
				p.extra = tools.NewRegistry()
			}
			p.extra.Register(t)
		}
	}
}

// WithPendingImages attaches image content parts to the initial user message
// for this run.
func WithPendingImages(imgs []llm.ContentPart) RunOption {
	return func(p *runParams) {
		p.pendingImages = imgs
	}
}

// WithSteerChannel sets the channel for steer input injection during this run.
// The frontend (TUI/ACP/channel) writes SteerInput values to this channel at
// steer points. Pass nil to disable steer for this run.
func WithSteerChannel(ch chan SteerInput) RunOption {
	return func(p *runParams) {
		p.steerCh = ch
	}
}

// WithSteerTimeout overrides how long the loop waits for a steer response
// before continuing without steer. Zero (the default) uses
// defaultSteerTimeout. Mainly used by tests to avoid waiting the full default.
func WithSteerTimeout(d time.Duration) RunOption {
	return func(p *runParams) {
		p.steerTimeout = d
	}
}

// WithDisplayUserMessage records the user's OWN text for this turn when the caller
// transformed it before calling the agent (today: @-file expansion, which the frontends
// apply themselves). The session keeps it as DisplayContent, so a reloaded transcript shows
// `@path` instead of the inlined file the model was given. The model still receives the
// transformed message — this option never changes what is sent.
func WithDisplayUserMessage(text string) RunOption {
	return func(p *runParams) {
		p.displayUserMessage = text
	}
}

// WithOneOffMeta attaches one-off transcription metadata to this run. When
// non-nil, the run's execution is recorded to a sidecar JSONL file. Pass nil
// (the default) to disable recording.
func WithOneOffMeta(meta *OneOffMeta) RunOption {
	return func(p *runParams) {
		p.oneoffMeta = meta
	}
}

// delegatorForRun is the run's sending side, or nil — a run with no options at all (the common
// case) has no params and therefore no delegation.
func (p *runParams) delegatorForRun() systemreminder.RemoteTaskProvider {
	if p == nil || p.delegator == nil {
		return nil
	}
	return p.delegator
}

// buildToolView extracts the toolView from runParams. Returns nil when params
// is nil or the toolView is empty (no restriction, no extras).
func buildToolView(p *runParams) *toolView {
	if p == nil {
		return nil
	}
	if !p.restrict && p.extra == nil {
		return nil
	}
	return &p.toolView
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
// over touching a.Config.ToolRegistry directly on execution paths, so that per-run
// tool restrictions are honoured.
func (a *AIAgent) resolve(ctx context.Context) toolResolver {
	return toolResolver{reg: a.Config.ToolRegistry, view: toolViewFrom(ctx)}
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
	if !r.view.admits(name) {
		return false
	}
	return r.reg.GetTool(name) != nil
}

// admits reports whether this run may use the name AT ALL — the view's rule, plus
// the run's own extras, which the registry never sees.
//
// It differs from visible in exactly one way, and the difference is load-bearing:
// it does not require the tool to be in the registry yet. The request builder
// (filterActiveSchemas) needs that, because a deferred MCP tool is admitted
// BEFORE its first call registers it — that registration is what lazyRegisterMCPTool
// does at invocation time.
func (r toolResolver) admits(name string) bool {
	return r.hasExtra(name) || r.view.admits(name)
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
	case len(r.view.allow) > 0 || r.view.mcp:
		out = make([]tools.Schema, 0, len(r.view.allow)+1)
		for _, s := range all {
			if r.view.admits(s.Name) {
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
