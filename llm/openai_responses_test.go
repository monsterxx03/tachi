package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3/responses"
)

// --- helpers ---------------------------------------------------------------

// newResponsesMockServer starts an httptest server that records the last
// request body and serves the given handler.
func newResponsesMockServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *[]byte) {
	t.Helper()
	var lastBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			w.WriteHeader(404)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(400)
			return
		}
		lastBody = body
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &lastBody
}

func newTestResponsesProvider(t *testing.T, srv *httptest.Server) *OpenAIResponsesProvider {
	t.Helper()
	return NewOpenAIResponsesProvider("sk-test", srv.URL, "gpt-5.6")
}

// decodeBody parses a captured request body into a generic map.
func decodeBody(t *testing.T, data *[]byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(*data, &m); err != nil {
		t.Fatalf("failed to decode request body %q: %v", string(*data), err)
	}
	return m
}

// writeSSE writes an SSE event and flushes it.
func writeSSE(w http.ResponseWriter, data string) {
	fmt.Fprintf(w, "data: %s\n\n", data)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func completedResponseJSON(output string, inputTokens, outputTokens int64) string {
	return fmt.Sprintf(`{"id":"resp_1","object":"response","created_at":1785578300,"status":"completed","model":"gpt-5.6","output":[%s],"usage":{"input_tokens":%d,"output_tokens":%d,"input_tokens_details":{"cached_tokens":7,"cache_write_tokens":3},"output_tokens_details":{"reasoning_tokens":1},"total_tokens":%d}}`,
		output, inputTokens, outputTokens, inputTokens+outputTokens)
}

// --- non-streaming ---------------------------------------------------------

func TestOpenAIResponsesCreateChat_Text(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, completedResponseJSON(
			`{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Hello!","annotations":[]}]}`,
			10, 5))
	}
	srv, body := newResponsesMockServer(t, handler)
	p := newTestResponsesProvider(t, srv)

	resp, err := p.CreateChat(context.Background(), []Message{
		{Role: "user", Content: "Hi"},
	}, nil, ChatOptions{MaxTokens: 4096})
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}

	if resp.Content != "Hello!" {
		t.Errorf("Content = %q, want %q", resp.Content, "Hello!")
	}
	if resp.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", resp.FinishReason)
	}
	if resp.Usage == nil {
		t.Fatal("Usage is nil")
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 5 {
		t.Errorf("Usage = %+v, want input=10 output=5", resp.Usage)
	}
	if resp.Usage.CacheReadInputTokens != 7 || resp.Usage.CacheCreationInputTokens != 3 {
		t.Errorf("cache usage = %+v, want read=7 creation=3", resp.Usage)
	}

	// Request body assertions.
	req := decodeBody(t, body)
	if req["model"] != "gpt-5.6" {
		t.Errorf("model = %v", req["model"])
	}
	if req["store"] != false {
		t.Errorf("store = %v, want false (stateless)", req["store"])
	}
	if req["temperature"] != float64(1) {
		t.Errorf("temperature = %v, want 1", req["temperature"])
	}
	if req["max_output_tokens"] != float64(4096) {
		t.Errorf("max_output_tokens = %v, want 4096", req["max_output_tokens"])
	}
	if _, ok := req["instructions"]; ok {
		t.Errorf("instructions should be omitted when no system messages, got %v", req["instructions"])
	}
	if req["stream"] == true {
		t.Error("stream should not be set for non-streaming CreateChat")
	}

	input, ok := req["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("input = %v, want 1 item", req["input"])
	}
	first, _ := input[0].(map[string]any)
	if first["role"] != "user" || first["content"] != "Hi" || first["type"] != "message" {
		t.Errorf("input[0] = %v", first)
	}
}

func TestOpenAIResponsesCreateChat_ToolCall(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, completedResponseJSON(
			`{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_9","name":"Bash","arguments":"{\"command\":\"ls\"}"}`,
			20, 8))
	}
	srv, _ := newResponsesMockServer(t, handler)
	p := newTestResponsesProvider(t, srv)

	resp, err := p.CreateChat(context.Background(), []Message{
		{Role: "user", Content: "list files"},
	}, nil, ChatOptions{})
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}

	if resp.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, want tool_calls", resp.FinishReason)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %d, want 1", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_9" || tc.Function.Name != "Bash" || tc.Function.Arguments != `{"command":"ls"}` {
		t.Errorf("ToolCall = %+v", tc)
	}
}

