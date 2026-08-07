package mockllm_test

// Contract tests: the REAL SDK clients (llm.OpenAIProvider /
// llm.AnthropicProvider) parse the mock's wire output. If the mock's line
// format drifts from what the clients expect, these tests fail at the stub
// layer before any scenario does. They run with the regular unit tests
// (no integration build tag) on every `go test ./...`.

import (
	"context"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/monsterxx03/tachi/llm"
	"github.com/stretchr/testify/require"
)

// consumeStream drains the event channel into a slice.
func consumeStream(t *testing.T, ch <-chan llm.StreamEvent) []llm.StreamEvent {
	t.Helper()
	var out []llm.StreamEvent
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-timeout:
			t.Fatal("timed out waiting for stream events")
		}
	}
}

// TestContractOpenAIStream locks the OpenAI /v1/chat/completions SSE line
// format against the real go-openai client (via llm.OpenAIProvider).
func TestContractOpenAIStream(t *testing.T) {
	mock := mockllm.NewServer()
	defer mock.Close()
	mock.Script(mockllm.Step{Reply: mockllm.Stream(
		mockllm.Thinking("让我想想"),
		mockllm.Text("你好"),
		mockllm.ToolCallStart("call_1", "Bash", `{"command":"ls"}`),
		mockllm.ToolArgsDelta(`" -la"`),
		mockllm.Finish("tool_calls"),
		mockllm.Usage(120, 30),
		mockllm.Done(),
	)})

	provider := llm.NewOpenAIProvider("test-key", mock.BaseURL(), "mock-model")
	ch, err := provider.CreateChatStream(context.Background(),
		[]llm.Message{{Role: "user", Content: "hi"}}, nil,
		llm.ChatOptions{MaxTokens: 4096})
	require.NoError(t, err)

	events := consumeStream(t, ch)
	// ToolCallStart with initial args yields TWO events on the OpenAI wire:
	// the client emits ToolUseStart (id present) and InputJSONDelta
	// (arguments present) from the same chunk, then the explicit
	// ToolArgsDelta chunk emits a second InputJSONDelta.
	require.Len(t, events, 7)
	require.Equal(t, llm.StreamEventThinkingDelta, events[0].Type)
	require.Equal(t, "让我想想", events[0].ThinkingDelta)

	require.Equal(t, llm.StreamEventTextDelta, events[1].Type)
	require.Equal(t, "你好", events[1].TextDelta)

	require.Equal(t, llm.StreamEventToolUseStart, events[2].Type)
	require.NotNil(t, events[2].ToolCall)
	require.Equal(t, "call_1", events[2].ToolCall.ID)
	require.Equal(t, "Bash", events[2].ToolCall.Function.Name)

	require.Equal(t, llm.StreamEventInputJSONDelta, events[3].Type)
	require.Equal(t, `{"command":"ls"}`, events[3].InputDelta)

	require.Equal(t, llm.StreamEventInputJSONDelta, events[4].Type)
	require.Equal(t, `" -la"`, events[4].InputDelta)

	require.Equal(t, llm.StreamEventMessageDelta, events[5].Type)
	require.Equal(t, "tool_calls", events[5].FinishReason)

	require.Equal(t, llm.StreamEventDone, events[6].Type)
	require.Equal(t, "tool_calls", events[6].FinishReason)
	require.NotNil(t, events[6].Usage)
	require.Equal(t, int64(120), events[6].Usage.InputTokens)
	require.Equal(t, int64(30), events[6].Usage.OutputTokens)
}

