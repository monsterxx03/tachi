package mockllm_test

// Unit tests for the mock server's normalization, require assertions and
// script-consumption semantics (no build tag — run with `go test ./...`).

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/stretchr/testify/require"
)

func TestNormalizeOpenAIRequest(t *testing.T) {
	body := `{
		"model": "mock-model",
		"messages": [
			{"role": "system", "content": "You are tachi"},
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": "let me check", "reasoning_content": "thinking...",
			 "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "Bash", "arguments": "{\"command\":\"ls\"}"}}]},
			{"role": "tool", "tool_call_id": "call_1", "content": "README.md"}
		],
		"tools": [{"type": "function", "function": {"name": "Bash", "description": "run a command"}}]
	}`
	// normalize via the server handler is exercised elsewhere; here we drive
	// the parser indirectly by POSTing through the mock with a reply that
	// captures the request.
	mock := mockllm.NewServer()
	defer mock.Close()
	mock.Script(mockllm.Step{Reply: mockllm.Stream(mockllm.Done())})
	resp, err := http.Post(mock.BaseURL()+"/chat/completions", "application/json",
		strings.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	reqs := mock.Requests()
	require.Len(t, reqs, 1)
	msgs := reqs[0].Messages
	require.Len(t, msgs, 4)
	require.Equal(t, "system", msgs[0].Role)
	require.Contains(t, msgs[0].Content, "tachi")
	require.Equal(t, "user", msgs[1].Role)
	require.Equal(t, "assistant", msgs[2].Role)
	require.Equal(t, "thinking...", msgs[2].Thinking)
	require.Len(t, msgs[2].ToolCalls, 1)
	require.Equal(t, "call_1", msgs[2].ToolCalls[0].ID)
	require.Equal(t, "Bash", msgs[2].ToolCalls[0].Name)
	require.Equal(t, `{"command":"ls"}`, msgs[2].ToolCalls[0].Arguments)
	require.Equal(t, "tool", msgs[3].Role)
	require.Equal(t, "call_1", msgs[3].ToolCallID)
	require.Equal(t, "README.md", msgs[3].Content)
	require.Len(t, reqs[0].Tools, 1)
	require.Equal(t, "Bash", reqs[0].Tools[0].Name)
}

func TestNormalizeAnthropicRequest(t *testing.T) {
	body := `{
		"model": "mock-model",
		"system": [{"type": "text", "text": "You are tachi"}],
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "hi"}]},
			{"role": "assistant", "content": [
				{"type": "thinking", "thinking": "hmm", "signature": "sig-1"},
				{"type": "text", "text": "checking"},
				{"type": "tool_use", "id": "call_1", "name": "Bash", "input": {"command": "ls"}}
			]},
			{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "call_1",
			 "content": [{"type": "text", "text": "README.md"}], "is_error": false}]}
		],
		"tools": [{"name": "Bash", "description": "run a command"}]
	}`
	mock := mockllm.NewServer(mockllm.WithProtocol(mockllm.ProtocolAnthropic))
	defer mock.Close()
	mock.Script(mockllm.Step{Reply: mockllm.Stream(mockllm.Done())})
	resp, err := http.Post(mock.BaseURL()+"/v1/messages", "application/json",
		strings.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	msgs := mock.Requests()[0].Messages
	require.Len(t, msgs, 4)
	require.Equal(t, "system", msgs[0].Role)
	require.Contains(t, msgs[0].Content, "tachi")
	require.Equal(t, "user", msgs[1].Role)
	require.Equal(t, "hi", msgs[1].Content)
	require.Equal(t, "assistant", msgs[2].Role)
	require.Equal(t, "hmm", msgs[2].Thinking)
	require.Equal(t, "checking", msgs[2].Content)
	require.Len(t, msgs[2].ToolCalls, 1)
	require.Equal(t, "call_1", msgs[2].ToolCalls[0].ID)
	require.Equal(t, "Bash", msgs[2].ToolCalls[0].Name)
	require.Equal(t, `{"command": "ls"}`, msgs[2].ToolCalls[0].Arguments)
	require.Equal(t, "tool", msgs[3].Role)
	require.Equal(t, "call_1", msgs[3].ToolCallID)
	require.Equal(t, "README.md", msgs[3].Content)
	require.False(t, msgs[3].IsError)
	require.Len(t, mock.Requests()[0].Tools, 1)
	require.Equal(t, "Bash", mock.Requests()[0].Tools[0].Name)
}

// TestNormalizeAnthropicParallelToolResults locks the parallel-tool-result
// normalization: one user message carrying TWO tool_result blocks (the
// agent's collectToolMessages merges consecutive tool messages) must
// normalize into TWO tool messages — one per tool_use_id — so
// HasToolResult(call_1, ...) can match each result individually. Before the
// fix the merged message kept only the LAST block's ToolCallID, hiding
// call_1's result from the assertion surface.
func TestNormalizeAnthropicParallelToolResults(t *testing.T) {
	body := `{
		"model": "mock-model",
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "hi"}]},
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "call_1", "name": "Bash", "input": {"command": "echo one"}},
				{"type": "tool_use", "id": "call_2", "name": "Bash", "input": {"command": "echo two"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "call_1", "content": [{"type": "text", "text": "one"}]},
				{"type": "tool_result", "tool_use_id": "call_2", "content": [{"type": "text", "text": "two"}]}
			]}
		]
	}`
	mock := mockllm.NewServer(mockllm.WithProtocol(mockllm.ProtocolAnthropic))
	defer mock.Close()
	mock.Script(mockllm.Step{Reply: mockllm.Stream(mockllm.Done())})
	resp, err := http.Post(mock.BaseURL()+"/v1/messages", "application/json",
		strings.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	msgs := mock.Requests()[0].Messages
	// user + assistant(two tool_use) + tool(call_1) + tool(call_2)
	require.Len(t, msgs, 4)
	require.Equal(t, "assistant", msgs[1].Role)
	require.Len(t, msgs[1].ToolCalls, 2)

	require.Equal(t, "tool", msgs[2].Role)
	require.Equal(t, "call_1", msgs[2].ToolCallID)
	require.Equal(t, "one", msgs[2].Content)

	require.Equal(t, "tool", msgs[3].Role)
	require.Equal(t, "call_2", msgs[3].ToolCallID)
	require.Equal(t, "two", msgs[3].Content)

	// HasToolResult must now match BOTH calls.
	require.Empty(t, mockllm.HasToolResult("call_1", requireMatcher{substr: "one"})(mock.Requests()[0]))
	require.Empty(t, mockllm.HasToolResult("call_2", requireMatcher{substr: "two"})(mock.Requests()[0]))
}

// requireMatcher is a minimal Matcher implementation for tests that do not
// want to pull gomega into the core package tests.
type requireMatcher struct {
	substr string
}

func (m requireMatcher) Match(actual any) (bool, error) {
	s, ok := actual.(string)
	if !ok {
		return false, nil
	}
	return strings.Contains(s, m.substr), nil
}

func TestRequireAssertions(t *testing.T) {
	t.Run("has system prompt", func(t *testing.T) {
		mock := mockllm.NewServer()
		defer mock.Close()
		mock.Script(mockllm.Step{
			Require: mockllm.HasSystemPrompt(requireMatcher{substr: "tachi"}),
			Reply:   mockllm.Stream(mockllm.Text("ok"), mockllm.Finish("stop"), mockllm.Usage(1, 1), mockllm.Done()),
		})
		resp, err := http.Post(mock.BaseURL()+"/chat/completions", "application/json",
			strings.NewReader(`{"model":"m","messages":[{"role":"system","content":"You are tachi"},{"role":"user","content":"hi"}]}`))
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		resp.Body.Close()
		require.NoError(t, mock.Error())
	})

	t.Run("precondition failure is recorded", func(t *testing.T) {
		mock := mockllm.NewServer()
		defer mock.Close()
		mock.Script(mockllm.Step{
			Require: mockllm.HasSystemPrompt(requireMatcher{substr: "NOPE"}),
			Reply:   mockllm.Stream(mockllm.Done()),
		})
		resp, err := http.Post(mock.BaseURL()+"/chat/completions", "application/json",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		require.NoError(t, err)
		require.Equal(t, http.StatusPreconditionFailed, resp.StatusCode)
		resp.Body.Close()
		require.Error(t, mock.Error())
		require.Contains(t, mock.Error().Error(), "precondition failed")
	})

	t.Run("script exhaustion is recorded", func(t *testing.T) {
		mock := mockllm.NewServer()
		defer mock.Close()
		mock.Script(mockllm.Step{Reply: mockllm.Stream(mockllm.Text("a"), mockllm.Finish("stop"), mockllm.Usage(1, 1), mockllm.Done())})

		resp, err := http.Post(mock.BaseURL()+"/chat/completions", "application/json",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode) // first request consumes the step
		resp.Body.Close()

		resp, err = http.Post(mock.BaseURL()+"/chat/completions", "application/json",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		require.NoError(t, err)
		require.Equal(t, http.StatusInternalServerError, resp.StatusCode) // script exhausted
		resp.Body.Close()

		require.Error(t, mock.Error())
		require.Contains(t, mock.Error().Error(), "script exhausted")
	})

	t.Run("has tool result", func(t *testing.T) {
		mock := mockllm.NewServer()
		defer mock.Close()
		mock.Script(mockllm.Step{
			Require: mockllm.HasToolResult("call_1", requireMatcher{substr: "README.md"}),
			Reply:   mockllm.Stream(mockllm.Done()),
		})
		body := `{"model":"m","messages":[
			{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"README.md"}
		]}`
		resp, err := http.Post(mock.BaseURL()+"/chat/completions", "application/json",
			strings.NewReader(body))
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		resp.Body.Close()
		require.NoError(t, mock.Error())
	})
}

// TestRequestRecordTruncation: recorded RawBody is capped, but Require runs
// on the full parsed request so huge tool outputs never break assertions.
func TestRequestRecordTruncation(t *testing.T) {
	big := strings.Repeat("x", 200*1024)
	mock := mockllm.NewServer()
	defer mock.Close()
	mock.Script(mockllm.Step{
		Require: mockllm.HasToolResult("call_1", requireMatcher{substr: "needle"}),
		Reply:   mockllm.Stream(mockllm.Done()),
	})
	body := `{"model":"m","messages":[
		{"role":"tool","tool_call_id":"call_1","content":"` + big + `needle` + big + `"}
	]}`
	resp, err := http.Post(mock.BaseURL()+"/chat/completions", "application/json",
		strings.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	require.NoError(t, mock.Error())
	require.Less(t, len(mock.Requests()[0].RawBody), len(body))
}

func TestHasToolAssertions(t *testing.T) {
	mock := mockllm.NewServer()
	defer mock.Close()
	mock.Script(mockllm.Step{
		Require: mockllm.HasTool("Bash"),
		Reply:   mockllm.Stream(mockllm.Done()),
	})
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"Bash"}},{"type":"function","function":{"name":"ReadFile"}}]}`
	resp, err := http.Post(mock.BaseURL()+"/chat/completions", "application/json",
		strings.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	require.NoError(t, mock.Error())
}

func TestJSONReplyRendersValidResponses(t *testing.T) {
	mock := mockllm.NewServer()
	defer mock.Close()
	mock.Script(mockllm.Step{Reply: mockllm.JSON("answer", "reasoning")})
	resp, err := http.Post(mock.BaseURL()+"/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	resp.Body.Close()
	require.Equal(t, "answer", out.Choices[0].Message.Content)
	require.Equal(t, "reasoning", out.Choices[0].Message.ReasoningContent)
}
