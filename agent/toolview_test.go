package agent

import (
	"context"
	"testing"

	"github.com/monsterxx03/tachi/agent/tools"
)

// TestBuildToolViewNilVsEmpty guards the single most dangerous distinction in
// this file: a nil *toolView means "unrestricted", while a view holding an
// empty allow set means "no tools at all". Confusing the two would silently
// grant full tool access to a run that explicitly asked to be restricted
// (e.g. /compact), which is precisely the failure mode the tool view exists
// to eliminate.
func TestBuildToolViewNilVsEmpty(t *testing.T) {
	if v := buildToolView(nil); v != nil {
		t.Fatalf("buildToolView(nil) = %+v, want nil (unrestricted)", v)
	}
	if v := buildToolView([]RunOption{}); v != nil {
		t.Fatalf("buildToolView(empty) = %+v, want nil (unrestricted)", v)
	}

	v := buildToolView([]RunOption{WithNoTools()})
	if v == nil {
		t.Fatal("WithNoTools produced a nil view — the run would be unrestricted")
	}
	if v.allow == nil {
		t.Fatal("WithNoTools produced a nil allow set — nil is the unrestricted sentinel")
	}
	if len(v.allow) != 0 {
		t.Fatalf("WithNoTools allow = %v, want empty", v.allow)
	}
}

// TestBuildToolViewSkipsNilOptions ensures a nil RunOption in the variadic
// slice can't panic the agent loop.
func TestBuildToolViewSkipsNilOptions(t *testing.T) {
	v := buildToolView([]RunOption{nil, WithToolSet("Bash"), nil})
	if v == nil || !v.allow["Bash"] {
		t.Fatalf("view = %+v, want Bash allowed", v)
	}
	if len(v.allow) != 1 {
		t.Fatalf("allow = %v, want only Bash", v.allow)
	}
}

// TestWithToolSetAccumulates verifies multiple WithToolSet options union
// rather than overwrite, so callers can compose restrictions.
func TestWithToolSetAccumulates(t *testing.T) {
	v := buildToolView([]RunOption{WithToolSet("Bash"), WithToolSet("ReadFile", "Grep")})
	for _, name := range []string{"Bash", "ReadFile", "Grep"} {
		if !v.allow[name] {
			t.Errorf("allow[%q] = false, want true", name)
		}
	}
	if len(v.allow) != 3 {
		t.Fatalf("allow = %v, want 3 entries", v.allow)
	}
}

func TestToolViewContextRoundTrip(t *testing.T) {
	ctx := context.Background()
	if got := toolViewFrom(ctx); got != nil {
		t.Fatalf("bare context yielded view %+v, want nil", got)
	}

	// Attaching a nil view must leave the context untouched rather than
	// storing a typed nil that later reads as "restricted to nothing".
	if got := toolViewFrom(withToolView(ctx, nil)); got != nil {
		t.Fatalf("withToolView(ctx, nil) yielded %+v, want nil", got)
	}

	v := buildToolView([]RunOption{WithToolSet("Bash")})
	if got := toolViewFrom(withToolView(ctx, v)); got != v {
		t.Fatalf("round trip returned %+v, want %+v", got, v)
	}
}

// newResolverAgent builds a bare agent with a populated registry. It avoids
// NewAIAgent's provider wiring — the resolver only needs toolRegistry.
func newResolverAgent(names ...string) *AIAgent {
	reg := tools.NewRegistry()
	for _, n := range names {
		reg.Register(&viewStubTool{name: n})
	}
	return &AIAgent{toolRegistry: reg}
}

func TestResolverUnrestrictedPassesThrough(t *testing.T) {
	a := newResolverAgent("Bash", "ReadFile")
	res := a.resolve(context.Background())

	if len(res.schemas()) != 2 {
		t.Fatalf("schemas = %d, want 2", len(res.schemas()))
	}
	if !res.visible("Bash") || !res.visible("ReadFile") {
		t.Fatal("unrestricted resolver hid a registered tool")
	}
}

func TestResolverWithToolSetHidesOthers(t *testing.T) {
	a := newResolverAgent("Bash", "ReadFile", "WriteFile")
	ctx := withToolView(context.Background(), buildToolView([]RunOption{WithToolSet("Bash")}))
	res := a.resolve(ctx)

	schemas := res.schemas()
	if len(schemas) != 1 || schemas[0].Name != "Bash" {
		t.Fatalf("schemas = %+v, want only Bash", schemas)
	}
	if !res.visible("Bash") {
		t.Error("Bash should be visible")
	}
	if res.visible("WriteFile") {
		t.Error("WriteFile should be hidden by the view")
	}

	// The registry itself must be untouched — this is the whole point of the
	// design: no save/restore, so nothing can leak or be forgotten.
	if len(a.toolRegistry.GetToolNames()) != 3 {
		t.Fatalf("registry mutated: %v", a.toolRegistry.GetToolNames())
	}
}