// TestContractAnthropicStream locks the Anthropic /v1/messages SSE line
// format against the real anthropic-sdk-go client (via llm.AnthropicProvider):
// event:/data: pairs, thinking block with signature, tool_use block, and the
// stop_reason mapping (stop → end_turn, tool_calls → tool_use).
func TestContractAnthropicStream(t *testing.T) {
	mock := mockllm.NewServer(mockllm.WithProtocol(mockllm.ProtocolAnthropic))
	defer mock.Close()
	mock.Script(mockllm.Step{Reply: mockllm.Stream(
		mockllm.Thinking("思考中"),
		mockllm.Text("你好"),
		mockllm.ToolCallStart("call_1", "Bash", `{"command":"ls"}`),
		mockllm.Usage(120, 30),
		mockllm.Finish("tool_calls"),
		mockllm.Done(),
	)})

	provider := llm.NewAnthropicProvider("test-key", mock.BaseURL(), "mock-model")
	ch, err := provider.CreateChatStream(context.Background(),
		[]llm.Message{{Role: "user", Content: "hi"}}, nil,
		llm.ChatOptions{MaxTokens: 4096})
	require.NoError(t, err)

	events := consumeStream(t, ch)
	require.Len(t, events, 7) // thinking + signature + text + toolUse + inputJSON + msgDelta + done
	require.Equal(t, llm.StreamEventThinkingDelta, events[0].Type)
	require.Equal(t, "思考中", events[0].ThinkingDelta)
	require.Equal(t, llm.StreamEventSignatureDelta, events[1].Type)
	require.NotEmpty(t, events[1].SignatureDelta)

	require.Equal(t, llm.StreamEventTextDelta, events[2].Type)
	require.Equal(t, "你好", events[2].TextDelta)

	require.Equal(t, llm.StreamEventToolUseStart, events[3].Type)
	require.NotNil(t, events[3].ToolCall)
	require.Equal(t, "call_1", events[3].ToolCall.ID)
	require.Equal(t, "Bash", events[3].ToolCall.Function.Name)

	require.Equal(t, llm.StreamEventInputJSONDelta, events[4].Type)
	require.Equal(t, `{"command":"ls"}`, events[4].InputDelta)

	require.Equal(t, llm.StreamEventMessageDelta, events[5].Type)
	require.Equal(t, "tool_use", events[5].FinishReason)
	require.NotNil(t, events[5].Usage)
	require.Equal(t, int64(120), events[5].Usage.InputTokens)

	require.Equal(t, llm.StreamEventDone, events[6].Type)
	require.Equal(t, "tool_use", events[6].FinishReason)
	require.Equal(t, int64(120), events[6].Usage.InputTokens)
}

// TestContractAnthropicPingAndCache locks two real-API behaviors mockllm now
// mirrors: heartbeat ping events interleaved between content blocks (SDK
// consumes them silently — the event sequence is unaffected), and the cache
// accounting fields on message_start/message_delta usage.
func TestContractAnthropicPingAndCache(t *testing.T) {
	mock := mockllm.NewServer(mockllm.WithProtocol(mockllm.ProtocolAnthropic))
	defer mock.Close()
	mock.Script(mockllm.Step{Reply: mockllm.Stream(
		mockllm.Thinking("思考"),
		mockllm.Ping(), // real APIs insert pings between blocks
		mockllm.Text("你好"),
		mockllm.Ping(),
		mockllm.Usage(120, 30),
		mockllm.Finish("stop"),
		mockllm.Done(),
	)})

	provider := llm.NewAnthropicProvider("test-key", mock.BaseURL(), "mock-model")
	ch, err := provider.CreateChatStream(context.Background(),
		[]llm.Message{{Role: "user", Content: "hi"}}, nil,
		llm.ChatOptions{MaxTokens: 4096})
	require.NoError(t, err)

	events := consumeStream(t, ch)
	// pings vanish: thinking + signature + text + msgDelta + done
	require.Len(t, events, 5)
	require.Equal(t, llm.StreamEventThinkingDelta, events[0].Type)
	require.Equal(t, "思考", events[0].ThinkingDelta)
	require.Equal(t, llm.StreamEventTextDelta, events[2].Type)
	require.Equal(t, "你好", events[2].TextDelta)

	require.Equal(t, llm.StreamEventMessageDelta, events[3].Type)
	require.NotNil(t, events[3].Usage)
	require.Equal(t, int64(120), events[3].Usage.InputTokens)
	require.Equal(t, int64(30), events[3].Usage.OutputTokens)
	// cache fields present on the wire, parsed into the client usage
	require.Equal(t, int64(0), events[3].Usage.CacheCreationInputTokens)
	require.Equal(t, int64(0), events[3].Usage.CacheReadInputTokens)

	require.Equal(t, llm.StreamEventDone, events[4].Type)
	require.Equal(t, "end_turn", events[4].FinishReason)
}

