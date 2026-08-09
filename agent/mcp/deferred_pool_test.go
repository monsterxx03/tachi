package mcp

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testDeferredTool is a helper to create DeferredTool instances for tests.
func testDeferredTool(name, serverName, desc string) *DeferredTool {
	return &DeferredTool{
		Name:        name,
		ServerName:  serverName,
		Description: desc,
		nameParts:   parseToolName(name),
		descLower:   strings.ToLower(desc),
		hintLower:   "",
		serverLower: strings.ToLower(serverName),
	}
}

// ---------------------------------------------------------------------------
// DeferredPool — basic CRUD
// ---------------------------------------------------------------------------

func TestDeferredPool_AddAndGet(t *testing.T) {
	p := NewDeferredPool()
	p.Add(testDeferredTool("mcp__pg__query", "pg", "Query database"))
	p.Add(testDeferredTool("mcp__gh__pr", "gh", "Create PR"))

	tool := p.Get("mcp__pg__query")
	require.NotNil(t, tool)
	assert.Equal(t, "mcp__pg__query", tool.Name)
	assert.Equal(t, "pg", tool.ServerName)

	assert.Nil(t, p.Get("nonexistent"))
}

func TestDeferredPool_Len(t *testing.T) {
	p := NewDeferredPool()
	assert.Equal(t, 0, p.Len())

	p.Add(testDeferredTool("mcp__pg__query", "pg", ""))
	assert.Equal(t, 1, p.Len())

	p.Add(testDeferredTool("mcp__gh__pr", "gh", ""))
	assert.Equal(t, 2, p.Len())
}

func TestDeferredPool_All(t *testing.T) {
	p := NewDeferredPool()
	p.Add(testDeferredTool("mcp__z__tool", "z", ""))
	p.Add(testDeferredTool("mcp__a__tool", "a", ""))
	p.Add(testDeferredTool("mcp__m__tool", "m", ""))

	all := p.All()
	assert.Equal(t, 3, len(all))
	// All() sorts alphabetically by name
	assert.Equal(t, "mcp__a__tool", all[0].Name)
	assert.Equal(t, "mcp__m__tool", all[1].Name)
	assert.Equal(t, "mcp__z__tool", all[2].Name)
}

func TestDeferredPool_All_Empty(t *testing.T) {
	p := NewDeferredPool()
	assert.Empty(t, p.All())
}

// ---------------------------------------------------------------------------
// Search — edge cases
// ---------------------------------------------------------------------------

func TestSearch_EmptyQuery(t *testing.T) {
	p := NewDeferredPool()
	p.Add(testDeferredTool("mcp__a__tool", "a", ""))
	p.Add(testDeferredTool("mcp__b__tool", "b", ""))

	results := p.Search("", 1)
	assert.Equal(t, 1, len(results))
}

func TestSearch_SelectSingle(t *testing.T) {
	p := NewDeferredPool()
	p.Add(testDeferredTool("mcp__pg__query", "pg", "Query PG"))
	p.Add(testDeferredTool("mcp__pg__list", "pg", "List tables"))

	results := p.Search("select:mcp__pg__query", 5)
	require.Equal(t, 1, len(results))
	assert.Equal(t, "mcp__pg__query", results[0].Name)
}

func TestSearch_Select_NotFound(t *testing.T) {
	p := NewDeferredPool()
	results := p.Search("select:nonexistent", 5)
	assert.Empty(t, results)
}

func TestSearch_Select(t *testing.T) {
	p := NewDeferredPool()
	p.Add(testDeferredTool("mcp__pg__query", "pg", ""))
	p.Add(testDeferredTool("mcp__pg__list", "pg", ""))
	p.Add(testDeferredTool("mcp__gh__pr", "gh", ""))

	results := p.Search("select:mcp__pg__query,mcp__gh__pr", 5)
	require.Equal(t, 2, len(results))
	assert.Equal(t, "mcp__pg__query", results[0].Name)
	assert.Equal(t, "mcp__gh__pr", results[1].Name)
}

func TestSearch_Select_WithBlanks(t *testing.T) {
	p := NewDeferredPool()
	p.Add(testDeferredTool("mcp__a__tool", "a", ""))

	results := p.Search("select:,,mcp__a__tool,", 5)
	require.Equal(t, 1, len(results))
	assert.Equal(t, "mcp__a__tool", results[0].Name)
}