func TestResolverWithNoToolsHidesEverything(t *testing.T) {
	a := newResolverAgent("Bash", "ReadFile")
	ctx := withToolView(context.Background(), buildToolView([]RunOption{WithNoTools()}))
	res := a.resolve(ctx)

	if s := res.schemas(); len(s) != 0 {
		t.Fatalf("schemas = %+v, want none", s)
	}
	if res.visible("Bash") {
		t.Error("WithNoTools left Bash visible")
	}
}

// TestResolverRefusesHiddenToolInvocation covers the reason the view must also
// apply at execution time and not only when building schemas: a model may
// still emit a call for a tool it saw in an earlier turn (or hallucinate one).
func TestResolverRefusesHiddenToolInvocation(t *testing.T) {
	executed := false
	reg := tools.NewRegistry()
	reg.Register(&viewStubTool{name: "WriteFile", onExecute: func() { executed = true }})
	a := &AIAgent{toolRegistry: reg}

	ctx := withToolView(context.Background(), buildToolView([]RunOption{WithToolSet("Bash")}))
	tr := a.resolve(ctx).invoke(ctx, "WriteFile", "{}")

	if tr.Status != tools.ToolResultError {
		t.Fatalf("status = %v, want error", tr.Status)
	}
	if executed {
		t.Fatal("hidden tool was executed — the view must gate invoke, not just schemas")
	}
	if _, ok := tr.Err.(*tools.UnknownToolError); !ok {
		t.Fatalf("err = %T (%v), want *tools.UnknownToolError", tr.Err, tr.Err)
	}

	if _, err := a.resolve(ctx).executeConfirmed(ctx, "WriteFile", "{}"); err == nil {
		t.Fatal("executeConfirmed accepted a hidden tool")
	}
}

// TestResolverHiddenToolIsNotParallel documents why hidden tools report
// non-parallel: they are about to be refused, and batching them alongside
// genuinely parallel calls would only muddle result ordering.
func TestResolverHiddenToolIsNotParallel(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&viewStubTool{name: "Grep", parallel: true})
	reg.Register(&viewStubTool{name: "Glob", parallel: true})
	a := &AIAgent{toolRegistry: reg}

	if !a.resolve(context.Background()).isParallel("Grep") {
		t.Fatal("unrestricted: Grep should be parallel")
	}

	ctx := withToolView(context.Background(), buildToolView([]RunOption{WithToolSet("Grep")}))
	res := a.resolve(ctx)
	if !res.isParallel("Grep") {
		t.Error("visible parallel tool reported non-parallel")
	}
	if res.isParallel("Glob") {
		t.Error("hidden tool reported parallel")
	}
}

// TestResolverSchemaOrderIsStable pins the ordering guarantee that motivated
// this refactor. The previous implementation restored the registry by ranging
// over a map, and Registry rebuilds its MCP ordering from Register() call
// order — so MCP tools came back in a random order after every /commit or
// /compact, changing the tools array sent to the provider and invalidating
// prompt caches. A tool view never mutates the registry, so order is fixed.
func TestResolverSchemaOrderIsStable(t *testing.T) {
	a := newResolverAgent("Bash", "ReadFile",
		"mcp__srv__alpha", "mcp__srv__beta", "mcp__srv__gamma",
		"mcp__srv__delta", "mcp__srv__epsilon")

	want := []string{"Bash", "ReadFile",
		"mcp__srv__alpha", "mcp__srv__beta", "mcp__srv__gamma",
		"mcp__srv__delta", "mcp__srv__epsilon"}

	// Repeat with restricted runs interleaved: a view-scoped run must leave
	// the full ordering intact for the next unrestricted turn.
	for round := 0; round < 20; round++ {
		restricted := withToolView(context.Background(),
			buildToolView([]RunOption{WithToolSet("Bash")}))
		if s := a.resolve(restricted).schemas(); len(s) != 1 {
			t.Fatalf("round %d: restricted schemas = %d, want 1", round, len(s))
		}

		got := make([]string, 0, len(want))
		for _, s := range a.resolve(context.Background()).schemas() {
			got = append(got, s.Name)
		}
		for i := range want {
			if i >= len(got) || got[i] != want[i] {
				t.Fatalf("round %d: schema order = %v, want %v", round, got, want)
			}
		}
	}
}

