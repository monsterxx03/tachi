package mcp

import (
	"fmt"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testDeferredTool is a helper to create DeferredTool instances for tests.
func testDeferredTool(name, serverName, desc string) *DeferredTool {
	return &DeferredTool{
		Name:       name,
		ServerName: serverName,
		Description: desc,
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

func TestSearch_Exact(t *testing.T) {
	p := NewDeferredPool()
	p.Add(testDeferredTool("mcp__pg__query", "pg", "Query PG"))
	p.Add(testDeferredTool("mcp__pg__list", "pg", "List tables"))

	results := p.Search("exact:mcp__pg__query", 5)
	require.Equal(t, 1, len(results))
	assert.Equal(t, "mcp__pg__query", results[0].Name)
}

func TestSearch_Exact_NotFound(t *testing.T) {
	p := NewDeferredPool()
	results := p.Search("exact:nonexistent", 5)
	assert.Nil(t, results)
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

// ---------------------------------------------------------------------------
// Search — maxResults bounds
// ---------------------------------------------------------------------------

func TestSearch_MaxResultsBounds(t *testing.T) {
	p := NewDeferredPool()
	for i := 0; i < 50; i++ {
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
	// Note: name is lowercased before CamelCase splitting, so CamelCase
	// boundaries are lost. Single-segment CamelCase stays as a single word.
	parts := parseToolName("mcp__github__createPullRequest")
	assert.Equal(t, []string{"github", "createpullrequest"}, parts)
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
		{"+mustHave optional", []string{"+mustHave", "optional"}},
		{"+x", []string{"+x"}},           // single char after + preserved with + prefix
		{"", nil},
		{"a", []string{"a"}},             // single char is kept
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := tokenize(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ---------------------------------------------------------------------------
// matchesAny
// ---------------------------------------------------------------------------

func TestMatchesAny(t *testing.T) {
	tests := []struct {
		name        string
		term        string
		parts       []string
		description string
		searchHint  string
		want        bool
	}{
		{"exact name part", "query", []string{"postgres", "query"}, "", "", true},
		{"substring name part", "quer", []string{"postgres", "query"}, "", "", true},
		{"search hint", "pg", []string{"postgres", "query"}, "", "pg, database", true},
		{"description", "execute", []string{"postgres", "query"}, "Execute SQL", "", true},
		{"no match", "python", []string{"postgres", "query"}, "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesAny(tt.term, tt.parts, tt.description, tt.searchHint)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ---------------------------------------------------------------------------
// scoreTool
// ---------------------------------------------------------------------------

func TestScoreTool(t *testing.T) {
	parts := []string{"postgres", "query"}
	desc := "Execute SQL queries against PostgreSQL"
	hint := "postgres, query, sql, database"
	server := "postgres"

	// Server exact match
	assert.Greater(t, scoreTool("postgres", parts, desc, hint, server), 0)
	// Name part exact match
	assert.Greater(t, scoreTool("query", parts, desc, hint, server), 0)
	// Description match (lowest)
	score := scoreTool("execute", parts, desc, hint, server)
	assert.Greater(t, score, 0)
	// No match
	assert.Equal(t, 0, scoreTool("python", parts, desc, hint, server))
}

// ---------------------------------------------------------------------------
// contains
// ---------------------------------------------------------------------------

func TestContains(t *testing.T) {
	assert.True(t, contains([]string{"a", "b", "c"}, "a"))
	assert.True(t, contains([]string{"a", "b", "c"}, "A"))  // case insensitive
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
		for i := 0; i < 50; i++ {
			p.Add(testDeferredTool("mcp__x__tool", "x", ""))
		}
		done <- true
	}()

	go func() {
		for i := 0; i < 50; i++ {
			_ = p.Len()
			_ = p.Get("mcp__x__tool")
			_ = p.All()
		}
		done <- true
	}()

	go func() {
		for i := 0; i < 50; i++ {
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