func TestOpenAIResponsesCreateChat_SystemToInstructions(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, completedResponseJSON(
			`{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ok","annotations":[]}]}`,
			10, 2))
	}
	srv, body := newResponsesMockServer(t, handler)
	p := newTestResponsesProvider(t, srv)

	_, err := p.CreateChat(context.Background(), []Message{
		{Role: "system", Content: "You are tachi."},
		{Role: "system", Content: "Be concise."},
		{Role: "user", Content: "hi"},
	}, nil, ChatOptions{})
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}

	req := decodeBody(t, body)
	if got := req["instructions"]; got != "You are tachi.\nBe concise." {
		t.Errorf("instructions = %q", got)
	}
	input, _ := req["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input = %v, system messages must not appear in input", req["input"])
	}
}

func TestOpenAIResponses_ReasoningParam(t *testing.T) {
	// effort passthrough (+ summary requested for non-streaming reasoning)
	{
		handler := func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, completedResponseJSON(`{"id":"m","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"x","annotations":[]}]}`, 1, 1))
		}
		srv, body := newResponsesMockServer(t, handler)
		p := newTestResponsesProvider(t, srv)
		thinking := true
		_, err := p.CreateChat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil,
			ChatOptions{Thinking: &thinking, ThinkingEffort: "xhigh"})
		if err != nil {
			t.Fatal(err)
		}
		req := decodeBody(t, body)
		reasoning, _ := req["reasoning"].(map[string]any)
		if reasoning["effort"] != "xhigh" {
			t.Errorf("reasoning.effort = %v, want xhigh", reasoning["effort"])
		}
		if reasoning["summary"] != "auto" {
			t.Errorf("reasoning.summary = %v, want auto", reasoning["summary"])
		}
	}

	// explicitly disabled thinking on a reasoning model → effort "none"
	{
		handler := func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, completedResponseJSON(`{"id":"m","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"x","annotations":[]}]}`, 1, 1))
		}
		srv, body := newResponsesMockServer(t, handler)
		p := newTestResponsesProvider(t, srv)
		thinking := false
		_, err := p.CreateChat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil,
			ChatOptions{Thinking: &thinking, ThinkingEffort: "high"})
		if err != nil {
			t.Fatal(err)
		}
		req := decodeBody(t, body)
		reasoning, ok := req["reasoning"].(map[string]any)
		if !ok {
			t.Fatalf("reasoning should be present with effort=none when thinking is disabled, got %v", req["reasoning"])
		}
		if reasoning["effort"] != "none" {
			t.Errorf("reasoning.effort = %v, want none", reasoning["effort"])
		}
	}

	// explicitly disabled thinking on a non-reasoning model → no reasoning
	// field (the server would reject the param for models that don't reason)
	{
		handler := func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, completedResponseJSON(`{"id":"m","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"x","annotations":[]}]}`, 1, 1))
		}
		srv, body := newResponsesMockServer(t, handler)
		p := NewOpenAIResponsesProvider("sk-test", srv.URL, "gpt-4o")
		thinking := false
		_, err := p.CreateChat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil,
			ChatOptions{Thinking: &thinking})
		if err != nil {
			t.Fatal(err)
		}
		req := decodeBody(t, body)
		if _, ok := req["reasoning"]; ok {
			t.Errorf("reasoning should be omitted on non-reasoning models, got %v", req["reasoning"])
		}
	}

	// explicitly enabled with default effort → summary only, no effort field
	{
		handler := func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, completedResponseJSON(`{"id":"m","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"x","annotations":[]}]}`, 1, 1))
		}
		srv, body := newResponsesMockServer(t, handler)
		p := newTestResponsesProvider(t, srv)
		thinking := true
		_, err := p.CreateChat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil,
			ChatOptions{Thinking: &thinking})
		if err != nil {
			t.Fatal(err)
		}
		req := decodeBody(t, body)
		reasoning, ok := req["reasoning"].(map[string]any)
		if !ok {
			t.Fatalf("reasoning should be present when thinking is enabled, got %v", req["reasoning"])
		}
		if _, hasEffort := reasoning["effort"]; hasEffort {
			t.Errorf("reasoning.effort should be omitted (provider default), got %v", reasoning["effort"])
		}
		if reasoning["summary"] != "auto" {
			t.Errorf("reasoning.summary = %v, want auto", reasoning["summary"])
		}
	}
}