// TestResolverPreservesRegistryOrderUnderView checks that filtering keeps the
// registry's relative order rather than the allow-map's (random) iteration
// order — otherwise restricted runs would themselves churn the prompt cache.
func TestResolverPreservesRegistryOrderUnderView(t *testing.T) {
	a := newResolverAgent("Bash", "Glob", "Grep", "ReadFile",
		"mcp__srv__a", "mcp__srv__b", "mcp__srv__c")

	view := buildToolView([]RunOption{
		WithToolSet("ReadFile", "mcp__srv__c", "Bash", "mcp__srv__a"),
	})
	want := []string{"Bash", "ReadFile", "mcp__srv__a", "mcp__srv__c"}

	for round := 0; round < 20; round++ {
		ctx := withToolView(context.Background(), view)
		got := make([]string, 0, len(want))
		for _, s := range a.resolve(ctx).schemas() {
			got = append(got, s.Name)
		}
		if len(got) != len(want) {
			t.Fatalf("round %d: got %v, want %v", round, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("round %d: order = %v, want %v", round, got, want)
			}
		}
	}
}

// --- WithExtraTools ---

func TestWithExtraToolsIsPurelyAdditive(t *testing.T) {
	v := buildToolView([]RunOption{WithExtraTools(&viewStubTool{name: "SendFile"})})
	if v == nil {
		t.Fatal("view is nil")
	}
	if v.restrict {
		t.Error("WithExtraTools alone must not restrict the registry")
	}
	if v.extra == nil || v.extra.GetTool("SendFile") == nil {
		t.Fatal("SendFile not attached")
	}
}

func TestExtraToolsVisibleAlongsideFullRegistry(t *testing.T) {
	a := newResolverAgent("Bash", "ReadFile")
	ctx := withToolView(context.Background(),
		buildToolView([]RunOption{WithExtraTools(&viewStubTool{name: "SendFile"})}))
	res := a.resolve(ctx)

	got := schemaNames(res.schemas())
	want := []string{"Bash", "ReadFile", "SendFile"}
	if !equalStrings(got, want) {
		t.Fatalf("schemas = %v, want %v", got, want)
	}
	if !res.visible("Bash") || !res.visible("SendFile") {
		t.Error("additive view hid a tool it should expose")
	}

	// The registry must not have gained the extra tool.
	if a.toolRegistry.GetTool("SendFile") != nil {
		t.Fatal("extra tool leaked into the agent's registry")
	}
}

// TestExtraToolsExemptFromToolSet documents the composition rule: attaching a
// tool is itself the statement that the run may use it, so extras don't also
// need to be named in WithToolSet.
func TestExtraToolsExemptFromToolSet(t *testing.T) {
	a := newResolverAgent("Bash", "ReadFile", "WriteFile")
	ctx := withToolView(context.Background(), buildToolView([]RunOption{
		WithToolSet("Bash"),
		WithExtraTools(&viewStubTool{name: "SendFile"}),
	}))
	res := a.resolve(ctx)

	got := schemaNames(res.schemas())
	want := []string{"Bash", "SendFile"}
	if !equalStrings(got, want) {
		t.Fatalf("schemas = %v, want %v", got, want)
	}
	if res.visible("WriteFile") {
		t.Error("WriteFile should stay hidden")
	}
}

func TestExtraToolsSurviveWithNoTools(t *testing.T) {
	a := newResolverAgent("Bash", "ReadFile")
	ctx := withToolView(context.Background(), buildToolView([]RunOption{
		WithNoTools(),
		WithExtraTools(&viewStubTool{name: "SendFile"}),
	}))

	got := schemaNames(a.resolve(ctx).schemas())
	if !equalStrings(got, []string{"SendFile"}) {
		t.Fatalf("schemas = %v, want [SendFile]", got)
	}
}

func TestExtraToolIsInvocable(t *testing.T) {
	executed := false
	a := newResolverAgent("Bash")
	ctx := withToolView(context.Background(), buildToolView([]RunOption{
		WithExtraTools(&viewStubTool{name: "SendFile", onExecute: func() { executed = true }}),
	}))

	tr := a.resolve(ctx).invoke(ctx, "SendFile", "{}")
	if tr.Status != tools.ToolResultSuccess {
		t.Fatalf("status = %v, err = %v; want success", tr.Status, tr.Err)
	}
	if !executed {
		t.Fatal("extra tool was not executed")
	}
}

// TestExtraToolShadowsRegistryEntry pins precedence: the run-scoped instance
// wins, and the shadowed registry entry must not also appear in the schemas
// (a duplicate tool name is a protocol error with some providers).
func TestExtraToolShadowsRegistryEntry(t *testing.T) {
	registryRan, extraRan := false, false
	reg := tools.NewRegistry()
	reg.Register(&viewStubTool{name: "SendFile", onExecute: func() { registryRan = true }})
	a := &AIAgent{toolRegistry: reg}

	ctx := withToolView(context.Background(), buildToolView([]RunOption{
		WithExtraTools(&viewStubTool{name: "SendFile", onExecute: func() { extraRan = true }}),
	}))
	res := a.resolve(ctx)

	if got := schemaNames(res.schemas()); !equalStrings(got, []string{"SendFile"}) {
		t.Fatalf("schemas = %v, want exactly one SendFile", got)
	}
	res.invoke(ctx, "SendFile", "{}")
	if !extraRan || registryRan {
		t.Fatalf("extraRan=%v registryRan=%v; the run-scoped tool must win", extraRan, registryRan)
	}
}