func TestSearch_Select_SuffixOnly(t *testing.T) {
	// select: with just the tool name (no mcp__server prefix) should match via suffix.
	p := NewDeferredPool()
	p.Add(testDeferredTool("mcp__alpha__switch_backend_endpoint", "alpha", "Switch backend"))
	p.Add(testDeferredTool("mcp__beta__SQL_submit_mutation", "beta", "Submit SQL mutation"))
	p.Add(testDeferredTool("mcp__gamma__list_users", "gamma", "List users"))

	results := p.Search("select:switch_backend_endpoint,SQL_submit_mutation", 5)
	require.Equal(t, 2, len(results))
	assert.Equal(t, "mcp__alpha__switch_backend_endpoint", results[0].Name)
	assert.Equal(t, "mcp__beta__SQL_submit_mutation", results[1].Name)
}

func TestSearch_Select_SuffixMatchesExactFirst(t *testing.T) {
	// Full name exact match takes priority over suffix (handled by Phase 1 first).
	p := NewDeferredPool()
	p.Add(testDeferredTool("mcp__alpha__echo", "alpha", "Echo service"))
	p.Add(testDeferredTool("mcp__beta__echo", "beta", "Echo service"))

	results := p.Search("select:mcp__alpha__echo", 5)
	require.Equal(t, 1, len(results))
	assert.Equal(t, "mcp__alpha__echo", results[0].Name)
}

func TestSearch_Select_SuffixMultipleServers(t *testing.T) {
	// Same tool name on different servers → suffix match returns all.
	p := NewDeferredPool()
	p.Add(testDeferredTool("mcp__alpha__echo", "alpha", "Echo"))
	p.Add(testDeferredTool("mcp__beta__echo", "beta", "Echo"))

	results := p.Search("select:echo", 5)
	require.Equal(t, 2, len(results))
	names := []string{results[0].Name, results[1].Name}
	assert.ElementsMatch(t, []string{"mcp__alpha__echo", "mcp__beta__echo"}, names)
}

func TestSearch_Select_Mixed(t *testing.T) {
	// Mix of full names and suffix-only names in a single select query.
	p := NewDeferredPool()
	p.Add(testDeferredTool("mcp__pg__query", "pg", "Query"))
	p.Add(testDeferredTool("mcp__gh__create_pr", "gh", "Create PR"))

	results := p.Search("select:mcp__pg__query,create_pr", 5)
	require.Equal(t, 2, len(results))
	names := []string{results[0].Name, results[1].Name}
	assert.ElementsMatch(t, []string{"mcp__pg__query", "mcp__gh__create_pr"}, names)
}

func TestSearch_ServerPrefix(t *testing.T) {
	p := NewDeferredPool()
	p.Add(testDeferredTool("mcp__pg__query", "pg", ""))
	p.Add(testDeferredTool("mcp__pg__list", "pg", ""))
	p.Add(testDeferredTool("mcp__gh__pr", "gh", ""))

	results := p.Search("mcp__pg", 5)
	require.Equal(t, 2, len(results))
	names := []string{results[0].Name, results[1].Name}
	assert.Contains(t, names, "mcp__pg__query")
	assert.Contains(t, names, "mcp__pg__list")
}

func TestSearch_ServerPrefix_NotFound(t *testing.T) {
	p := NewDeferredPool()
	p.Add(testDeferredTool("mcp__pg__query", "pg", ""))

	// No tool matches — falls through to keyword search
	results := p.Search("mcp__nonexistent", 5)
	assert.Empty(t, results)
}

func TestSearch_Keyword(t *testing.T) {
	p := NewDeferredPool()
	p.Add(testDeferredTool("mcp__pg__query", "pg", "Execute SQL queries against PostgreSQL"))
	p.Add(testDeferredTool("mcp__gh__pr", "gh", "Create and manage pull requests"))

	results := p.Search("query", 5)
	require.Equal(t, 1, len(results))
	assert.Equal(t, "mcp__pg__query", results[0].Name)
}