func TestOpenAIResponses_RequestInputShape(t *testing.T) {
	// Full history replay: user → assistant(tool calls) → tool → user.
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, completedResponseJSON(`{"id":"m","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"done","annotations":[]}]}`, 1, 1))
	}
	srv, body := newResponsesMockServer(t, handler)
	p := newTestResponsesProvider(t, srv)

	tools := []Tool{
		NewTool("Bash", "Run a command", map[string]ToolParameterProperty{
			"command": {Type: "string", Description: "the command"},
		}, []string{"command"}),
	}
	_, err := p.CreateChat(context.Background(), []Message{
		{Role: "user", Content: "list files"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "Bash", Arguments: `{"command":"ls"}`}},
		}},
		{Role: "tool", ToolCallID: "call_1", Content: "file1.txt", Name: "Bash"},
		{Role: RoleSteer, Content: "Continue"},
	}, tools, ChatOptions{})
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}

	req := decodeBody(t, body)
	input, _ := req["input"].([]any)
	if len(input) != 5 {
		t.Fatalf("input len = %d, want 5 (user, assistant-msg, function_call, function_call_output, steer-user)", len(input))
	}
	byType := map[string]map[string]any{}
	for _, it := range input {
		m := it.(map[string]any)
		byType[m["type"].(string)] = m
	}
	fc, ok := byType["function_call"]
	if !ok {
		t.Fatalf("function_call item missing: %v", input)
	}
	if fc["call_id"] != "call_1" || fc["name"] != "Bash" || fc["arguments"] != `{"command":"ls"}` {
		t.Errorf("function_call = %v", fc)
	}
	fco, ok := byType["function_call_output"]
	if !ok {
		t.Fatalf("function_call_output item missing: %v", input)
	}
	if fco["call_id"] != "call_1" || fco["output"] != "file1.txt" {
		t.Errorf("function_call_output = %v", fco)
	}

	// tools: strict mode + JSON schema passthrough
	toolList, _ := req["tools"].([]any)
	if len(toolList) != 1 {
		t.Fatalf("tools = %v", req["tools"])
	}
	tool, _ := toolList[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "Bash" || tool["strict"] != true {
		t.Errorf("tool = %v", tool)
	}
	params, _ := tool["parameters"].(map[string]any)
	if params["type"] != "object" {
		t.Errorf("tool.parameters = %v", params)
	}
	if reqs, ok := params["required"].([]any); !ok || len(reqs) != 1 || reqs[0] != "command" {
		t.Errorf("tool.parameters.required = %v", params["required"])
	}
}

// --- streaming -------------------------------------------------------------

func TestOpenAIResponsesCreateChatStream_Text(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, `{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","status":"in_progress","role":"assistant","content":[]}}`)
		writeSSE(w, `{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"Hi"}`)
		writeSSE(w, `{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":" there"}`)
		writeSSE(w, `{"type":"response.output_item.done","output_index":0,"item":{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Hi there","annotations":[]}]}}`)
		writeSSE(w, `{"type":"response.completed","response":`+strings.TrimPrefix(completedResponseJSON(`{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Hi there","annotations":[]}]}`, 10, 4), "")+`}`)
	}
	srv, _ := newResponsesMockServer(t, handler)
	p := newTestResponsesProvider(t, srv)

	ch, err := p.CreateChatStream(context.Background(), []Message{
		{Role: "user", Content: "Hi"},
	}, nil, ChatOptions{})
	if err != nil {
		t.Fatalf("CreateChatStream: %v", err)
	}

	var deltas []string
	var done *StreamEvent
	for ev := range ch {
		switch ev.Type {
		case StreamEventTextDelta:
			deltas = append(deltas, ev.TextDelta)
		case StreamEventDone:
			done = &ev
		case StreamEventError:
			t.Fatalf("unexpected stream error: %v", ev.Error)
		}
	}

	if len(deltas) != 2 || strings.Join(deltas, "") != "Hi there" {
		t.Errorf("text deltas = %v", deltas)
	}
	if done == nil {
		t.Fatal("no Done event")
	}
	if done.FinishReason != "stop" {
		t.Errorf("Done.FinishReason = %q, want stop", done.FinishReason)
	}
	if done.Usage == nil || done.Usage.OutputTokens != 4 {
		t.Errorf("Done.Usage = %+v", done.Usage)
	}
}

