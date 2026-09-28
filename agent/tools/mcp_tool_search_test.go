package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// stubSearcher implements MCPSearcher for testing. It deliberately does NOT
// implement MCPServerLister: that is the "a searcher which cannot name its
// servers" case, where a `server` argument is passed through unvalidated.
type stubSearcher struct {
	results []MCPSearchResultItem
}

func (s *stubSearcher) Search(_ context.Context, opts MCPSearchOptions) []MCPSearchResultItem {
	return s.results
}

// listerSearcher is the shape both real searchers have: it can name its
// servers, so the tool rejects an unknown `server` instead of searching nothing.
type listerSearcher struct {
	stubSearcher
	servers []string
}

func (s *listerSearcher) Servers() []string { return s.servers }

// stubTracker implements MCPSearchTracker for testing. It also implements
// MCPSearchTrackerProvider, returning itself for any session ID — callers
// that want to observe per-session routing use stubSessionTrackerProvider.
type stubTracker struct {
	added []string
}

func (s *stubTracker) Add(name string) {
	s.added = append(s.added, name)
}

func (s *stubTracker) List() []string {
	return s.added
}

func (s *stubTracker) TrackerFor(sessionID string) MCPSearchTracker { return s }

func TestMCPSearchToolsTool_Name(t *testing.T) {
	tool := NewMCPSearchToolsTool(nil, nil)
	if tool.Name() != "MCPSearchTools" {
		t.Errorf("expected MCPSearchTools, got %s", tool.Name())
	}
}

func TestMCPSearchToolsTool_Description(t *testing.T) {
	tool := NewMCPSearchToolsTool(nil, nil)
	if tool.Description() == "" {
		t.Error("expected non-empty description")
	}
}

func TestMCPSearchToolsTool_Properties(t *testing.T) {
	tool := NewMCPSearchToolsTool(nil, nil)
	props := tool.Properties()
	if _, ok := props["query"]; !ok {
		t.Error("expected query property")
	}
	if _, ok := props["max_results"]; !ok {
		t.Error("expected max_results property")
	}
	// server is the narrowing argument; its values are dynamic (a server can
	// connect mid-session), so it is a plain string and must never be required.
	if _, ok := props["server"]; !ok {
		t.Error("expected server property")
	}
	for _, name := range tool.Required() {
		if name == "server" {
			t.Error("server must stay optional: a search without it is the whole pool")
		}
	}
}

func TestMCPSearchToolsTool_Required(t *testing.T) {
	tool := NewMCPSearchToolsTool(nil, nil)
	required := tool.Required()
	if len(required) != 1 || required[0] != "query" {
		t.Errorf("expected [query], got %v", required)
	}
}

func TestMCPSearchToolsTool_Parallel(t *testing.T) {
	tool := NewMCPSearchToolsTool(nil, nil)
	if !tool.Parallel() {
		t.Error("expected MCPSearchTools to be parallel")
	}
}