func TestSearch_Keyword_RequiredPrefix(t *testing.T) {
	p := NewDeferredPool()
	p.Add(testDeferredTool("mcp__pg__query", "pg", "Query the database"))
	p.Add(testDeferredTool("mcp__gh__pr", "gh", "Create pull requests"))

	results := p.Search("+pg query", 5)
	require.Equal(t, 1, len(results))
	assert.Equal(t, "mcp__pg__query", results[0].Name)
}

func TestSearch_Keyword_ScoreOrder(t *testing.T) {
	p := NewDeferredPool()
	p.Add(testDeferredTool("mcp__gh__query", "gh", "Query issues"))
	p.Add(testDeferredTool("mcp__pg__query", "pg", "Query database"))

	results := p.Search("query", 5)
	require.Equal(t, 2, len(results))
	// Equal score → sort order between equals is undefined
	names := []string{results[0].Name, results[1].Name}
	assert.ElementsMatch(t, []string{"mcp__gh__query", "mcp__pg__query"}, names)
}

func TestSearch_Keyword_ScoreWithServer(t *testing.T) {
	p := NewDeferredPool()
	// "gh" matches server name exactly (+15), "query" matches name part (+10) → total 25
	p.Add(testDeferredTool("mcp__gh__create_pr", "gh", "Create PRs"))
	// "query" matches name part (+10) → total 10
	p.Add(testDeferredTool("mcp__pg__query", "pg", "Query database"))

	results := p.Search("gh query", 5)
	require.Equal(t, 2, len(results))
	// gh__create_pr should score higher (server name + name part match)
	assert.Equal(t, "mcp__gh__create_pr", results[0].Name)
}

func TestSearch_Keyword_DedupTerms(t *testing.T) {
	// "+postgres postgres" should not double-count "postgres" in scoring.
	p := NewDeferredPool()
	// "postgres" matches server name → should be +15, not +30
	p.Add(testDeferredTool("mcp__pg__query", "postgres", "Query database"))
	p.Add(testDeferredTool("mcp__gh__pr", "gh", "Create pull requests"))

	results := p.Search("+postgres postgres", 5)
	require.Equal(t, 1, len(results))
	assert.Equal(t, "mcp__pg__query", results[0].Name)
}

// TestSearch_Keyword_SuffixOnly: user searches the tool name portion without "mcp__server" prefix.
// "get_mcp_server_detail" tokenizes to ["get", "mcp", "server", "detail"] and matches name parts.
func TestSearch_Keyword_SuffixOnly(t *testing.T) {
	p := NewDeferredPool()
	p.Add(testDeferredTool("mcp__iam-admin__get_mcp_server_detail", "iam-admin",
		"根据 MCP Server 名称查询详情：含 owner、可用范围、敏感标识、默认开关、可用条件规则、时间及更新人"))

	results := p.Search("get_mcp_server_detail", 5)
	require.Equal(t, 1, len(results))
	assert.Equal(t, "mcp__iam-admin__get_mcp_server_detail", results[0].Name)
}

func TestSearch_Keyword_SuffixOnly_CamelCase(t *testing.T) {
	// CamelCase query: "createPullRequest" should find the matching tool.
	p := NewDeferredPool()
	p.Add(testDeferredTool("mcp__github__createPullRequest", "github", "Create pull requests"))
	p.Add(testDeferredTool("mcp__github__listIssues", "github", "List issues"))

	results := p.Search("createPullRequest", 5)
	require.Equal(t, 1, len(results))
	assert.Equal(t, "mcp__github__createPullRequest", results[0].Name)
}

func TestSearch_Keyword_SuffixRequired(t *testing.T) {
	// +get_mcp_server_detail → all sub-terms required ("+get", "+mcp", "+server", "+detail")
	p := NewDeferredPool()
	p.Add(testDeferredTool("mcp__iam-admin__get_mcp_server_detail", "iam-admin", "Query MCP server"))
	p.Add(testDeferredTool("mcp__iam-admin__list_users", "iam-admin", "List users"))

	results := p.Search("+get_mcp_server_detail", 5)
	require.Equal(t, 1, len(results))
	assert.Equal(t, "mcp__iam-admin__get_mcp_server_detail", results[0].Name)
}

