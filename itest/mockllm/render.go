package mockllm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// unmarshalJSON is a tiny indirection keeping require.go free of direct
// encoding/json imports is unnecessary — it just wraps json.Unmarshal for the
// few body-probing helpers.
func unmarshalJSON(data []byte, v any) error { return json.Unmarshal(data, v) }

func writeJSON(w http.ResponseWriter, v any) {
	_ = json.NewEncoder(w).Encode(v)
}

// mapFinishReason translates protocol-agnostic finish reasons onto each
// wire's vocabulary. The agent loop treats "tool_calls"/"tool_use" as
// tool-call terminations and everything else as a plain stop (agent_loop.go),
// so "stop" must become Anthropic's "end_turn".
func mapFinishReason(p Protocol, reason string) string {
	switch {
	case p == ProtocolAnthropic && reason == "stop":
		return "end_turn"
	case p == ProtocolAnthropic && reason == "tool_calls":
		return "tool_use"
	default:
		return reason
	}
}

// ── OpenAI wire format ─────────────────────────────────────────────────────
//
// go-openai's stream reader consumes one `data: <json>` per line (no blank
// lines required); the stream ends with `data: [DONE]`. Usage must arrive in
// a chunk with empty choices (the client captures usage before skipping
// choice-less chunks).

func renderOpenAIStream(ctx context.Context, w http.ResponseWriter, chunks []Chunk) {
	flusher, _ := w.(http.Flusher)
	data := func(obj any) {
		b, err := json.Marshal(obj)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	choice := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{
			"choices": []any{map[string]any{
				"index":         0,
				"delta":         delta,
				"finish_reason": finish,
			}},
		}
	}
	for _, c := range chunks {
		switch c.kind {
		case chunkThinking:
			data(choice(map[string]any{"reasoning_content": c.text}, nil))
		case chunkText:
			data(choice(map[string]any{"content": c.text}, nil))
		case chunkToolStart:
			data(choice(map[string]any{
				"tool_calls": []any{map[string]any{
					"index":    0,
					"id":       c.id,
					"type":     "function",
					"function": map[string]any{"name": c.name, "arguments": c.args},
				}},
			}, nil))
		case chunkToolArgs:
			data(choice(map[string]any{
				"tool_calls": []any{map[string]any{
					"index":    0,
					"function": map[string]any{"arguments": c.args},
				}},
			}, nil))
		case chunkFinish:
			data(choice(map[string]any{}, mapFinishReason(ProtocolOpenAI, c.finish)))
		case chunkUsage:
			data(map[string]any{
				"choices": []any{},
				"usage": map[string]any{
					"prompt_tokens":     c.promptTokens,
					"completion_tokens": c.completionTokens,
					"total_tokens":      c.promptTokens + c.completionTokens,
				},
			})
		case chunkDone:
			fmt.Fprint(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		case chunkPause:
			select {
			case <-ctx.Done():
				return
			case <-time.After(c.pause):
			}
		case chunkMalformed:
			fmt.Fprint(w, "data: {broken json\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		case chunkSignature:
			// OpenAI has no signature concept — ignored.
		}
	}
}

// renderOpenAIJSON renders the non-streaming chat completion response
// (title generation, /compact, deepresearch — CreateChat calls).
func renderOpenAIJSON(w http.ResponseWriter, content, reasoning string, toolCallChunks []Chunk) {
	msg := map[string]any{
		"role":    "assistant",
		"content": content,
	}
	finish := "stop"
	var toolCalls []any
	for _, c := range toolCallChunks {
		switch c.kind {
		case chunkToolStart:
			toolCalls = append(toolCalls, map[string]any{
				"id":       c.id,
				"type":     "function",
				"function": map[string]any{"name": c.name, "arguments": c.args},
			})
			finish = "tool_calls"
		case chunkToolArgs:
			if len(toolCalls) > 0 {
				last := toolCalls[len(toolCalls)-1].(map[string]any)
				fn := last["function"].(map[string]any)
				fn["arguments"] = fn["arguments"].(string) + c.args
			}
		}
	}
	if reasoning != "" {
		msg["reasoning_content"] = reasoning
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	writeJSON(w, map[string]any{
		"id":      "chatcmpl-mock",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   "mock-model",
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
		"usage": map[string]any{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		},
	})
}

// ── Anthropic wire format ──────────────────────────────────────────────────
//
// anthropic-sdk-go's SSE decoder dispatches on blank-line-separated
// `event:`/`data:` pairs; the JSON payload's `type` field must match the
// event name. Thinking blocks carry a signature; usage is folded into
// message_delta.

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func renderAnthropicStream(ctx context.Context, w http.ResponseWriter, chunks []Chunk) {
	flusher, _ := w.(http.Flusher)
	event := func(name string, payload map[string]any) {
		payload["type"] = name
		b, err := json.Marshal(payload)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: %s\n", name)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}

	// The SDK accumulates input_tokens from message_start; start with a
	// non-zero-ish baseline so usage-driven assertions see real numbers.
	event("message_start", map[string]any{
		"message": map[string]any{
			"id":            "msg_mock",
			"type":          "message",
			"role":          "assistant",
			"model":         "mock-model",
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})

	var pendingUsage *anthropicUsage
	finished := false // a message_delta (Finish) was already emitted
	blockIdx := 0
	blockStart := func(block map[string]any) {
		event("content_block_start", map[string]any{"index": blockIdx, "content_block": block})
	}
	blockDelta := func(delta map[string]any) {
		event("content_block_delta", map[string]any{"index": blockIdx, "delta": delta})
	}
	blockStop := func() {
		event("content_block_stop", map[string]any{"index": blockIdx})
		blockIdx++
	}

	for _, c := range chunks {
		switch c.kind {
		case chunkThinking:
			blockStart(map[string]any{"type": "thinking", "thinking": c.text, "signature": "mock-signature"})
			blockDelta(map[string]any{"type": "thinking_delta", "thinking": c.text})
			blockDelta(map[string]any{"type": "signature_delta", "signature": "mock-signature"})
			blockStop()
		case chunkSignature:
			blockDelta(map[string]any{"type": "signature_delta", "signature": c.text})
		case chunkText:
			blockStart(map[string]any{"type": "text", "text": ""})
			blockDelta(map[string]any{"type": "text_delta", "text": c.text})
			blockStop()
		case chunkToolStart:
			blockStart(map[string]any{"type": "tool_use", "id": c.id, "name": c.name, "input": map[string]any{}})
			if c.args != "" {
				blockDelta(map[string]any{"type": "input_json_delta", "partial_json": c.args})
			}
			blockStop()
		case chunkToolArgs:
			blockDelta(map[string]any{"type": "input_json_delta", "partial_json": c.args})
		case chunkUsage:
			// Anthropic carries usage only inside message_delta; a Usage
			// chunk after the finish (unusual ordering) is dropped — it
			// cannot be retro-attached to the already-flushed delta.
			if !finished {
				pendingUsage = &anthropicUsage{InputTokens: c.promptTokens, OutputTokens: c.completionTokens}
			}
		case chunkFinish:
			md := map[string]any{
				"delta": map[string]any{
					"stop_reason":   mapFinishReason(ProtocolAnthropic, c.finish),
					"stop_sequence": nil,
				},
			}
			if pendingUsage != nil {
				md["usage"] = pendingUsage
				pendingUsage = nil // consumed by this message_delta
			}
			event("message_delta", md)
			finished = true
		case chunkDone:
			event("message_stop", map[string]any{})
		case chunkPause:
			select {
			case <-ctx.Done():
				return
			case <-time.After(c.pause):
			}
		case chunkMalformed:
			fmt.Fprint(w, "event: message_start\ndata: {broken json\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
	}

	// A Usage chunk without a trailing Finish still needs to reach the SDK —
	// emit a usage-only message_delta so the client sees the numbers.
	if pendingUsage != nil {
		event("message_delta", map[string]any{
			"delta": map[string]any{"stop_reason": nil, "stop_sequence": nil},
			"usage": pendingUsage,
		})
	}
}

// renderAnthropicJSON renders the non-streaming message response.
func renderAnthropicJSON(w http.ResponseWriter, content, reasoning string) {
	var blocks []any
	if reasoning != "" {
		blocks = append(blocks, map[string]any{
			"type":      "thinking",
			"thinking":  reasoning,
			"signature": "mock-signature",
		})
	}
	if content != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": content})
	}
	writeJSON(w, map[string]any{
		"id":            "msg_mock",
		"type":          "message",
		"role":          "assistant",
		"model":         "mock-model",
		"content":       blocks,
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  0,
			"output_tokens": 0,
		},
	})
}
