package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// remoteShapeView is the tool policy of an a2a remote turn (node/runner.go): a fixed
// list of this software's own tools, plus MCP admitted BY SHAPE.
func remoteShapeView() *toolView {
	return buildToolView(applyRunOptions([]RunOption{
		WithToolSet("Bash", "ReadFile"),
		WithMCPTools(),
	}))
}

// The shape rule is what makes MCP reachable from a run whose tool set is a name
// list: the deferred tools are named at runtime (mcp__server__tool), so a list can
// never contain them, and the search tool that loads them carries the other half.
//
// The assertion that matters is the pair: a run with the rule sees BOTH the search
// tool and the pool's tools, while everything outside its names stays hidden.
func TestMCPToolsAreAdmittedByShapeInARestrictedRun(t *testing.T) {
	a := newResolverAgent("Bash", "ReadFile", "WriteFile", tools.ToolNameMCPSearchTools, "mcp__srv__a")
	res := a.resolve(withToolView(context.Background(), remoteShapeView()))

	assert.True(t, res.visible(tools.ToolNameMCPSearchTools), "the search tool is half the mechanism")
	assert.True(t, res.visible("mcp__srv__a"), "and a loaded tool is the other half")
	assert.False(t, res.visible("WriteFile"), "the name list still decides everything else")
	assert.Equal(t, []string{"Bash", tools.ToolNameMCPSearchTools, "ReadFile", "mcp__srv__a"},
		schemaNames(res.schemas()))
	assert.True(t, remoteShapeView().admitsMCP())
}

// Naming the search tool is NOT the capability, and this is the case that says so:
// such a run could search, while its own view drops every schema the search loads.
// A reminder built from "it named the search tool" would advertise exactly that
// broken chain, which is why admitsMCP asks about the shape rule alone.
func TestNamingTheSearchToolAloneDoesNotMakeARunAnMCPRun(t *testing.T) {
	a := newResolverAgent("Bash", tools.ToolNameMCPSearchTools, "mcp__srv__a")
	view := buildToolView(applyRunOptions([]RunOption{
		WithToolSet("Bash", tools.ToolNameMCPSearchTools),
	}))
	res := a.resolve(withToolView(context.Background(), view))

	assert.True(t, res.visible(tools.ToolNameMCPSearchTools), "an explicit name is still an explicit name")
	assert.False(t, res.visible("mcp__srv__a"), "but a loaded tool is not admitted by the shape rule")
	assert.False(t, view.admitsMCP(), "so the pool must not be advertised to this run")
}

// A tool the session already loaded (an earlier turn's search) reaches a run only
// if that run's view admits MCP — the discovered-set branch of filterActiveSchemas
// reads the pool directly, so a missing check there puts a schema into a request
// whose tool the executor would refuse as an unknown tool.
func TestADiscoveredMCPToolOnlyReachesARunThatAdmitsMCP(t *testing.T) {
	config.SetBaseDir(t.TempDir())
	t.Cleanup(func() { config.SetBaseDir("") })

	a := newResolverAgent("Bash", "ReadFile", tools.ToolNameMCPSearchTools, "mcp__srv__a")
	mgr := mcp.NewManager(t.Context(), nil, logger.Default())
	t.Cleanup(mgr.Close)
	a.Config.MCPManager = mgr

	const name = "mcp__srv__a"
	mgr.Pool().Add(&mcp.DeferredTool{Name: name, ServerName: "srv", Schema: tools.Schema{Name: name}})
	mgr.SetFor("sess").Add(name) // this session loaded it in an earlier turn

	// A run that hides MCP, in the same session: nothing may put it back.
	hiddenView := buildToolView(applyRunOptions([]RunOption{WithToolSet("Bash")}))
	hidden := a.resolve(withToolView(context.Background(), hiddenView))
	assert.Equal(t, []string{"Bash"}, schemaNames(a.filterActiveSchemas("sess", hidden, hidden.schemas())))

	// The remote-shaped run in that session: the loaded tool reaches the request, and
	// calling it is admitted (registering it on first use is the executor's half).
	open := a.resolve(withToolView(context.Background(), remoteShapeView()))
	assert.Equal(t, []string{"Bash", tools.ToolNameMCPSearchTools, "ReadFile", name},
		schemaNames(a.filterActiveSchemas("sess", open, open.schemas())))
	assert.True(t, open.visible(name))
}

// The reminder belongs to the agent while the tool view belongs to a run, so a
// restricted run must not be told about a pool it cannot load from — and it must not
// consume the once-per-session fire either, or the ordinary conversation that COULD
// use those tools is never told about them.
func TestDeferredReminderStaysSilentOnARunThatCannotUseMCP(t *testing.T) {
	a := newMCPTestAgent(t, config.DefaultConfig())
	a.attachSharedMCPReminder()
	a.DeferredPool().Add(&mcp.DeferredTool{
		Name:        "mcp__pg__query",
		ServerName:  "pg",
		Description: "Query the PostgreSQL database",
	})

	// A /commit-shaped run: Bash and nothing else.
	commit := withToolView(context.Background(), buildToolView(applyRunOptions(
		[]RunOption{WithToolSet(tools.ToolNameBash)})))
	if block := a.collectReminders(commit, a.buildReminderContext(true, false)); block != "" {
		t.Fatalf("a run that cannot load MCP tools was told about the pool:\n%s", block)
	}

	// The ordinary conversation is still told: the silenced run did not spend the fire.
	block := a.collectReminders(context.Background(), a.buildReminderContext(true, false))
	require.Contains(t, block, "mcp__pg__query")
}
