package acp

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Build a minimal mock ACP agent for testing buildPermissionHandler.
type mockACPAgent struct {
	acp.Agent
	initializeCalled bool
}

func (m *mockACPAgent) Initialize(_ context.Context, _ acp.InitializeRequest) (acp.InitializeResponse, error) {
	m.initializeCalled = true
	return acp.InitializeResponse{ProtocolVersion: acp.ProtocolVersionNumber}, nil
}

func TestBuildPermissionHandler_AllowOnce(t *testing.T) {
	// Two independent pipes: agent→client, client→agent
	agentToClientR, agentToClientW := io.Pipe()
	clientToAgentR, clientToAgentW := io.Pipe()

	mockAgent := &mockACPAgent{}
	conn := acp.NewAgentSideConnection(mockAgent, agentToClientW, clientToAgentR)
	t.Cleanup(func() { agentToClientW.Close(); clientToAgentW.Close() })

	// Build the handler
	aiAgent := newBareTestAgent(t, nil, 0)
	handler := buildPermissionHandler(conn, "test-session", aiAgent)

	// Goroutine simulates the ACP client reading the JSON-RPC request and sending a response
	go func() {
		var reqMap map[string]any
		decoder := json.NewDecoder(agentToClientR)
		err := decoder.Decode(&reqMap)
		require.NoError(t, err)

		// Correct format: outcome discriminator "selected" with optionId in camelCase
		response := map[string]any{
			"jsonrpc": "2.0",
			"id":      reqMap["id"],
			"result": map[string]any{
				"outcome": map[string]any{
					"outcome":  "selected",
					"optionId": "allow",
				},
			},
		}
		json.NewEncoder(clientToAgentW).Encode(response)
	}()

	approved, err := handler(t.Context(), "Bash", "tool-1", "diff content", "args here")
	assert.NoError(t, err)
	assert.True(t, approved)
}

func TestBuildPermissionHandler_Reject(t *testing.T) {
	agentToClientR, agentToClientW := io.Pipe()
	clientToAgentR, clientToAgentW := io.Pipe()

	mockAgent := &mockACPAgent{}
	conn := acp.NewAgentSideConnection(mockAgent, agentToClientW, clientToAgentR)
	t.Cleanup(func() { agentToClientW.Close(); clientToAgentW.Close() })

	aiAgent := newBareTestAgent(t, nil, 0)
	handler := buildPermissionHandler(conn, "test-session", aiAgent)

	go func() {
		var reqMap map[string]any
		json.NewDecoder(agentToClientR).Decode(&reqMap)
		response := map[string]any{
			"jsonrpc": "2.0",
			"id":      reqMap["id"],
			"result": map[string]any{
				"outcome": map[string]any{
					"outcome":  "selected",
					"optionId": "reject",
				},
			},
		}
		json.NewEncoder(clientToAgentW).Encode(response)
	}()

	// Use "Bash" instead of "EditFile" — EditFile auto-approves in ACP mode.
	approved, err := handler(t.Context(), "Bash", "tool-1", "diff", "args")
	assert.NoError(t, err)
	assert.False(t, approved)
}

func TestBuildPermissionHandler_AllowAll(t *testing.T) {
	agentToClientR, agentToClientW := io.Pipe()
	clientToAgentR, clientToAgentW := io.Pipe()

	mockAgent := &mockACPAgent{}
	conn := acp.NewAgentSideConnection(mockAgent, agentToClientW, clientToAgentR)
	t.Cleanup(func() { agentToClientW.Close(); clientToAgentW.Close() })

	aiAgent := newBareTestAgent(t, nil, 0)
	handler := buildPermissionHandler(conn, "test-session", aiAgent)

	go func() {
		var reqMap map[string]any
		json.NewDecoder(agentToClientR).Decode(&reqMap)
		response := map[string]any{
			"jsonrpc": "2.0",
			"id":      reqMap["id"],
			"result": map[string]any{
				"outcome": map[string]any{
					"outcome":  "selected",
					"optionId": "allow_all",
				},
			},
		}
		json.NewEncoder(clientToAgentW).Encode(response)
	}()

	approved, err := handler(t.Context(), "Bash", "tool-2", "diff", "args")
	assert.NoError(t, err)
	assert.True(t, approved)
}

func TestBuildPermissionHandler_Cancelled(t *testing.T) {
	agentToClientR, agentToClientW := io.Pipe()
	clientToAgentR, clientToAgentW := io.Pipe()

	mockAgent := &mockACPAgent{}
	conn := acp.NewAgentSideConnection(mockAgent, agentToClientW, clientToAgentR)
	t.Cleanup(func() { agentToClientW.Close(); clientToAgentW.Close() })

	aiAgent := newBareTestAgent(t, nil, 0)
	handler := buildPermissionHandler(conn, "test-session", aiAgent)

	go func() {
		var reqMap map[string]any
		json.NewDecoder(agentToClientR).Decode(&reqMap)
		response := map[string]any{
			"jsonrpc": "2.0",
			"id":      reqMap["id"],
			"result": map[string]any{
				"outcome": map[string]any{
					"outcome": "cancelled",
				},
			},
		}
		json.NewEncoder(clientToAgentW).Encode(response)
	}()

	approved, err := handler(t.Context(), "Bash", "tool-3", "diff", "args")
	assert.NoError(t, err)
	assert.False(t, approved)
}