// ---------------------------------------------------------------------------
// Search — maxResults bounds
// ---------------------------------------------------------------------------

func TestSearch_MaxResultsBounds(t *testing.T) {
	p := NewDeferredPool()
	for i := range 50 {
		name := fmt.Sprintf("mcp__x__tool_%d", i)
		p.Add(testDeferredTool(name, "x", ""))
	}

	// Negative → defaults to 5
	r := p.Search("", -1)
	assert.Equal(t, 5, len(r))

	// > 20 → capped at 20
	r = p.Search("", 100)
	assert.Equal(t, 20, len(r))
}

func TestSearch_MaxResultsRespected(t *testing.T) {
	p := NewDeferredPool()
	p.Add(testDeferredTool("mcp__pg__query", "pg", ""))
	p.Add(testDeferredTool("mcp__pg__list", "pg", ""))
	p.Add(testDeferredTool("mcp__gh__pr", "gh", ""))

	results := p.Search("mcp__pg", 1)
	assert.Equal(t, 1, len(results))
}

// TestSearch_AcronymQuery_Regression verifies that all-uppercase acronyms like "SQL"
// are not split into individual letters, which previously caused keyword search to
// produce false matches for common letters like "s", "q", "l" across all tools,
// drowning out legitimate results.
func TestSearch_AcronymQuery_Regression(t *testing.T) {
	// Build a realistic pool with ~100 tools, including the two target tools.
	// The structure mirrors typical multi-server MCP setups:
	//   - "alpha" server: general infrastructure tools
	//   - "beta" server: database / SQL-related tools (triggers the acronym bug)
	//   - "gamma" server: document management tools
	p := NewDeferredPool()

	type toolSpec struct{ server, name, desc string }
	var specs []toolSpec

	// alpha server: ~15 general-purpose tools
	for i, name := range []string{
		"ping", "echo", "status", "version", "health_check",
		"list_users", "get_user", "create_user", "delete_user",
		"list_roles", "get_role", "assign_role",
		"get_config", "set_config", "reload_config",
	} {
		specs = append(specs, toolSpec{"alpha", name, "Alpha service: " + name + " #" + itoa(i)})
	}

	// alpha server: tools with "backend" / "endpoint" / "switch" in name — make the
	// target tool's tokens appear in other tools to ensure the search ranks correctly.
	for _, name := range []string{
		"get_backend_info", "list_endpoints", "switch_region",
		"update_backend_config", "register_endpoint",
	} {
		specs = append(specs, toolSpec{"alpha", name, "Alpha infra: " + name})
	}
	// Target 1 — the one that was being drowned out before the fix
	specs = append(specs, toolSpec{"alpha", "switch_backend_endpoint", "Switches to a different backend endpoint"})

	// beta server: ~20 SQL / database tools (the acronym trigger)
	for _, name := range []string{
		"SQL_query", "SQL_exec", "SQL_explain", "SQL_list_connections",
		"SQL_get_table_info", "SQL_list_schemas", "SQL_describe_table",
		"SQL_profile_query", "SQL_show_indexes", "SQL_analyze",
		"SQL_vacuum", "SQL_list_functions", "SQL_get_trigger",
		"SQL_export_csv", "SQL_import_data", "SQL_clone_table",
		"SQL_rename_column", "SQL_add_constraint",
	} {
		specs = append(specs, toolSpec{"beta", name, "Beta DB: " + name})
	}
	// Target 2 — the SQL tool with "submit" and "mutation" in the name
	specs = append(specs, toolSpec{"beta", "SQL_submit_sql_mutation", "Submits a SQL mutation request"})

	// beta server: non-SQL tools mixed in (so "sql" token isn't universal on beta)
	for _, name := range []string{
		"cache_flush", "cache_warm", "queue_drain", "queue_peek",
	} {
		specs = append(specs, toolSpec{"beta", name, "Beta ops: " + name})
	}

	// gamma server: ~40 document / table / article tools (bulk of the noise)
	gammaVerbs := []string{"get", "list", "create", "update", "delete", "search", "publish", "archive", "restore", "export"}
	gammaNouns := []string{"Document", "Article", "Spreadsheet", "Comment"}
	for _, noun := range gammaNouns {
		for _, verb := range gammaVerbs {
			name := verb + noun
			specs = append(specs, toolSpec{"gamma", name, "Gamma docs: " + name})
		}
	}

	require.GreaterOrEqual(t, len(specs), 80, "pool should have a realistic number of tools")

	for _, s := range specs {
		dt := NewDeferredToolFromMCPTool(MCPTool{
			serverName: s.server,
			serverTool: &mcp.Tool{
				Name:        s.name,
				Description: s.desc,
			},
		}, "")
		p.Add(dt)
	}

	query := "switch_backend_endpoint SQL_submit_sql_mutation"
	results := p.Search(query, 5)

	foundSwitch := false
	foundSQL := false
	for _, r := range results {
		if strings.Contains(r.Name, "switch_backend_endpoint") {
			foundSwitch = true
		}
		if strings.Contains(r.Name, "SQL_submit_sql_mutation") {
			foundSQL = true
		}
	}

	assert.True(t, foundSwitch, "switch_backend_endpoint should be in search results")
	assert.True(t, foundSQL, "SQL_submit_sql_mutation should be in search results")
}