func TestOpenAIResponsesCreateChatStream_ToolCall(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, `{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","status":"in_progress","call_id":"call_7","name":"Bash","arguments":""}}`)
		writeSSE(w, `{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"command\":"}`)
		writeSSE(w, `{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"\"ls\"}"}`)
		writeSSE(w, `{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_7","name":"Bash","arguments":"{\"command\":\"ls\"}"}}`)
		writeSSE(w, `{"type":"response.completed","response":`+strings.TrimPrefix(completedResponseJSON(`{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_7","name":"Bash","arguments":"{\"command\":\"ls\"}"}`, 20, 6), "")+`}`)
	}
	srv, _ := newResponsesMockServer(t, handler)
	p := newTestResponsesProvider(t, srv)

	ch, err := p.CreateChatStream(context.Background(), []Message{
		{Role: "user", Content: "list files"},
	}, nil, ChatOptions{})
	if err != nil {
		t.Fatalf("CreateChatStream: %v", err)
	}

	var toolStart *StreamEvent
	var argDeltas []StreamEvent
	var done *StreamEvent
	for ev := range ch {
		switch ev.Type {
		case StreamEventToolUseStart:
			ev := ev
			toolStart = &ev
		case StreamEventInputJSONDelta:
			argDeltas = append(argDeltas, ev)
		case StreamEventDone:
			done = &ev
		case StreamEventError:
			t.Fatalf("unexpected stream error: %v", ev.Error)
		}
	}

	if toolStart == nil {
		t.Fatal("no ToolUseStart event")
	}
	if toolStart.ToolCall == nil || toolStart.ToolCall.ID != "call_7" || toolStart.ToolCall.Function.Name != "Bash" {
		t.Errorf("ToolUseStart = %+v", toolStart)
	}
	if len(argDeltas) != 2 {
		t.Fatalf("arg deltas = %d, want 2", len(argDeltas))
	}
	for _, d := range argDeltas {
		if d.ToolIndex != 0 {
			t.Errorf("delta ToolIndex = %d, want 0", d.ToolIndex)
		}
	}
	if got := argDeltas[0].InputDelta + argDeltas[1].InputDelta; got != `{"command":"ls"}` {
		t.Errorf("assembled arguments = %q", got)
	}
	if done == nil || done.FinishReason != "tool_calls" {
		t.Errorf("Done = %+v, want finish_reason tool_calls", done)
	}
}

func TestOpenAIResponsesCreateChatStream_ThinkingDelta(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, `{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","status":"in_progress"}}`)
		writeSSE(w, `{"type":"response.reasoning_text.delta","item_id":"rs_1","output_index":0,"content_index":0,"delta":"thinking..."}`)
		writeSSE(w, `{"type":"response.output_item.added","output_index":1,"item":{"id":"msg_1","type":"message","status":"in_progress","role":"assistant","content":[]}}`)
		writeSSE(w, `{"type":"response.output_text.delta","item_id":"msg_1","output_index":1,"content_index":0,"delta":"answer"}`)
		writeSSE(w, `{"type":"response.completed","response":`+strings.TrimPrefix(completedResponseJSON(`{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"answer","annotations":[]}]}`, 15, 7), "")+`}`)
	}
	srv, _ := newResponsesMockServer(t, handler)
	p := newTestResponsesProvider(t, srv)

	ch, err := p.CreateChatStream(context.Background(), []Message{
		{Role: "user", Content: "think"},
	}, nil, ChatOptions{})
	if err != nil {
		t.Fatalf("CreateChatStream: %v", err)
	}

	var thinking, text []string
	for ev := range ch {
		switch ev.Type {
		case StreamEventThinkingDelta:
			thinking = append(thinking, ev.ThinkingDelta)
		case StreamEventTextDelta:
			text = append(text, ev.TextDelta)
		case StreamEventError:
			t.Fatalf("unexpected stream error: %v", ev.Error)
		}
	}

	if len(thinking) != 1 || thinking[0] != "thinking..." {
		t.Errorf("thinking deltas = %v", thinking)
	}
	if len(text) != 1 || text[0] != "answer" {
		t.Errorf("text deltas = %v", text)
	}
}