// TestContractUsageWithCache locks the cache accounting in UsageWithCache:
// cache-read tokens surface via prompt_tokens_details.cached_tokens on the
// OpenAI wire and cache_read_input_tokens on the Anthropic wire; cache
// creation only exists on Anthropic (cache_creation_input_tokens).
func TestContractUsageWithCache(t *testing.T) {
	t.Run("openai cache read", func(t *testing.T) {
		mock := mockllm.NewServer()
		defer mock.Close()
		mock.Script(mockllm.Step{Reply: mockllm.Stream(
			mockllm.Text("ok"),
			mockllm.UsageWithCache(100, 20, 45, 10),
			mockllm.Finish("stop"),
			mockllm.Done(),
		)})
		provider := llm.NewOpenAIProvider("test-key", mock.BaseURL(), "mock-model")
		ch, err := provider.CreateChatStream(context.Background(),
			[]llm.Message{{Role: "user", Content: "hi"}}, nil, llm.ChatOptions{MaxTokens: 4096})
		require.NoError(t, err)
		events := consumeStream(t, ch)
		require.Len(t, events, 3)
		require.Equal(t, llm.StreamEventDone, events[2].Type)
		require.NotNil(t, events[2].Usage)
		require.Equal(t, int64(100), events[2].Usage.InputTokens)
		require.Equal(t, int64(45), events[2].Usage.CacheReadInputTokens)
		// cache creation is not expressible on the OpenAI wire
		require.Equal(t, int64(0), events[2].Usage.CacheCreationInputTokens)
	})

	t.Run("anthropic cache read and creation", func(t *testing.T) {
		mock := mockllm.NewServer(mockllm.WithProtocol(mockllm.ProtocolAnthropic))
		defer mock.Close()
		mock.Script(mockllm.Step{Reply: mockllm.Stream(
			mockllm.Text("ok"),
			mockllm.UsageWithCache(100, 20, 45, 10),
			mockllm.Finish("stop"),
			mockllm.Done(),
		)})
		provider := llm.NewAnthropicProvider("test-key", mock.BaseURL(), "mock-model")
		ch, err := provider.CreateChatStream(context.Background(),
			[]llm.Message{{Role: "user", Content: "hi"}}, nil, llm.ChatOptions{MaxTokens: 4096})
		require.NoError(t, err)
		events := consumeStream(t, ch)
		require.Len(t, events, 3)
		require.Equal(t, llm.StreamEventMessageDelta, events[1].Type)
		require.NotNil(t, events[1].Usage)
		require.Equal(t, int64(100), events[1].Usage.InputTokens)
		require.Equal(t, int64(45), events[1].Usage.CacheReadInputTokens)
		require.Equal(t, int64(10), events[1].Usage.CacheCreationInputTokens)
	})
}