// itoa is a tiny helper to avoid importing fmt in tests.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for n := i; n > 0; n /= 10 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
	}
	return string(digits)
}

// ---------------------------------------------------------------------------
// NewDeferredToolFromMCPTool
// ---------------------------------------------------------------------------

func TestNewDeferredToolFromMCPTool(t *testing.T) {
	mcpTool := MCPTool{
		serverName: "postgres",
		serverTool: &mcp.Tool{
			Name:        "query",
			Description: "Run a SQL query against the database",
			InputSchema: mcp.ToolInputSchema{
				Type: "object",
				Properties: map[string]any{
					"sql": map[string]any{
						"type":        "string",
						"description": "SQL query to execute",
					},
				},
				Required: []string{"sql"},
			},
		},
	}

	dt := NewDeferredToolFromMCPTool(mcpTool, "")
	require.NotNil(t, dt)
	assert.Equal(t, "mcp__postgres__query", dt.Name)
	assert.Equal(t, "postgres", dt.ServerName)
	assert.Equal(t, "Run a SQL query against the database", dt.Description)
	assert.NotNil(t, dt.Tool)
	// Schema should have the required fields
	assert.Equal(t, "object", dt.Schema.Parameters.Type)
	_, hasSQL := dt.Schema.Parameters.Properties["sql"]
	assert.True(t, hasSQL)
}

func TestNewDeferredToolFromMCPTool_SearchHintOverride(t *testing.T) {
	mcpTool := MCPTool{
		serverName: "postgres",
		serverTool: &mcp.Tool{
			Name:        "query",
			Description: "Run SQL",
		},
	}

	dt := NewDeferredToolFromMCPTool(mcpTool, "database, sql, query, postgres")
	assert.Equal(t, "database, sql, query, postgres", dt.SearchHint)
}

// ---------------------------------------------------------------------------
// buildSearchHint
// ---------------------------------------------------------------------------

func TestBuildSearchHint(t *testing.T) {
	mcpTool := MCPTool{
		serverName: "postgres",
		serverTool: &mcp.Tool{
			Name:        "list_tables",
			Description: "List all tables in the current database schema",
		},
	}

	hint := buildSearchHint(mcpTool)
	assert.Contains(t, hint, "postgres")
	assert.Contains(t, hint, "list")
	assert.Contains(t, hint, "tables")
	assert.Contains(t, hint, "database")
}

func TestBuildSearchHint_ShortDescription(t *testing.T) {
	mcpTool := MCPTool{
		serverName: "echo",
		serverTool: &mcp.Tool{
			Name:        "ping",
			Description: "Pings the server",
		},
	}

	hint := buildSearchHint(mcpTool)
	assert.Contains(t, hint, "echo")
	assert.Contains(t, hint, "ping")
	// "Pings the server" → "pings" (len>3), "the" (stop word, skipped), "server" (len>3)
	assert.Contains(t, hint, "server")
}

