package mockllm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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
			oaiUsage := map[string]any{
				"prompt_tokens":     c.promptTokens,
				"completion_tokens": c.completionTokens,
				"total_tokens":      c.promptTokens + c.completionTokens,
			}
			// Cache-read tokens ride in prompt_tokens_details.cached_tokens
			// (go-openai parses them into Usage.CacheReadInputTokens).
			oaiUsage["prompt_tokens_details"] = map[string]any{"cached_tokens": c.cacheReadTokens}
			data(map[string]any{
				"choices": []any{},
				"usage":   oaiUsage,
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
		case chunkPing:
			// OpenAI has no heartbeat event — ignored.
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

// ── OpenAI Responses wire format ───────────────────────────────────────────
//
// The official openai-go SDK parses responses-streaming SSE by unmarshalling
// the `data:` JSON directly (the `event:` name is decorative unless the
// "thread." prefix or synthesizeEventData mode is used) — the JSON's own
// "type" field discriminates the event union. Usage arrives ONLY on the
// terminal response.completed event (inside response.usage); mid-stream
// events carry deltas only. renderOpenAIResponsesStream therefore buffers
// the output items / usage / finish reason while emitting delta events, then
// flushes everything in the completed event at Done.

// oaiResponsesUsage mirrors the real Responses API usage payload: input /
// output totals plus the cache accounting (cached_tokens /
// cache_write_tokens) and reasoning tokens. Only attached to the terminal
// response object.
type oaiResponsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	TotalTokens        int `json:"total_tokens"`
	InputTokensDetails struct {
		CachedTokens     int `json:"cached_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func renderOpenAIResponsesStream(ctx context.Context, w http.ResponseWriter, chunks []Chunk) {
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

	var (
		text      strings.Builder // accumulated output_text (message item + delta)
		textItem  map[string]any  // live message item in output (flushed on first text)
		output    []any           // full output items echoed in the completed response
		toolCalls []*struct {
			ID        string
			CallID    string
			Name      string
			Arguments strings.Builder
			item      map[string]any // the live function_call item in output; Arguments mirror it
		}
		usage  *oaiResponsesUsage
		finish string // scripted finish reason ("stop"/"tool_calls"/"length")
		// NOTE: output_index is NOT the real API's global item counter —
		// thinking/text deltas share the current index and only tool starts
		// increment it. The SDK client tracks tool indices by item_id, so the
		// difference is invisible; kept simple on purpose.
		idx int // output_index / item counter
	)
	itemID := func(prefix string) string { return fmt.Sprintf("%s_%d", prefix, idx) }

	for _, c := range chunks {
		switch c.kind {
		case chunkThinking:
			// Reasoning deltas stream as standalone events; the reasoning
			// item itself is not announced (the client only consumes deltas).
			data(map[string]any{
				"type":    "response.reasoning_text.delta",
				"item_id": itemID("rs"), "output_index": idx, "content_index": 0,
				"delta": c.text,
			})
		case chunkText:
			// Flush the message item into output on the FIRST text delta so
			// it precedes any later function_call items (the real API emits
			// text before tool calls) — deriveResponsesFinishReason scans the
			// trailing output item for the finish reason.
			if textItem == nil {
				textItem = map[string]any{
					"type": "message", "id": itemID("msg"), "role": "assistant",
					"status": "completed",
					"content": []any{map[string]any{
						"type": "output_text", "text": "", "annotations": []any{},
					}},
				}
				output = append(output, textItem)
			}
			data(map[string]any{
				"type":    "response.output_text.delta",
				"item_id": itemID("msg"), "output_index": idx, "content_index": 0,
				"delta": c.text,
			})
			text.WriteString(c.text)
			textItem["content"] = []any{map[string]any{
				"type": "output_text", "text": text.String(), "annotations": []any{},
			}}
		case chunkToolStart:
			// Announce the function_call item (the client maps item_id →
			// sequential tool index here), then stream its initial args.
			id := itemID("fc")
			item := map[string]any{
				"type": "function_call", "id": id,
				"call_id": c.id, "name": c.name, "arguments": "",
			}
			data(map[string]any{
				"type":    "response.output_item.added",
				"item_id": id, "output_index": idx, "item": item,
			})
			tc := &struct {
				ID        string
				CallID    string
				Name      string
				Arguments strings.Builder
				item      map[string]any
			}{ID: id, CallID: c.id, Name: c.name, item: item}
			toolCalls = append(toolCalls, tc)
			output = append(output, item)
			if c.args != "" {
				tc.Arguments.WriteString(c.args)
				tc.item["arguments"] = tc.Arguments.String() // keep the echoed item in sync
				data(map[string]any{
					"type":    "response.function_call_arguments.delta",
					"item_id": id, "output_index": idx, "delta": c.args,
				})
			}
			idx++
		case chunkToolArgs:
			if len(toolCalls) > 0 {
				tc := toolCalls[len(toolCalls)-1]
				tc.Arguments.WriteString(c.args)
				tc.item["arguments"] = tc.Arguments.String() // keep the echoed item in sync
				data(map[string]any{
					"type":    "response.function_call_arguments.delta",
					"item_id": tc.ID, "output_index": idx - 1, "delta": c.args,
				})
			}
		case chunkFinish:
			finish = c.finish
		case chunkUsage:
			u := &oaiResponsesUsage{}
			u.InputTokens = c.promptTokens
			u.OutputTokens = c.completionTokens
			u.TotalTokens = c.promptTokens + c.completionTokens
			u.InputTokensDetails.CachedTokens = c.cacheReadTokens
			u.InputTokensDetails.CacheWriteTokens = c.cacheCreationToks
			usage = u
		case chunkDone:
			// The message item was already flushed into output on the first
			// text delta; now emit the terminal event carrying the full
			// response object + usage.
			if finish == "length" || finish == "max_tokens" {
				resp := map[string]any{
					"id": "resp_mock", "object": "response",
					"created_at": time.Now().Unix(), "status": "incomplete",
					"model": "mock-model", "output": output,
					"incomplete_details": map[string]any{"reason": "max_output_tokens"},
				}
				if usage != nil {
					resp["usage"] = usage // same accounting as the completed path
				}
				data(map[string]any{"type": "response.incomplete", "response": resp})
				return
			}
			resp := map[string]any{
				"id": "resp_mock", "object": "response",
				"created_at": time.Now().Unix(), "status": "completed",
				"model": "mock-model", "output": output,
			}
			if usage != nil {
				resp["usage"] = usage
			}
			data(map[string]any{"type": "response.completed", "response": resp})
			return
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
			// Responses reasoning has no signature concept — ignored.
		case chunkPing:
			// Responses API has no heartbeat event — ignored.
		}
	}
	// No Done chunk: close the stream (the client's fallback emits a clean
	// stop without a completed event).
}

// renderOpenAIResponsesJSON renders the non-streaming Responses API response
// (CreateChat calls — title generation, /compact, deepresearch).
func renderOpenAIResponsesJSON(w http.ResponseWriter, content, reasoning string, toolCallChunks []Chunk) {
	var output []any
	if reasoning != "" {
		output = append(output, map[string]any{
			"type": "reasoning", "id": "rs_0",
			// Real DeepSeek /responses returns reasoning as PLAINTEXT
			// reasoning_text parts in content (summary is empty) — mirror
			// that so the client's reasoning collection path is exercised.
			"summary": []any{},
			"content": []any{map[string]any{
				"type": "reasoning_text", "text": reasoning, "annotations": []any{},
			}},
		})
	}
	if content != "" {
		output = append(output, map[string]any{
			"type": "message", "id": "msg_0", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{
				"type": "output_text", "text": content, "annotations": []any{},
			}},
		})
	}
	for _, c := range toolCallChunks {
		switch c.kind {
		case chunkToolStart:
			output = append(output, map[string]any{
				"type": "function_call", "id": "fc_0",
				"call_id": c.id, "name": c.name, "arguments": c.args,
			})
		case chunkToolArgs:
			// Append to the last function_call's arguments — mirrors
			// renderOpenAIJSON so split-argument streams stay intact.
			if len(output) > 0 {
				if last, ok := output[len(output)-1].(map[string]any); ok && last["type"] == "function_call" {
					last["arguments"] = last["arguments"].(string) + c.args
				}
			}
		}
	}
	resp := map[string]any{
		"id": "resp_mock", "object": "response",
		"created_at": time.Now().Unix(), "status": "completed",
		"model": "mock-model", "output": output,
		"usage": oaiResponsesUsage{TotalTokens: 0},
	}
	writeJSON(w, resp)
}

// anthropicUsage mirrors the real API's usage payload: input/output plus the
// cache accounting fields (cache_creation_input_tokens / cache_read_input_tokens)
// and service_tier that message_start and message_delta carry.
type anthropicUsage struct {
	InputTokens              int    `json:"input_tokens"`
	OutputTokens             int    `json:"output_tokens"`
	CacheCreationInputTokens int    `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int    `json:"cache_read_input_tokens"`
	ServiceTier              string `json:"service_tier"`
}

// anthropicZeroUsage is the baseline usage attached to message_start, matching
// the real API's field set (cache counts 0, service tier "standard").
func anthropicZeroUsage() anthropicUsage {
	return anthropicUsage{ServiceTier: "standard"}
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
			"usage":         anthropicZeroUsage(),
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
				u := anthropicZeroUsage()
				u.InputTokens = c.promptTokens
				u.OutputTokens = c.completionTokens
				u.CacheReadInputTokens = c.cacheReadTokens
				u.CacheCreationInputTokens = c.cacheCreationToks
				pendingUsage = &u
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
		case chunkPing:
			// Heartbeat event ({"type":"ping"}) — the SDK consumes it
			// silently, exactly like real APIs send between blocks.
			event("ping", map[string]any{})
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
		"usage":         anthropicZeroUsage(),
	})
}