func TestOpenAIResponsesCreateChatStream_ServerError(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		io.WriteString(w, `{"error":{"message":"boom","type":"server_error","code":"server_error"}}`)
	}
	srv, _ := newResponsesMockServer(t, handler)
	p := newTestResponsesProvider(t, srv)

	ch, err := p.CreateChatStream(context.Background(), []Message{
		{Role: "user", Content: "hi"},
	}, nil, ChatOptions{})
	if err != nil {
		t.Fatalf("CreateChatStream should return the stream, got error: %v", err)
	}

	select {
	case ev := <-ch:
		if ev.Type != StreamEventError {
			t.Fatalf("first event type = %q, want error", ev.Type)
		}
		if ev.Error == nil {
			t.Fatal("Error event has nil error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for error event")
	}
}

func TestOpenAIResponsesCreateChatStream_CompletedWithoutUsage(t *testing.T) {
	// response.completed with zeroed usage → Done carries nil Usage.
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, `{"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.6","output":[],"usage":{"input_tokens":0,"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":0}}}`)
	}
	srv, _ := newResponsesMockServer(t, handler)
	p := newTestResponsesProvider(t, srv)

	ch, err := p.CreateChatStream(context.Background(), []Message{
		{Role: "user", Content: "hi"},
	}, nil, ChatOptions{})
	if err != nil {
		t.Fatalf("CreateChatStream: %v", err)
	}

	for ev := range ch {
		switch ev.Type {
		case StreamEventDone:
			if ev.Usage != nil {
				t.Errorf("Usage = %+v, want nil", ev.Usage)
			}
			return
		case StreamEventError:
			t.Fatalf("unexpected error: %v", ev.Error)
		}
	}
	t.Fatal("no Done event")
}

// --- pure functions --------------------------------------------------------

