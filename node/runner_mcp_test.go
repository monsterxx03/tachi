package node

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// A remote turn uses THIS MACHINE's MCP tools, on the wire and end to end.
//
// The chain has four links and each one is the reason this test is not a unit test: the host's
// config has an MCP server, the node's agent connects it and puts its tools in the deferred pool,
// the run's tool view admits MCP BY SHAPE (`agent.WithMCPTools` — the deferred names exist only at
// runtime), a search loads a tool, and the NEXT request's tool list carries it. Remove the shape
// rule and the search tool is filtered out of the first request, which is where this test fails.
func TestARemoteTurnUsesTheHostsMCPTools(t *testing.T) {
	base := nodeHome(t)
	// The node's own log is where the MCP init signals it finished (see waitForLogLine), and the
	// logger is process-global, so it is pointed at a temp dir like everything else here.
	logDir := t.TempDir()
	require.NoError(t, logger.Init(logDir, *logger.DefaultConfig()))

	stub := startStubMCPServer(t)

	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	// Turn 1 opens the conversation (and, on this side, builds the agent that connects the MCP
	// server). Turn 2 asks for the tool by describing what it does — the way a deferred tool is
	// meant to be reached — and turn 3 answers with what came back.
	mock.Script(append(
		append(
			oneMessageScript("这一轮不需要工具"),
			[]mockllm.Step{{Reply: mockllm.Stream(
				mockllm.ToolCallStart("search_1", tools.ToolNameMCPSearchTools, `{"query":"ping"}`),
				mockllm.Finish("tool_calls"),
				mockllm.Done(),
			)}}...),
		oneMessageScript("找到并使用了 stub 的工具")...,
	)...)

	cfg := nodeConfig(t, mock, filepath.Join(base, "workspace"))
	cfg.MCPServers = []config.MCPServerConfig{{
		Name: stubMCPServerName,
		Type: config.MCPTransportHTTP,
		URL:  stub.url,
		// The loader fills this in for a real config (config.DefaultMCPServerTimeout, see
		// config/mcp_json.go); a hand-built one has to, or the connect runs with a zero deadline
		// and fails before it leaves the machine.
		Timeout: config.DefaultMCPServerTimeout,
	}}
	h := startRealNode(t, cfg)
	client := newTestClient(t, h)

	// The pool fills in the BACKGROUND — a first message arriving before the servers finished
	// connecting is documented behaviour, not a bug — so the conversation is opened first and
	// then waits for the node's own completion signal. Sleeping and hoping would make this test
	// a report on how fast the machine is (design §7.1.1, and the reminder's own comment about a
	// pool that is still filling).
	first, err := client.SendMessage(context.Background(), sendRequest("你好", false))
	require.NoError(t, err)
	opened, ok := first.(*a2a.Task)
	require.True(t, ok)
	require.NotEmpty(t, opened.ContextID)
	waitForLogLine(t, filepath.Join(logDir, "debug.log"), "MCP: async init completed")

	// The same conversation continues: the pool this turn sees is the one that just filled.
	_, err = sendInContext(t, client, "列出这个 server 的工具并调用一次", opened.ContextID)
	require.NoError(t, err)

	requests := mock.Requests()
	require.Len(t, requests, 3, "one turn to open the conversation, then the search and its answer")

	// The search tool is offered — a name no list could have given it.
	require.True(t, offeredTools(requests[1])[tools.ToolNameMCPSearchTools],
		"a remote turn must be able to search this machine's MCP tools: %v", offeredTools(requests[1]))
	// ...while the tool itself is still deferred, exactly as it is for a local conversation.
	require.False(t, offeredTools(requests[1])[stubMCPToolFullName],
		"a deferred tool belongs to the pool, not to the request before a search loads it")

	// And once the search loaded it, it reaches the next request — the half that proves the
	// request filter and the tool view agree about MCP.
	require.True(t, offeredTools(requests[2])[stubMCPToolFullName],
		"the loaded tool must reach the next request: %v", offeredTools(requests[2]))
}

const (
	// stubMCPServerName is the stub server's name in the test config, and therefore the middle
	// segment of every tool name it contributes.
	stubMCPServerName = "stub"
	// stubMCPToolName is the tool the stub server exposes.
	stubMCPToolName = "ping"
	// stubMCPToolFullName is how that tool is named to the model.
	stubMCPToolFullName = "mcp__" + stubMCPServerName + "__" + stubMCPToolName
	// stubMCPFillerTools is how many filler tools the stub server adds on top of the one that
	// matters, so that this machine's config lands on the SEARCH path (see startStubMCPServer).
	stubMCPFillerTools = 6
)

// stubMCP is a stub MCP server listening over streamable HTTP: an httptest server, because this
// package's tests run in-process and the stdio transport would need the test binary to re-execute
// itself as the server.
type stubMCP struct{ url string }

func startStubMCPServer(t *testing.T) stubMCP {
	t.Helper()
	s := mcpserver.NewMCPServer("stub", "1.0.0")
	s.AddTool(
		mcpsdk.NewTool(stubMCPToolName, mcpsdk.WithDescription("ping the stub server")),
		func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			return mcpsdk.NewToolResultText("pong"), nil
		},
	)
	// Filler tools, and they are not decoration: with FEWER tools than
	// mcp_tool_search.min_tools_for_search every tool is auto-loaded (visible without a search),
	// so a one-tool server would test the auto-load path instead of the search one.
	for i := range stubMCPFillerTools {
		name := fmt.Sprintf("filler%d", i)
		s.AddTool(
			mcpsdk.NewTool(name, mcpsdk.WithDescription("a tool this test never calls")),
			func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				return mcpsdk.NewToolResultText("filler"), nil
			},
		)
	}
	// The streamable server's own endpoint path is /mcp, which is what the client's URL names.
	srv := httptest.NewServer(mcpserver.NewStreamableHTTPServer(s))
	t.Cleanup(srv.Close)
	return stubMCP{url: srv.URL + "/mcp"}
}

// offeredTools is the request's tool list as a set — what the model was actually given.
func offeredTools(req *mockllm.RecordedRequest) map[string]bool {
	out := make(map[string]bool, len(req.Tools))
	for _, tool := range req.Tools {
		out[tool.Name] = true
	}
	return out
}

// waitForLogLine waits until the log file contains want, or fails after a bounded wait.
func waitForLogLine(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the log %s never said %q", path, want)
}