func TestBuildSearchHint_EmptyDescription(t *testing.T) {
	mcpTool := MCPTool{
		serverName: "test",
		serverTool: &mcp.Tool{
			Name:        "tool",
			Description: "",
		},
	}

	hint := buildSearchHint(mcpTool)
	assert.Equal(t, "test, tool", hint)
}

// ---------------------------------------------------------------------------
// parseToolName
// ---------------------------------------------------------------------------

func TestParseToolName_MCP(t *testing.T) {
	parts := parseToolName("mcp__postgres__query")
	assert.Equal(t, []string{"postgres", "query"}, parts)
}

func TestParseToolName_MCP_CamelCase(t *testing.T) {
	// CamelCase boundaries are now correctly detected (lowercased AFTER splitting).
	parts := parseToolName("mcp__github__createPullRequest")
	assert.Equal(t, []string{"github", "create", "pull", "request"}, parts)
}

func TestParseToolName_MCP_MultiSegment(t *testing.T) {
	parts := parseToolName("mcp__my_server__list_all_tables")
	assert.Equal(t, []string{"my", "server", "list", "all", "tables"}, parts)
}

func TestParseToolName_NonMCP(t *testing.T) {
	parts := parseToolName("Bash")
	assert.Equal(t, []string{"bash"}, parts)
}

func TestParseToolName_NonMCP_Camel(t *testing.T) {
	parts := parseToolName("EditFile")
	assert.Equal(t, []string{"editfile"}, parts)
}

func TestParseToolName_EdgeCases(t *testing.T) {
	// "mcp____" → rest is "_", split "__" → [""], splitOnUnderscoreOrCamel("") → nil
	parts := parseToolName("mcp____")
	assert.Nil(t, parts)

	// "mcp__x__" → split "__" → ["x", ""]
	assert.Equal(t, []string{"x"}, parseToolName("mcp__x__"))

	// "mcp____x" → split "__" → ["", "x"]
	assert.Equal(t, []string{"x"}, parseToolName("mcp____x"))
}

// ---------------------------------------------------------------------------
// splitOnUnderscoreOrCamel
// ---------------------------------------------------------------------------