func TestMCPSearchToolsTool_Execute_Success(t *testing.T) {
	tool := NewMCPSearchToolsTool(
		&stubSearcher{
			results: []MCPSearchResultItem{
				{
					Name:        "mcp__pg__query",
					Description: "Query a PostgreSQL database",
				},
				{
					Name:        "mcp__pg__list_tables",
					Description: "List all tables",
				},
			},
		},
		&stubTracker{},
	)

	output, err := tool.ExecuteContext(t.Context(), `{"query": "select:mcp__pg__query,mcp__pg__list_tables", "max_results": 5}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("failed to parse output JSON: %v", err)
	}

	matches, ok := result["matches"].([]any)
	if !ok {
		t.Fatal("expected matches array")
	}
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(matches))
	}

	// Check tool_definitions are included
	toolDefs, ok := result["tool_definitions"].(map[string]any)
	if !ok {
		t.Fatal("expected tool_definitions map")
	}
	if _, ok := toolDefs["mcp__pg__query"]; !ok {
		t.Error("expected mcp__pg__query in tool_definitions")
	}
	if _, ok := toolDefs["mcp__pg__list_tables"]; !ok {
		t.Error("expected mcp__pg__list_tables in tool_definitions")
	}
}

func TestMCPSearchToolsTool_Execute_TrackerCalled(t *testing.T) {
	tracker := &stubTracker{}
	tool := NewMCPSearchToolsTool(
		&stubSearcher{
			results: []MCPSearchResultItem{
				{Name: "mcp__pg__query", Description: "Query DB"},
			},
		},
		tracker,
	)

	_, err := tool.ExecuteContext(t.Context(), `{"query": "select:mcp__pg__query"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(tracker.added) != 1 || tracker.added[0] != "mcp__pg__query" {
		t.Errorf("expected tracker.Add called with 'mcp__pg__query', got %v", tracker.added)
	}
}

func TestMCPSearchToolsTool_Execute_EmptyResults(t *testing.T) {
	tool := NewMCPSearchToolsTool(
		&stubSearcher{results: nil},
		&stubTracker{},
	)

	output, err := tool.ExecuteContext(t.Context(), `{"query": "nonexistent"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("failed to parse output JSON: %v", err)
	}

	matches, _ := result["matches"].([]any)
	if len(matches) != 0 {
		t.Errorf("expected 0 matches, got %d", len(matches))
	}

	total, _ := result["total_deferred_tools"].(float64)
	if total != 0 {
		t.Errorf("expected total_deferred_tools=0, got %v", total)
	}
}

func TestMCPSearchToolsTool_Execute_InvalidJSON(t *testing.T) {
	tool := NewMCPSearchToolsTool(nil, nil)
	_, err := tool.ExecuteContext(t.Context(), `not json`)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestMCPSearchToolsTool_Execute_MissingQuery(t *testing.T) {
	tool := NewMCPSearchToolsTool(nil, nil)
	_, err := tool.ExecuteContext(t.Context(), `{}`)
	if err == nil {
		t.Error("expected error for missing query")
	}
}

func TestMCPSearchToolsTool_Execute_EmptyQuery(t *testing.T) {
	tool := NewMCPSearchToolsTool(nil, nil)
	_, err := tool.ExecuteContext(t.Context(), `{"query": ""}`)
	if err == nil {
		t.Error("expected error for empty query")
	}
}

func TestMCPSearchToolsTool_Execute_DefaultMaxResults(t *testing.T) {
	// When max_results is not specified, should default to 5
	tool := NewMCPSearchToolsTool(
		&stubSearcher{results: nil},
		&stubTracker{},
	)

	output, err := tool.ExecuteContext(t.Context(), `{"query": "something"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("failed to parse output JSON: %v", err)
	}
	// Just verify it doesn't crash — default max_results=5 is handled by searcher
	if _, ok := result["query"]; !ok {
		t.Error("expected query in output")
	}
}

func TestMCPSearchToolsTool_SchemaToPropsJSON(t *testing.T) {
	props := map[string]PropertySchema{
		"name": {
			Type:        "string",
			Description: "Name field",
			Items:       nil,
		},
		"count": {
			Type:        "integer",
			Description: "Count field",
			Items:       map[string]any{"type": "number"},
		},
		"untyped": {
			Description: "no type keyword",
		},
	}

	result := schemaToPropsJSON(props)
	if _, ok := result["name"]; !ok {
		t.Error("expected name in result")
	}
	nameMap := result["name"].(map[string]any)
	if nameMap["type"] != "string" {
		t.Errorf("expected type string, got %v", nameMap["type"])
	}
	if nameMap["description"] != "Name field" {
		t.Errorf("expected 'Name field', got %v", nameMap["description"])
	}

	// Items should be included when present
	countMap := result["count"].(map[string]any)
	if countMap["items"] == nil {
		t.Error("expected items for count property")
	}

	// A typeless property must not report "type": "" — the schema shown to the
	// model has to match what the API request will carry.
	untypedMap := result["untyped"].(map[string]any)
	if _, ok := untypedMap["type"]; ok {
		t.Errorf("expected no type key for a typeless property, got %v", untypedMap)
	}
}

func TestIsMCPSchema(t *testing.T) {
	if !IsMCPSchema("mcp__pg__query") {
		t.Error("expected true for mcp__ prefix")
	}
	if IsMCPSchema("Bash") {
		t.Error("expected false for non-MCP tool")
	}
	if IsMCPSchema("") {
		t.Error("expected false for empty string")
	}
}

func TestIsMCPSearchTool(t *testing.T) {
	if !IsMCPSearchTool("MCPSearchTools") {
		t.Error("expected true for MCPSearchTools")
	}
	if IsMCPSearchTool("mcp__pg__query") {
		t.Error("expected false for MCP tool")
	}
	if IsMCPSearchTool("Bash") {
		t.Error("expected false for built-in tool")
	}
}

// recordingSearcher records the request it was called with, for clamping and
// parameter-passing tests.
type recordingSearcher struct {
	lastOpts MCPSearchOptions
	results  []MCPSearchResultItem
}

func (s *recordingSearcher) Search(_ context.Context, opts MCPSearchOptions) []MCPSearchResultItem {
	s.lastOpts = opts
	return s.results
}

func TestMCPSearchToolsTool_MaxResultsClamped(t *testing.T) {
	searcher := &recordingSearcher{}
	tool := NewMCPSearchToolsTool(searcher, &stubTracker{})

	// Out-of-range value must be clamped to maxMCPSearchMaxResults (20).
	if _, err := tool.ExecuteContext(context.TODO(), `{"query": "db", "max_results": 500}`); err != nil {
		t.Fatalf("ExecuteContext failed: %v", err)
	}
	if searcher.lastOpts.MaxResults != 20 {
		t.Errorf("expected max_results clamped to 20, got %d", searcher.lastOpts.MaxResults)
	}

	// <= 0 resets to the default 5.
	if _, err := tool.ExecuteContext(context.TODO(), `{"query": "db", "max_results": -1}`); err != nil {
		t.Fatalf("ExecuteContext failed: %v", err)
	}
	if searcher.lastOpts.MaxResults != 5 {
		t.Errorf("expected max_results default 5 for <= 0, got %d", searcher.lastOpts.MaxResults)
	}

	// In-range values pass through.
	if _, err := tool.ExecuteContext(context.TODO(), `{"query": "db", "max_results": 7}`); err != nil {
		t.Fatalf("ExecuteContext failed: %v", err)
	}
	if searcher.lastOpts.MaxResults != 7 {
		t.Errorf("expected max_results 7 passed through, got %d", searcher.lastOpts.MaxResults)
	}
}

// The server the model writes is normalized before it reaches the searcher —
// "mcp__GITHUB" and " github " are the same server as "github" — because the
// spelling in the reminder block and the spelling in a tool name are both things
// it can copy.
func TestMCPSearchToolsTool_ServerReachesTheSearcherNormalized(t *testing.T) {
	searcher := &recordingSearcher{}
	tool := NewMCPSearchToolsTool(searcher, &stubTracker{})

	for _, written := range []string{"github", "mcp__github", " GITHUB ", "mcp__GITHUB"} {
		args := `{"query": "find a story", "server": "` + written + `"}`
		if _, err := tool.ExecuteContext(context.TODO(), args); err != nil {
			t.Fatalf("server %q rejected: %v", written, err)
		}
		if searcher.lastOpts.Server != "github" {
			t.Errorf("server %q normalized to %q, want github", written, searcher.lastOpts.Server)
		}
	}

	// No server at all is not a server named "": the whole pool is searched.
	if _, err := tool.ExecuteContext(context.TODO(), `{"query": "find a story"}`); err != nil {
		t.Fatalf("ExecuteContext failed: %v", err)
	}
	if searcher.lastOpts.Server != "" {
		t.Errorf("expected an empty server, got %q", searcher.lastOpts.Server)
	}
}

// An unknown server is an error naming the ones that exist. A search over an
// empty candidate set would answer exactly like "no tool matches", and a model
// that reads it that way stops trying instead of fixing the name.
func TestMCPSearchToolsTool_UnknownServerIsRejectedWithTheList(t *testing.T) {
	tool := NewMCPSearchToolsTool(
		&listerSearcher{servers: []string{"alpha", "beta"}},
		&stubTracker{},
	)

	output, err := tool.ExecuteContext(context.TODO(), `{"query": "find a story", "server": "jira"}`)
	if err == nil {
		t.Fatalf("expected an error, got output %s", output)
	}
	if !strings.Contains(err.Error(), `"jira"`) {
		t.Errorf("the message must quote back what the model wrote, got %q", err)
	}
	if !strings.Contains(err.Error(), "alpha, beta") {
		t.Errorf("the message must carry the names that DO exist, got %q", err)
	}

	// A listed server (any casing, with or without the prefix) still goes through.
	if _, err := tool.ExecuteContext(context.TODO(), `{"query": "find a story", "server": "mcp__Beta"}`); err != nil {
		t.Fatalf("a connected server must be accepted: %v", err)
	}
}

// Nothing connected yet is its own answer: the name may well be right, there is
// simply nothing to search, and the model should retry without the parameter.
func TestMCPSearchToolsTool_ServerBeforeAnyConnection(t *testing.T) {
	tool := NewMCPSearchToolsTool(&listerSearcher{}, &stubTracker{})

	_, err := tool.ExecuteContext(context.TODO(), `{"query": "find a story", "server": "github"}`)
	if err == nil {
		t.Fatal("expected an error while no server is connected")
	}
	if !strings.Contains(err.Error(), "no MCP server is connected") {
		t.Errorf("unexpected message: %v", err)
	}
}

// A searcher that cannot name its servers does not validate: the search goes
// through and an unknown name matches nothing, which is the behaviour every
// caller had before the parameter existed.
func TestMCPSearchToolsTool_ServerPassedThroughWithoutALister(t *testing.T) {
	searcher := &recordingSearcher{}
	tool := NewMCPSearchToolsTool(searcher, &stubTracker{})

	if _, err := tool.ExecuteContext(context.TODO(), `{"query": "find a story", "server": "jira"}`); err != nil {
		t.Fatalf("an unlistable searcher must not reject a server: %v", err)
	}
	if searcher.lastOpts.Server != "jira" {
		t.Errorf("expected the server passed through, got %q", searcher.lastOpts.Server)
	}
}

func TestNormalizeMCPServer(t *testing.T) {
	for written, want := range map[string]string{
		"github":      "github",
		"mcp__github": "github",
		"  GITHUB  ":  "github",
		"MCP__GitLab": "gitlab", // the prefix itself is matched case-insensitively
		"":            "",
		"   ":         "",
	} {
		if got := NormalizeMCPServer(written); got != want {
			t.Errorf("NormalizeMCPServer(%q) = %q, want %q", written, got, want)
		}
	}
}

// stubSessionTrackerProvider routes TrackerFor by session ID, so tests can
// observe per-session discovery routing.
type stubSessionTrackerProvider struct {
	trackers map[string]*stubTracker
}

func (p *stubSessionTrackerProvider) TrackerFor(sessionID string) MCPSearchTracker {
	if p.trackers == nil {
		return nil
	}
	tr, ok := p.trackers[sessionID]
	if !ok {
		return nil // explicit nil interface — never box a typed nil
	}
	return tr
}

// queryFilterSearcher mimics the real DeferredPool's "select:" filtering so
// per-session routing tests can assert that only the requested tool is
// recorded.
type queryFilterSearcher struct {
	results []MCPSearchResultItem
}

func (s *queryFilterSearcher) Search(_ context.Context, opts MCPSearchOptions) []MCPSearchResultItem {
	sel, ok := strings.CutPrefix(opts.Query, "select:")
	if !ok {
		return s.results
	}
	var out []MCPSearchResultItem
	for _, name := range strings.Split(sel, ",") {
		for _, r := range s.results {
			if r.Name == name || strings.HasSuffix(r.Name, "__"+name) {
				out = append(out, r)
			}
		}
	}
	return out
}

func TestMCPSearchToolsTool_Execute_SessionRouting(t *testing.T) {
	provider := &stubSessionTrackerProvider{trackers: map[string]*stubTracker{
		"sess-a": {},
		"sess-b": {},
	}}
	searcher := &queryFilterSearcher{results: []MCPSearchResultItem{
		{Name: "mcp__pg__query", Description: "Query DB"},
		{Name: "mcp__pg__list", Description: "List tables"},
	}}
	tool := NewMCPSearchToolsTool(searcher, provider)

	// Session A discovers pg__query; session B discovers pg__list.
	args := `{"query": "select:mcp__pg__query"}`
	if _, err := tool.ExecuteContext(WithSessionID(t.Context(), "sess-a"), args); err != nil {
		t.Fatalf("sess-a execute failed: %v", err)
	}
	args = `{"query": "select:mcp__pg__list"}`
	if _, err := tool.ExecuteContext(WithSessionID(t.Context(), "sess-b"), args); err != nil {
		t.Fatalf("sess-b execute failed: %v", err)
	}

	if got := provider.trackers["sess-a"].added; len(got) != 1 || got[0] != "mcp__pg__query" {
		t.Errorf("sess-a discovered %v, want [mcp__pg__query]", got)
	}
	if got := provider.trackers["sess-b"].added; len(got) != 1 || got[0] != "mcp__pg__list" {
		t.Errorf("sess-b discovered %v, want [mcp__pg__list]", got)
	}

	// No session context: must not panic and must not record anywhere.
	if _, err := tool.ExecuteContext(context.Background(), `{"query": "select:mcp__pg__query"}`); err != nil {
		t.Fatalf("no-session execute failed: %v", err)
	}
	if got := provider.trackers["sess-a"].added; len(got) != 1 {
		t.Errorf("no-session call leaked into sess-a: %v", got)
	}
}
