// Package mockllm implements an in-process mock LLM server that speaks the
// OpenAI chat-completions (POST /v1/chat/completions), Anthropic
// (POST /v1/messages) and OpenAI Responses (POST /responses) wire protocols
// over httptest.
//
// Scenarios script the server with protocol-agnostic building blocks:
//
//	mock := mockllm.NewServer()
//	mock.Script(
//		mockllm.Step{Reply: mockllm.Stream(
//			mockllm.ToolCallStart("call_1", "Bash", `{"command":"ls"}`),
//			mockllm.Finish("tool_calls"), mockllm.Usage(120, 30), mockllm.Done(),
//		)},
//	)
//
// The same script renders to either wire format; `Require` assertions and the
// recorded requests are normalized into protocol-independent views, so
// scenario layers never touch protocol details (see contract_test.go for the
// wire-format lock-in tests using real SDK clients).
package mockllm

import (
	"context"
	"net/http"
	"time"
)

// Protocol selects which wire protocol the server speaks.
type Protocol int

const (
	// ProtocolOpenAI serves POST /v1/chat/completions (go-openai client).
	ProtocolOpenAI Protocol = iota
	// ProtocolAnthropic serves POST /v1/messages (anthropic-sdk-go client).
	ProtocolAnthropic
	// ProtocolOpenAIResponses serves POST /responses (official openai-go
	// Responses API client).
	ProtocolOpenAIResponses
)

func (p Protocol) String() string {
	switch p {
	case ProtocolAnthropic:
		return "anthropic"
	case ProtocolOpenAIResponses:
		return "openai-res"
	default:
		return "openai"
	}
}

// chunkKind enumerates the stream building blocks.
type chunkKind int

const (
	chunkThinking chunkKind = iota
	chunkSignature
	chunkText
	chunkToolStart
	chunkToolArgs
	chunkFinish
	chunkUsage
	chunkDone
	chunkPause
	chunkMalformed
	chunkPing
)

// Chunk is a single step in a scripted stream reply. Constructed via the
// package-level functions (Thinking, Text, ToolCallStart, ...); the wire
// rendering is protocol-specific (see render.go).
type Chunk struct {
	kind              chunkKind
	text              string
	id                string
	name              string
	args              string
	finish            string
	promptTokens      int
	completionTokens  int
	cacheReadTokens   int
	cacheCreationToks int
	pause             time.Duration
}

// Thinking emits a reasoning/thinking delta. On the Anthropic protocol the
// block is rendered with a stable signature so multi-turn history round-trips
// faithfully (the agent loop echoes thinking blocks back with signatures).
func Thinking(text string) Chunk {
	return Chunk{kind: chunkThinking, text: text}
}

// Signature emits an Anthropic signature_delta (OpenAI protocol: no-op).
// Normally unnecessary — Thinking() already carries a signature on Anthropic —
// but available for finer control of multi-chunk thinking streams.
func Signature(sig string) Chunk {
	return Chunk{kind: chunkSignature, text: sig}
}

// Text emits a text delta.
func Text(text string) Chunk {
	return Chunk{kind: chunkText, text: text}
}

// ToolCallStart opens a tool call with its id, function name and initial
// arguments JSON. The arguments may be split across multiple ToolArgsDelta
// chunks to model incremental streaming.
func ToolCallStart(id, name, args string) Chunk {
	return Chunk{kind: chunkToolStart, id: id, name: name, args: args}
}

// ToolArgsDelta appends a fragment of tool arguments JSON.
func ToolArgsDelta(args string) Chunk {
	return Chunk{kind: chunkToolArgs, args: args}
}

// Finish terminates the assistant message with a finish reason. Protocol
// agnostic values: "stop", "tool_calls", "length"/"max_tokens". "stop"
// maps to "end_turn" and "tool_calls" to "tool_use" on the Anthropic wire.
func Finish(reason string) Chunk {
	return Chunk{kind: chunkFinish, finish: reason}
}

// Usage reports prompt/completion token counts. OpenAI renders it as a chunk
// with empty choices; Anthropic folds it into the message_delta usage.
func Usage(promptTokens, completionTokens int) Chunk {
	return Chunk{kind: chunkUsage, promptTokens: promptTokens, completionTokens: completionTokens}
}