// TestContractOpenAIStopReason locks the plain-stop finish: OpenAI "stop",
// Anthropic "end_turn" — both must surface as Done without a tool_calls
// finish so the agent loop ends the turn.
func TestContractOpenAIStopReason(t *testing.T) {
	mock := mockllm.NewServer()
	defer mock.Close()
	mock.Script(mockllm.Step{Reply: mockllm.Stream(
		mockllm.Text("完成了"),
		mockllm.Usage(50, 10),
		mockllm.Finish("stop"),
		mockllm.Done(),
	)})

	provider := llm.NewOpenAIProvider("test-key", mock.BaseURL(), "mock-model")
	ch, err := provider.CreateChatStream(context.Background(),
		[]llm.Message{{Role: "user", Content: "hi"}}, nil, llm.ChatOptions{MaxTokens: 4096})
	require.NoError(t, err)
	events := consumeStream(t, ch)
	require.Len(t, events, 3)
	require.Equal(t, llm.StreamEventDone, events[2].Type)
	require.Equal(t, "stop", events[2].FinishReason)
}

func TestContractAnthropicStopReason(t *testing.T) {
	mock := mockllm.NewServer(mockllm.WithProtocol(mockllm.ProtocolAnthropic))
	defer mock.Close()
	mock.Script(mockllm.Step{Reply: mockllm.Stream(
		mockllm.Text("完成了"),
		mockllm.Usage(50, 10),
		mockllm.Finish("stop"),
		mockllm.Done(),
	)})

	provider := llm.NewAnthropicProvider("test-key", mock.BaseURL(), "mock-model")
	ch, err := provider.CreateChatStream(context.Background(),
		[]llm.Message{{Role: "user", Content: "hi"}}, nil, llm.ChatOptions{MaxTokens: 4096})
	require.NoError(t, err)
	events := consumeStream(t, ch)
	require.Len(t, events, 3)
	require.Equal(t, llm.StreamEventDone, events[2].Type)
	require.Equal(t, "end_turn", events[2].FinishReason)
}

// TestContractOpenAIJSON locks the non-streaming chat completion response
// (CreateChat — title generation /compact paths).
func TestContractOpenAIJSON(t *testing.T) {
	mock := mockllm.NewServer()
	defer mock.Close()
	mock.Script(mockllm.Step{Reply: mockllm.JSON("非流式回答", "推理内容")})

	provider := llm.NewOpenAIProvider("test-key", mock.BaseURL(), "mock-model")
	resp, err := provider.CreateChat(context.Background(),
		[]llm.Message{{Role: "user", Content: "hi"}}, nil, llm.ChatOptions{MaxTokens: 4096})
	require.NoError(t, err)
	require.Equal(t, "非流式回答", resp.Content)
	require.Equal(t, "推理内容", resp.Reasoning)
	require.Equal(t, "stop", resp.FinishReason)
}

func TestContractAnthropicJSON(t *testing.T) {
	mock := mockllm.NewServer(mockllm.WithProtocol(mockllm.ProtocolAnthropic))
	defer mock.Close()
	mock.Script(mockllm.Step{Reply: mockllm.JSON("非流式回答", "推理内容")})

	provider := llm.NewAnthropicProvider("test-key", mock.BaseURL(), "mock-model")
	resp, err := provider.CreateChat(context.Background(),
		[]llm.Message{{Role: "user", Content: "hi"}}, nil, llm.ChatOptions{MaxTokens: 4096})
	require.NoError(t, err)
	require.Equal(t, "非流式回答", resp.Content)
	require.Len(t, resp.ThinkingBlocks, 1)
	require.Equal(t, "推理内容", resp.ThinkingBlocks[0].Thinking)
	require.NotEmpty(t, resp.ThinkingBlocks[0].Signature)
}

// TestContractStatusError locks the retry path: a 429 returned before the
// stream is established must surface as an error from CreateChatStream.
func TestContractStatusError(t *testing.T) {
	mock := mockllm.NewServer()
	defer mock.Close()
	mock.Script(mockllm.Step{Reply: mockllm.StatusError(429, "rate limited")})

	provider := llm.NewOpenAIProvider("test-key", mock.BaseURL(), "mock-model")
	_, err := provider.CreateChatStream(context.Background(),
		[]llm.Message{{Role: "user", Content: "hi"}}, nil, llm.ChatOptions{MaxTokens: 4096})
	require.Error(t, err)
}