// TestExtraToolsAreInvisibleToConcurrentRuns is the property that makes this
// safe for channel mode, where one cached agent serves many threads: run A's
// ephemeral tools must not be reachable from run B. The old approach registered
// them on the shared agent, so an overlapping turn on another thread could see
// (and invoke) a tool bound to the first thread's attachment sink.
func TestExtraToolsAreInvisibleToConcurrentRuns(t *testing.T) {
	a := newResolverAgent("Bash")

	ctxA := withToolView(context.Background(),
		buildToolView([]RunOption{WithExtraTools(&viewStubTool{name: "SendFileA"})}))
	ctxB := withToolView(context.Background(),
		buildToolView([]RunOption{WithExtraTools(&viewStubTool{name: "SendFileB"})}))

	resA, resB := a.resolve(ctxA), a.resolve(ctxB)

	if !resA.visible("SendFileA") || resA.visible("SendFileB") {
		t.Error("run A sees the wrong extras")
	}
	if !resB.visible("SendFileB") || resB.visible("SendFileA") {
		t.Error("run B sees the wrong extras")
	}
	if tr := resB.invoke(ctxB, "SendFileA", "{}"); tr.Status != tools.ToolResultError {
		t.Error("run B was able to invoke run A's ephemeral tool")
	}

	// And a plain run (no view) sees neither.
	plain := a.resolve(context.Background())
	if plain.visible("SendFileA") || plain.visible("SendFileB") {
		t.Error("an unrestricted run must not see any run-scoped extras")
	}
	if got := schemaNames(plain.schemas()); !equalStrings(got, []string{"Bash"}) {
		t.Fatalf("plain schemas = %v, want [Bash]", got)
	}
}

// TestExtraToolsAppendAfterRegistry keeps extras at the tail so that attaching
// one doesn't shift the registry tools ahead of it, preserving the cacheable
// prompt prefix.
func TestExtraToolsAppendAfterRegistry(t *testing.T) {
	a := newResolverAgent("Bash", "ReadFile", "mcp__srv__a", "mcp__srv__b")
	view := buildToolView([]RunOption{WithExtraTools(
		&viewStubTool{name: "AAASendFile"}, // sorts first alphabetically
	)})

	want := []string{"Bash", "ReadFile", "mcp__srv__a", "mcp__srv__b", "AAASendFile"}
	for round := 0; round < 10; round++ {
		got := schemaNames(a.resolve(withToolView(context.Background(), view)).schemas())
		if !equalStrings(got, want) {
			t.Fatalf("round %d: schemas = %v, want %v", round, got, want)
		}
	}
}

func TestExtraToolParallelismComesFromTheExtra(t *testing.T) {
	a := newResolverAgent("Bash")
	ctx := withToolView(context.Background(), buildToolView([]RunOption{
		WithExtraTools(
			&viewStubTool{name: "ParTool", parallel: true},
			&viewStubTool{name: "SeqTool"},
		),
	}))
	res := a.resolve(ctx)

	if !res.isParallel("ParTool") {
		t.Error("parallel extra reported non-parallel")
	}
	if res.isParallel("SeqTool") {
		t.Error("non-parallel extra reported parallel")
	}
}

func TestWithExtraToolsIgnoresNil(t *testing.T) {
	v := buildToolView([]RunOption{WithExtraTools(nil, &viewStubTool{name: "SendFile"}, nil)})
	if v.extra == nil || v.extra.GetTool("SendFile") == nil {
		t.Fatal("SendFile not attached")
	}
	// A nil-only call must not fabricate an empty extra registry.
	if v2 := buildToolView([]RunOption{WithExtraTools(nil)}); v2.extra != nil {
		t.Error("nil-only WithExtraTools created an extra registry")
	}
}

// --- helpers ---

func schemaNames(ss []tools.Schema) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.Name)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- test stub ---

type viewStubTool struct {
	name      string
	parallel  bool
	onExecute func()
}

func (s *viewStubTool) Name() string        { return s.name }
func (s *viewStubTool) Description() string { return "stub tool " + s.name }
func (s *viewStubTool) Properties() map[string]tools.PropertySchema {
	return map[string]tools.PropertySchema{}
}
func (s *viewStubTool) Required() []string { return nil }
func (s *viewStubTool) Parallel() bool     { return s.parallel }
func (s *viewStubTool) ExecuteContext(_ context.Context, _ string) (string, error) {
	if s.onExecute != nil {
		s.onExecute()
	}
	return "ok", nil
}