// UsageWithCache is Usage plus cache accounting: cacheRead = tokens served
// from a cache hit, cacheCreation = tokens written into the cache.
// Protocol mapping:
//   - Anthropic: cache_read_input_tokens / cache_creation_input_tokens on
//     message_delta usage (mirrors the real API).
//   - OpenAI: only cache read is expressible — prompt_tokens_details.
//     cached_tokens (parsed by go-openai into Usage.CacheReadInputTokens);
//     cache creation has no standard OpenAI field and stays 0.
func UsageWithCache(promptTokens, completionTokens, cacheRead, cacheCreation int) Chunk {
	return Chunk{
		kind:              chunkUsage,
		promptTokens:      promptTokens,
		completionTokens:  completionTokens,
		cacheReadTokens:   cacheRead,
		cacheCreationToks: cacheCreation,
	}
}

// Done terminates the SSE stream. OpenAI: "data: [DONE]". Anthropic:
// message_stop event. Omitted, the server closes the stream at the end of
// the chunk list (clients treat EOF as stream end); always prefer an explicit
// Done for determinism.
func Done() Chunk {
	return Chunk{kind: chunkDone}
}

// Pause holds the stream for d, letting scenarios park the client in the
// streaming state (e.g. to inject keyboard input at a known point). The wait
// is cancelled when the request context ends.
func Pause(d time.Duration) Chunk {
	return Chunk{kind: chunkPause, pause: d}
}

// Malformed writes a broken SSE line, exercising client error handling.
func Malformed() Chunk {
	return Chunk{kind: chunkMalformed}
}

// Ping emits an Anthropic heartbeat event ({"type":"ping"}), which real APIs
// insert between content blocks; the SDK consumes it silently. OpenAI
// protocol: no-op. Insert it anywhere in the stream to model a real flow.
func Ping() Chunk {
	return Chunk{kind: chunkPing}
}

// ReplyFunc renders one HTTP response for a script step.
type ReplyFunc func(ctx context.Context, w http.ResponseWriter, p Protocol)

// Step is one scripted interaction: an optional precondition checked against
// the incoming request, and the reply to send. Steps are consumed in request
// order; running out of steps fails the scenario (see Server.Error).
type Step struct {
	Require RequireFunc
	Reply   ReplyFunc
}

// Stream renders a streaming SSE reply from protocol-agnostic chunks.
func Stream(chunks ...Chunk) ReplyFunc {
	return func(ctx context.Context, w http.ResponseWriter, p Protocol) {
		writeSSEHeaders(w)
		switch p {
		case ProtocolAnthropic:
			renderAnthropicStream(ctx, w, chunks)
		case ProtocolOpenAIResponses:
			renderOpenAIResponsesStream(ctx, w, chunks)
		default:
			renderOpenAIStream(ctx, w, chunks)
		}
	}
}

// JSON renders a non-streaming JSON response (used by title generation,
// /compact and other CreateChat calls).
func JSON(content string, reasoning string, toolCalls ...Chunk) ReplyFunc {
	return func(_ context.Context, w http.ResponseWriter, p Protocol) {
		w.Header().Set("Content-Type", "application/json")
		switch p {
		case ProtocolAnthropic:
			renderAnthropicJSON(w, content, reasoning)
		case ProtocolOpenAIResponses:
			renderOpenAIResponsesJSON(w, content, reasoning, toolCalls)
		default:
			renderOpenAIJSON(w, content, reasoning, toolCalls)
		}
	}
}

// StatusError returns a plain HTTP error before the stream is established
// (retry tests use 429; the retry provider only retries errors that arrive
// this way — never mid-SSE).
func StatusError(code int, msg string) ReplyFunc {
	return func(_ context.Context, w http.ResponseWriter, _ Protocol) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		writeJSON(w, map[string]any{
			"error": map[string]any{
				"message": msg,
				"type":    "mock_error",
				"code":    code,
			},
		})
	}
}

// MalformedSSE sends an SSE response whose body is not parseable.
func MalformedSSE() ReplyFunc {
	return func(ctx context.Context, w http.ResponseWriter, p Protocol) {
		writeSSEHeaders(w)
		switch p {
		case ProtocolAnthropic:
			renderAnthropicStream(ctx, w, []Chunk{{kind: chunkMalformed}})
		case ProtocolOpenAIResponses:
			renderOpenAIResponsesStream(ctx, w, []Chunk{{kind: chunkMalformed}})
		default:
			renderOpenAIStream(ctx, w, []Chunk{{kind: chunkMalformed}})
		}
	}
}

// Hold keeps the response open until the request context is cancelled
// (timeout / client abort), exercising cancellation paths without leaking
// handler goroutines.
func Hold() ReplyFunc {
	return func(ctx context.Context, w http.ResponseWriter, _ Protocol) {
		writeSSEHeaders(w)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-ctx.Done()
	}
}

func writeSSEHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
}