func TestResponsesDeriveFinishReason(t *testing.T) {
	mk := func(status responses.ResponseStatus, incompleteReason string, lastType string) *responses.Response {
		out := []responses.ResponseOutputItemUnion{}
		if lastType != "" {
			out = append(out, responses.ResponseOutputItemUnion{Type: lastType})
		}
		return &responses.Response{
			Status:            status,
			IncompleteDetails: responses.ResponseIncompleteDetails{Reason: incompleteReason},
			Output:            out,
		}
	}

	cases := []struct {
		name string
		resp *responses.Response
		want string
	}{
		{"completed empty", mk(responses.ResponseStatusCompleted, "", ""), "stop"},
		{"completed text", mk(responses.ResponseStatusCompleted, "", "message"), "stop"},
		{"completed tool call", mk(responses.ResponseStatusCompleted, "", "function_call"), "tool_calls"},
		{"incomplete max tokens", mk(responses.ResponseStatusIncomplete, "max_output_tokens", "message"), "length"},
		{"incomplete content filter", mk(responses.ResponseStatusIncomplete, "content_filter", ""), "stop"},
		{"failed", mk(responses.ResponseStatusFailed, "", ""), "error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := deriveResponsesFinishReason(c.resp); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestResponsesUsageMapping(t *testing.T) {
	u := responses.ResponseUsage{
		InputTokens:  100,
		OutputTokens: 50,
		InputTokensDetails: responses.ResponseUsageInputTokensDetails{
			CachedTokens:     30,
			CacheWriteTokens: 20,
		},
		OutputTokensDetails: responses.ResponseUsageOutputTokensDetails{
			ReasoningTokens: 10,
		},
		TotalTokens: 150,
	}

	got := usageFromResponsesUsage(u)
	if got.InputTokens != 100 || got.LastInputTokens != 100 {
		t.Errorf("InputTokens = %d/%d", got.InputTokens, got.LastInputTokens)
	}
	if got.OutputTokens != 50 {
		t.Errorf("OutputTokens = %d, want 50", got.OutputTokens)
	}
	if got.CacheReadInputTokens != 30 {
		t.Errorf("CacheReadInputTokens = %d, want 30", got.CacheReadInputTokens)
	}
	if got.CacheCreationInputTokens != 20 {
		t.Errorf("CacheCreationInputTokens = %d, want 20", got.CacheCreationInputTokens)
	}
}

func TestResponsesConvertToolMessages_SkipMissingCallID(t *testing.T) {
	p := NewOpenAIResponsesProvider("k", "", "gpt-5.6")
	_, items := p.convertMessages([]Message{
		{Role: "tool", Content: "orphan result without call id"},
		{Role: "user", Content: "hi"},
	})
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1 (orphan tool message must be skipped)", len(items))
	}
}

func TestResponsesConvertMessages_ThinkingDropped(t *testing.T) {
	p := NewOpenAIResponsesProvider("k", "", "gpt-5.6")
	_, items := p.convertMessages([]Message{
		{Role: "assistant", Content: "answer", ThinkingBlocks: []ThinkingBlock{
			{Type: "thinking", Thinking: "secret chain of thought"},
		}},
	})
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	if items[0].OfMessage == nil {
		t.Fatalf("expected a message item, got %+v", items[0])
	}
	if items[0].OfMessage.Content.OfString.Value != "answer" {
		t.Errorf("content = %q, want answer (thinking must not leak into content)", items[0].OfMessage.Content.OfString.Value)
	}
}

func TestResponsesConvertMessages_SteerBecomesUser(t *testing.T) {
	p := NewOpenAIResponsesProvider("k", "", "gpt-5.6")
	_, items := p.convertMessages([]Message{
		{Role: RoleSteer, Content: "Continue"},
	})
	if len(items) != 1 || items[0].OfMessage == nil {
		t.Fatalf("items = %+v", items)
	}
	if items[0].OfMessage.Role != responses.EasyInputMessageRoleUser {
		t.Errorf("steer role = %q, want user", items[0].OfMessage.Role)
	}
}

func TestNewProvider_OpenAIResponses(t *testing.T) {
	p, err := NewProvider(ProviderTypeOpenAIResponses, "sk-test", "https://api.openai.com/v1", "gpt-5.6")
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	rp, ok := p.(*OpenAIResponsesProvider)
	if !ok {
		t.Fatalf("expected *OpenAIResponsesProvider, got %T", p)
	}
	if rp.Name() != ProviderTypeOpenAIResponses {
		t.Errorf("Name = %q", rp.Name())
	}
	if rp.Model() != "gpt-5.6" {
		t.Errorf("Model = %q", rp.Model())
	}
	if rp.baseURL != "https://api.openai.com/v1" || rp.apiKey != "sk-test" {
		t.Errorf("provider = %+v", rp)
	}
}

func TestOpenAIResponsesCreateChatStream_Incomplete(t *testing.T) {
	// max_output_tokens truncation: response.incomplete must terminate the
	// stream with finish_reason "length" (not the clean-stop fallback).
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, `{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","status":"in_progress","role":"assistant","content":[]}}`)
		writeSSE(w, `{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"partial"}`)
		writeSSE(w, `{"type":"response.incomplete","response":{"id":"resp_1","object":"response","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"model":"gpt-5.6","output":[{"id":"msg_1","type":"message","status":"incomplete","role":"assistant","content":[{"type":"output_text","text":"partial","annotations":[]}]}],"usage":{"input_tokens":10,"output_tokens":100,"output_tokens_details":{"reasoning_tokens":60},"total_tokens":110}}}`)
	}
	srv, _ := newResponsesMockServer(t, handler)
	p := newTestResponsesProvider(t, srv)

	ch, err := p.CreateChatStream(context.Background(), []Message{
		{Role: "user", Content: "long output please"},
	}, nil, ChatOptions{MaxTokens: 64})
	if err != nil {
		t.Fatalf("CreateChatStream: %v", err)
	}

	var deltas []string
	var done *StreamEvent
	for ev := range ch {
		switch ev.Type {
		case StreamEventTextDelta:
			deltas = append(deltas, ev.TextDelta)
		case StreamEventDone:
			done = &ev
		case StreamEventError:
			t.Fatalf("unexpected stream error: %v", ev.Error)
		}
	}

	if len(deltas) != 1 || deltas[0] != "partial" {
		t.Errorf("text deltas = %v", deltas)
	}
	if done == nil {
		t.Fatal("no Done event")
	}
	if done.FinishReason != "length" {
		t.Errorf("Done.FinishReason = %q, want length", done.FinishReason)
	}
	if done.Usage == nil || done.Usage.OutputTokens != 100 {
		t.Errorf("Done.Usage = %+v, want output=100", done.Usage)
	}
}