func TestSplitOnUnderscoreOrCamel(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"simple", []string{"simple"}},
		{"two_words", []string{"two", "words"}},
		{"camelCase", []string{"camel", "case"}},
		{"CamelCase", []string{"camel", "case"}},
		{"mixed_case_words", []string{"mixed", "case", "words"}},
		{"", nil},
		// Acronym handling
		{"SQL", []string{"sql"}},
		{"HTTPServer", []string{"http", "server"}},
		{"getHTTPResponse", []string{"get", "http", "response"}},
		{"SQLServer", []string{"sql", "server"}},
		{"parseXMLFile", []string{"parse", "xml", "file"}},
		{"AA", []string{"aa"}},
		{"createPullRequest", []string{"create", "pull", "request"}},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := splitOnUnderscoreOrCamel(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ---------------------------------------------------------------------------
// tokenize
// ---------------------------------------------------------------------------

func TestTokenize(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"hello world", []string{"hello", "world"}},
		{"  spaced  out  ", []string{"spaced", "out"}},
		{"+mustHave optional", []string{"+must", "+have", "optional"}},
		{"++double", []string{"+double"}},  // multiple + stripped to single
		{"+++triple", []string{"+triple"}}, // multiple + stripped to single
		{"+x", []string{"+x"}},             // single char after + preserved with + prefix
		// Underscore splitting
		{"get_mcp_server_detail", []string{"get", "mcp", "server", "detail"}},
		{"+postgres_query", []string{"+postgres", "+query"}},
		// CamelCase splitting
		{"createPullRequest", []string{"create", "pull", "request"}},
		{"+listTables", []string{"+list", "+tables"}},
		// Mixed: whitespace + underscore
		{"iam get_mcp_server", []string{"iam", "get", "mcp", "server"}},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := tokenize(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ---------------------------------------------------------------------------
// parseSearchQuery
// ---------------------------------------------------------------------------

func TestParseSearchQuery(t *testing.T) {
	// "+" prefix marks a term as required; required terms also score.
	required, scoring := parseSearchQuery("+pg query")
	assert.Equal(t, []string{"pg"}, required)
	assert.Equal(t, []string{"pg", "query"}, scoring)

	// "+get_mcp_server_detail" → all four tokens required (underscore/camel
	// splitting happens in tokenize, before the "+" is stripped).
	required, scoring = parseSearchQuery("+get_mcp_server_detail")
	assert.Equal(t, []string{"get", "mcp", "server", "detail"}, required)
	assert.Equal(t, []string{"get", "mcp", "server", "detail"}, scoring)

	// Repeated terms (with or without "+") are deduplicated — bm25 would
	// otherwise double-count each occurrence.
	required, scoring = parseSearchQuery("+postgres postgres")
	assert.Equal(t, []string{"postgres"}, required)
	assert.Equal(t, []string{"postgres"}, scoring)

	// No usable terms.
	required, scoring = parseSearchQuery("++")
	assert.Empty(t, required)
	assert.Empty(t, scoring)
}

// ---------------------------------------------------------------------------
// tokenizeField
// ---------------------------------------------------------------------------

func TestTokenizeField(t *testing.T) {
	assert.Equal(t, []string{"postgres"}, tokenizeField("postgres"))
	assert.Equal(t, []string{"iam", "admin"}, tokenizeField("iam-admin"))
	assert.Equal(t, []string{"sql", "query", "against", "database"}, tokenizeField("SQL query against the database"))
	// Stop words removed
	assert.Equal(t, []string{"query", "database"}, tokenizeField("query the database"))
	// Underscore and CamelCase splitting
	assert.Equal(t, []string{"create", "pull", "request"}, tokenizeField("createPullRequest"))
	assert.Equal(t, []string{"get", "mcp", "server", "detail"}, tokenizeField("get_mcp_server_detail"))
	assert.Nil(t, tokenizeField(""))
}

// ---------------------------------------------------------------------------
// BM25 keyword ranking
// ---------------------------------------------------------------------------

// TestSearch_Keyword_BM25FieldWeights verifies the BM25 field boosts keep the
// previous ranking intent: a server-name match outranks a description-only
// match.
func TestSearch_Keyword_BM25FieldWeights(t *testing.T) {
	p := NewDeferredPool()
	p.Add(testDeferredTool("mcp__postgres__ping", "postgres", "Returns pong"))
	p.Add(testDeferredTool("mcp__x__query", "x", "Query the postgres database"))
	p.Add(testDeferredTool("mcp__y__ping", "y", "Query things"))

	results := p.Search("postgres", 5)
	require.Len(t, results, 2)
	// server-name match (server + name parts) ranks above description-only match
	assert.Equal(t, "mcp__postgres__ping", results[0].Name)
	assert.Equal(t, "mcp__x__query", results[1].Name)
}

// ---------------------------------------------------------------------------
// contains
// ---------------------------------------------------------------------------

func TestContains(t *testing.T) {
	assert.True(t, contains([]string{"a", "b", "c"}, "a"))
	assert.True(t, contains([]string{"a", "b", "c"}, "A")) // case insensitive
	assert.False(t, contains([]string{"a", "b", "c"}, "d"))
	assert.False(t, contains(nil, "a"))
}

// ---------------------------------------------------------------------------
// isStopWord
// ---------------------------------------------------------------------------

func TestIsStopWord(t *testing.T) {
	assert.True(t, isStopWord("the"))
	assert.True(t, isStopWord("THE"))
	assert.True(t, isStopWord("and"))
	assert.False(t, isStopWord("database"))
	assert.False(t, isStopWord("query"))
	assert.False(t, isStopWord(""))
}

// ---------------------------------------------------------------------------
// Concurrency sanity
// ---------------------------------------------------------------------------

func TestDeferredPool_ConcurrentAccess(t *testing.T) {
	p := NewDeferredPool()
	done := make(chan bool)

	go func() {
		for range 50 {
			p.Add(testDeferredTool("mcp__x__tool", "x", ""))
		}
		done <- true
	}()

	go func() {
		for range 50 {
			_ = p.Len()
			_ = p.Get("mcp__x__tool")
			_ = p.All()
		}
		done <- true
	}()

	go func() {
		for range 50 {
			p.Search("x", 5)
		}
		done <- true
	}()

	<-done
	<-done
	<-done
	// No race — run with -race to verify
	assert.True(t, true)
}