func TestResponsesStrictCompatibleSchema(t *testing.T) {
	cases := []struct {
		name   string
		schema string
		want   bool
	}{
		{
			name:   "all properties required",
			schema: `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"integer"}},"required":["a","b"]}`,
			want:   true,
		},
		{
			name:   "optional property not in required",
			schema: `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}},"required":["a"]}`,
			want:   false,
		},
		{
			name:   "required missing entirely",
			schema: `{"type":"object","properties":{"a":{"type":"string"}}}`,
			want:   false,
		},
		{
			name:   "nested object with optional property",
			schema: `{"type":"object","properties":{"q":{"type":"array","items":{"type":"object","properties":{"x":{"type":"string"},"opt":{"type":"string"}},"required":["x"]}}},"required":["q"]}`,
			want:   false,
		},
		{
			name:   "nested object fully required",
			schema: `{"type":"object","properties":{"q":{"type":"array","items":{"type":"object","properties":{"x":{"type":"string"}},"required":["x"]}}},"required":["q"]}`,
			want:   true,
		},
		{
			name:   "no properties",
			schema: `{"type":"object","required":[]}`,
			want:   true,
		},
		{
			name:   "default keyword forces non-strict",
			schema: `{"type":"object","properties":{"a":{"type":"string","default":"x"}},"required":["a"]}`,
			want:   false,
		},
		{
			name:   "default on nested items forces non-strict",
			schema: `{"type":"object","properties":{"q":{"type":"array","items":{"type":"object","properties":{"x":{"type":"string","default":"y"}},"required":["x"]}}},"required":["q"]}`,
			want:   false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var m map[string]any
			if err := json.Unmarshal([]byte(c.schema), &m); err != nil {
				t.Fatal(err)
			}
			if got := strictCompatibleSchema(m); got != c.want {
				t.Errorf("strictCompatibleSchema = %v, want %v", got, c.want)
			}
		})
	}
}

func TestResponsesConvertTools_StrictPerSchema(t *testing.T) {
	// convertTools must enable strict only for schemas that satisfy the
	// strict-mode constraints — unconditional strict would make the API
	// reject tools with optional parameters (400).
	p := NewOpenAIResponsesProvider("k", "", "gpt-5.6")
	tools := []Tool{
		// askuser-style: nested `options` property not in required.
		NewTool("ask", "Ask the user", map[string]ToolParameterProperty{
			"questions": {Type: "array", Description: "q", Items: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"question": map[string]any{"type": "string"},
					"options":  map[string]any{"type": "array"},
				},
				"required": []string{"question"},
			}},
		}, []string{"questions"}),
		// all top-level properties required.
		NewTool("run", "Run", map[string]ToolParameterProperty{
			"command": {Type: "string", Description: "c"},
		}, []string{"command"}),
	}
	out := p.convertTools(tools)
	if len(out) != 2 {
		t.Fatalf("tools = %d, want 2", len(out))
	}
	ask, run := out[0].OfFunction, out[1].OfFunction
	if ask == nil || run == nil {
		t.Fatalf("expected function tools, got %+v", out)
	}
	if ask.Strict.Valid() {
		t.Errorf("ask tool Strict = %v, want unset (has optional params)", ask.Strict.Or(false))
	}
	if !run.Strict.Valid() || !run.Strict.Or(false) {
		t.Errorf("run tool Strict = %v, want true (all params required)", run.Strict)
	}
}
