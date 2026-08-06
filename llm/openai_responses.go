package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

// OpenAIResponsesProvider implements the Provider interface on top of the
// OpenAI Responses API (POST /responses) using the official openai-go SDK.
//
// Only the client-managed history ("stateless") mode is supported: every
// request carries the full conversation as a flat input array of message /
// function_call / function_call_output items. The server-side state chain
// (previous_response_id, store) is deliberately not used.
type OpenAIResponsesProvider struct {
	client  openai.Client
	model   string
	apiKey  string
	baseURL string
	name    string // config provider name ("" = unknown); see Provider.ProviderName
}

func NewOpenAIResponsesProvider(apiKey, baseURL, model string) *OpenAIResponsesProvider {
	clientOpts := []option.RequestOption{
		option.WithAPIKey(apiKey),
		// Wrap the default HTTP client with a transport that injects the
		// session ID header from context, allowing per-request session
		// tracking (same as the chat-completions path).
		option.WithHTTPClient(&http.Client{Transport: &tachiTransport{base: http.DefaultTransport}}),
	}
	if baseURL != "" {
		// Only override when set: an empty WithBaseURL produces an empty URL
		// (the SDK falls back to its default only when the option is absent),
		// which would break request construction.
		clientOpts = append(clientOpts, option.WithBaseURL(baseURL))
	}
	client := openai.NewClient(clientOpts...)
	return &OpenAIResponsesProvider{
		client:  client,
		model:   model,
		apiKey:  apiKey,
		baseURL: baseURL,
	}
}

func (p *OpenAIResponsesProvider) Name() string         { return ProviderTypeOpenAIResponses }
func (p *OpenAIResponsesProvider) Model() string        { return p.model }
func (p *OpenAIResponsesProvider) ProviderName() string { return p.name }

// roleForResponses maps a tachi message role to the Responses API role.
// system and developer messages are handled by convertMessages (joined into
// instructions), so they are never produced here — developer is kept for
// symmetry with the API type.
func roleForResponses(role string) responses.EasyInputMessageRole {
	switch role {
	case "assistant":
		return responses.EasyInputMessageRoleAssistant
	case "system":
		return responses.EasyInputMessageRoleSystem
	case "developer":
		return responses.EasyInputMessageRoleDeveloper
	default:
		return responses.EasyInputMessageRoleUser
	}
}

// responsesContent builds the content union for an input message. Plain text
// uses the string shorthand; multi-modal messages use the content array form
// (input_text / input_image).
func responsesContent(msg Message) responses.EasyInputMessageContentUnionParam {
	if len(msg.ContentParts) == 0 {
		return responses.EasyInputMessageContentUnionParam{OfString: openai.String(msg.Content)}
	}
	parts := make([]responses.ResponseInputContentUnionParam, 0, len(msg.ContentParts))
	for _, part := range msg.ContentParts {
		switch part.Type {
		case ContentPartText:
			parts = append(parts, responses.ResponseInputContentUnionParam{
				OfInputText: &responses.ResponseInputTextParam{Text: part.Text},
			})
		case ContentPartImage:
			// Responses API accepts base64 images as data URIs, same as
			// chat completions.
			dataURI := "data:" + part.MediaType + ";base64," + part.Data
			parts = append(parts, responses.ResponseInputContentUnionParam{
				OfInputImage: &responses.ResponseInputImageParam{
					Detail:   responses.ResponseInputImageDetailAuto,
					ImageURL: openai.String(dataURI),
				},
			})
		}
	}
	return responses.EasyInputMessageContentUnionParam{
		OfInputItemContentList: parts,
	}
}

// convertMessages converts tachi llm.Messages into Responses API input items.
//
// Responses API uses a flat item array: assistant tool calls are NOT nested
// inside the assistant message (unlike chat completions) — they become
// standalone function_call items following the message, and tool results
// become function_call_output items. system and developer messages are
// collected into the instructions field (the official recommendation)
// instead of appearing in input.
//
// Thinking blocks are dropped: the Responses protocol forbids resending
// previous-turn reasoning content.
func (p *OpenAIResponsesProvider) convertMessages(messages []Message) (instructions string, items []responses.ResponseInputItemUnionParam) {
	var systemParts []string
	for _, msg := range messages {
		role := msg.Role
		if role == RoleSteer {
			role = "user" // Responses has no steer role; user is safe
		}
		switch role {
		case "system", "developer":
			systemParts = append(systemParts, msg.Content)
			continue
		case "tool":
			if msg.ToolCallID == "" {
				// The protocol requires call_id on function_call_output.
				// Skip malformed entries rather than sending a bad request.
				continue
			}
			output := msg.Content
			if msg.IsError && output != "" {
				output = "[error] " + output
			}
			items = append(items, responses.ResponseInputItemUnionParam{
				OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
					CallID: msg.ToolCallID,
					Output: responses.ResponseInputItemFunctionCallOutputOutputUnionParam{
						OfString: openai.String(output),
					},
				},
			})
			continue
		}

		// user / assistant message. When an assistant message carries tool
		// calls, emit the message first and the function_call items after it.
		if role == "assistant" && len(msg.ToolCalls) > 0 {
			items = append(items, responses.ResponseInputItemUnionParam{
				OfMessage: &responses.EasyInputMessageParam{
					Type:    responses.EasyInputMessageTypeMessage,
					Role:    responses.EasyInputMessageRoleAssistant,
					Content: responsesContent(msg),
				},
			})
			for _, tc := range msg.ToolCalls {
				args := tc.Function.Arguments
				if args != "" && !json.Valid([]byte(args)) {
					// Arguments may be incomplete (e.g. truncated by
					// max_output_tokens); degrade to empty object rather than
					// sending malformed JSON.
					args = "{}"
				}
				callID := tc.ID
				if callID == "" {
					callID = fmt.Sprintf("call_%d", len(items))
				}
				items = append(items, responses.ResponseInputItemUnionParam{
					OfFunctionCall: &responses.ResponseFunctionToolCallParam{
						CallID:    callID,
						Name:      tc.Function.Name,
						Arguments: args,
					},
				})
			}
			continue
		}

		items = append(items, responses.ResponseInputItemUnionParam{
			OfMessage: &responses.EasyInputMessageParam{
				Type:    responses.EasyInputMessageTypeMessage,
				Role:    roleForResponses(role),
				Content: responsesContent(msg),
			},
		})
	}
	instructions = strings.Join(systemParts, "\n")
	return instructions, items
}

// convertTools converts tachi Tool definitions into Responses API function
// tools. strict mode is enabled per tool only when the schema satisfies the
// strict-mode constraints (every object node lists all of its properties in
// "required"); otherwise the schema is sent as-is with strict off — sending
// a non-compliant schema with strict=true makes the API reject the request
// with a 400.
func (p *OpenAIResponsesProvider) convertTools(tools []Tool) []responses.ToolUnionParam {
	out := make([]responses.ToolUnionParam, 0, len(tools))
	for _, tool := range tools {
		data, err := json.Marshal(tool.Parameters)
		if err != nil {
			continue
		}
		var params map[string]any
		if err := json.Unmarshal(data, &params); err != nil {
			continue
		}
		ft := responses.FunctionToolParam{
			Name:       tool.Name,
			Parameters: params,
		}
		if strictCompatibleSchema(params) {
			ft.Strict = openai.Bool(true)
		}
		if tool.Description != "" {
			ft.Description = openai.String(tool.Description)
		}
		out = append(out, responses.ToolUnionParam{OfFunction: &ft})
	}
	return out
}

// strictCompatibleSchema reports whether a JSON Schema is compatible with
// OpenAI strict mode. Strict mode requires every object node (including
// nested ones under "items") to list all of its properties in "required".
//
// Strict mode also rejects the "default" keyword (unsupported in the strict
// JSON Schema subset — sending one with strict=true returns 400). Any node
// carrying "default" forces the whole schema to strict=false. This keeps the
// tool schemas (which now declare defaults, see tools.PropertySchema.Default)
// safe from a future regression where every parameter of a tool is required
// AND has a default.
func strictCompatibleSchema(schema map[string]any) bool {
	if _, has := schema["default"]; has {
		return false
	}
	props, _ := schema["properties"].(map[string]any)
	if len(props) > 0 {
		required := make(map[string]bool)
		for _, r := range asStringList(schema["required"]) {
			required[r] = true
		}
		for key, prop := range props {
			if !required[key] {
				return false
			}
			if sub, ok := prop.(map[string]any); ok && !strictCompatibleSchema(sub) {
				return false
			}
		}
	}
	if items, ok := schema["items"].(map[string]any); ok {
		return strictCompatibleSchema(items)
	}
	return true
}

// asStringList coerces a decoded JSON value to a []string ([]any of strings
// or []string), returning nil for anything else.
func asStringList(v any) []string {
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return t
	}
	return nil
}

// buildParams assembles the Responses API request parameters.
//
// Reasoning / thinking mapping. The Responses API is more capable than chat
// completions here: reasoning.effort supports "none", so an explicitly
// disabled thinking level can actually turn reasoning off on reasoning
// models (chat completions can only omit the field, which leaves the model's
// default effort — usually medium — in effect). Non-reasoning models get no
// reasoning field at all: they don't reason anyway, and some servers reject
// the param for them.
//
// When an effort is set, reasoning.summary is requested ("auto") so that
// non-streaming responses carry reasoning text (the reasoning output item's
// content is encrypted by default on POST /responses).
func (p *OpenAIResponsesProvider) buildParams(messages []Message, tools []Tool, opts ChatOptions) responses.ResponseNewParams {
	instructions, items := p.convertMessages(messages)
	params := responses.ResponseNewParams{
		Model: p.model,
		Input: responses.ResponseNewParamsInputUnion{
			OfInputItemList: items,
		},
		Tools:             p.convertTools(tools),
		Store:             openai.Bool(false), // stateless: never persist server-side
		Temperature:       openai.Float(1),
		ParallelToolCalls: openai.Bool(true),
	}
	if opts.MaxTokens > 0 {
		params.MaxOutputTokens = openai.Int(int64(opts.MaxTokens))
	}
	if instructions != "" {
		params.Instructions = openai.String(instructions)
	}
	switch {
	case opts.Thinking != nil && !*opts.Thinking:
		// Explicitly disabled: turn reasoning off on models that support it.
		if isReasoningModelPrefix(p.model) {
			params.Reasoning = shared.ReasoningParam{Effort: shared.ReasoningEffortNone}
		}
	case opts.ThinkingEffort != "":
		params.Reasoning = shared.ReasoningParam{
			Effort:  shared.ReasoningEffort(opts.ThinkingEffort),
			Summary: shared.ReasoningSummaryAuto,
		}
	case opts.Thinking != nil && *opts.Thinking:
		// Explicitly enabled with the provider default effort: only request
		// a summary so non-streaming responses still surface reasoning text.
		params.Reasoning = shared.ReasoningParam{Summary: shared.ReasoningSummaryAuto}
	}
	return params
}

func (p *OpenAIResponsesProvider) CreateChat(ctx context.Context, messages []Message, tools []Tool, opts ChatOptions) (*Response, error) {
	if opts.SessionID != "" {
		ctx = WithSessionID(ctx, opts.SessionID)
	}
	resp, err := p.client.Responses.New(ctx, p.buildParams(messages, tools, opts))
	if err != nil {
		return nil, err
	}
	return convertResponsesToLLM(resp), nil
}

// convertResponsesToLLM maps a non-streaming Responses API response to the
// common llm.Response.
func convertResponsesToLLM(resp *responses.Response) *Response {
	out := &Response{}
	var texts, reasoning []string
	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if part.Type == "output_text" {
					texts = append(texts, part.Text)
				}
			}
		case "function_call":
			out.ToolCalls = append(out.ToolCalls, ToolCall{
				ID:   item.CallID,
				Type: "function",
				Function: ToolCallFunction{
					Name:      item.Name,
					Arguments: item.Arguments.OfString,
				},
			})
		case "reasoning":
			// Prefer the (server-generated) summary, then fall back to any
			// reasoning_text parts that made it through unencrypted.
			for _, s := range item.Summary {
				reasoning = append(reasoning, s.Text)
			}
			for _, part := range item.Content {
				if part.Type == "reasoning_text" {
					reasoning = append(reasoning, part.Text)
				}
			}
		}
	}
	out.Content = strings.Join(texts, "")
	out.FinishReason = deriveResponsesFinishReason(resp)
	if len(reasoning) > 0 {
		out.Reasoning = strings.Join(reasoning, "\n")
	}
	if resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0 {
		out.Usage = usageFromResponsesUsage(resp.Usage)
	}
	return out
}

// usageFromResponsesUsage maps the Responses usage object into the common
// Usage struct. Reasoning tokens are included in OutputTokens (OpenAI bills
// them as output).
func usageFromResponsesUsage(u responses.ResponseUsage) *Usage {
	return &Usage{
		InputTokens:              u.InputTokens,
		LastInputTokens:          u.InputTokens,
		OutputTokens:             u.OutputTokens,
		CacheCreationInputTokens: u.InputTokensDetails.CacheWriteTokens,
		CacheReadInputTokens:     u.InputTokensDetails.CachedTokens,
	}
}

// deriveResponsesFinishReason infers a chat-completions-style finish reason
// from a Responses API response object. Responses has no finish_reason field:
// an incomplete response with max_output_tokens maps to "length", a trailing
// function_call output maps to "tool_calls", everything else is "stop".
func deriveResponsesFinishReason(resp *responses.Response) string {
	switch resp.Status {
	case responses.ResponseStatusIncomplete:
		if resp.IncompleteDetails.Reason == "max_output_tokens" {
			return "length"
		}
		return "stop"
	case responses.ResponseStatusFailed:
		return "error"
	}
	for i := len(resp.Output) - 1; i >= 0; i-- {
		switch resp.Output[i].Type {
		case "function_call":
			return "tool_calls"
		case "message":
			return "stop" // last meaningful output is text
		}
	}
	return "stop"
}

func (p *OpenAIResponsesProvider) CreateChatStream(ctx context.Context, messages []Message, tools []Tool, opts ChatOptions) (<-chan StreamEvent, error) {
	if opts.SessionID != "" {
		ctx = WithSessionID(ctx, opts.SessionID)
	}
	stream := p.client.Responses.NewStreaming(ctx, p.buildParams(messages, tools, opts))
	ch := make(chan StreamEvent, 32)
	go func() {
		defer close(ch)
		defer stream.Close()

		d := &responsesStreamDispatcher{
			ctx:             ctx,
			log:             logger.FromContext(ctx),
			toolIndexByItem: map[string]int{},
		}
		for stream.Next() {
			if done := d.emit(stream.Current(), ch); done {
				return
			}
		}
		if err := stream.Err(); err != nil {
			ch <- StreamEvent{Type: StreamEventError, Error: err}
			return
		}
		// Stream ended without a terminal event — report a clean stop.
		if !d.finished {
			ch <- StreamEvent{Type: StreamEventDone, FinishReason: "stop"}
		}
	}()
	return ch, nil
}

// responsesStreamDispatcher converts Responses API streaming events into
// common StreamEvents, tracking tool call indices across events.
type responsesStreamDispatcher struct {
	ctx             context.Context
	log             *logger.Logger
	nextToolIndex   int
	toolIndexByItem map[string]int // item_id → sequential tool index
	finished        bool
}

// emit dispatches a single stream event. It returns true when the stream has
// reached a terminal state (completed / failed / error) and the goroutine
// should stop reading.
func (d *responsesStreamDispatcher) emit(ev responses.ResponseStreamEventUnion, ch chan<- StreamEvent) bool {
	switch ev.Type {
	case "response.output_item.added":
		// function_call items announce the tool name + call_id before the
		// argument deltas arrive. Assign a sequential index per item (the
		// API's output_index is a global counter, not a per-tool index).
		if ev.Item.Type == "function_call" {
			idx := d.nextToolIndex
			d.nextToolIndex++
			if ev.Item.ID != "" {
				d.toolIndexByItem[ev.Item.ID] = idx
			}
			ch <- StreamEvent{
				Type:      StreamEventToolUseStart,
				ToolIndex: idx,
				ToolCall: &ToolCall{
					ID:   ev.Item.CallID,
					Type: "function",
					Function: ToolCallFunction{
						Name: ev.Item.Name,
					},
				},
			}
		}

	case "response.function_call_arguments.delta":
		ch <- StreamEvent{
			Type:       StreamEventInputJSONDelta,
			ToolIndex:  d.toolIndexByItem[ev.ItemID],
			InputDelta: ev.Delta,
		}

	case "response.output_text.delta":
		ch <- StreamEvent{Type: StreamEventTextDelta, TextDelta: ev.Delta}

	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		ch <- StreamEvent{Type: StreamEventThinkingDelta, ThinkingDelta: ev.Delta}

	case "response.completed":
		d.finished = true
		var usage *Usage
		if ev.Response.Usage.InputTokens > 0 || ev.Response.Usage.OutputTokens > 0 {
			usage = usageFromResponsesUsage(ev.Response.Usage)
		}
		ch <- StreamEvent{
			Type:         StreamEventDone,
			FinishReason: deriveResponsesFinishReason(&ev.Response),
			Usage:        usage,
		}
		return true

	case "response.incomplete":
		// e.g. max_output_tokens reached: same terminal handling as
		// completed, and the derived finish reason keeps the "length"
		// signal instead of falling through to the clean-stop fallback.
		d.finished = true
		var usage *Usage
		if ev.Response.Usage.InputTokens > 0 || ev.Response.Usage.OutputTokens > 0 {
			usage = usageFromResponsesUsage(ev.Response.Usage)
		}
		ch <- StreamEvent{
			Type:         StreamEventDone,
			FinishReason: deriveResponsesFinishReason(&ev.Response),
			Usage:        usage,
		}
		return true

	case "response.failed":
		d.finished = true
		msg := ev.Response.Error.Message
		if msg == "" {
			msg = "response failed"
		}
		d.log.Error(d.ctx, "openai responses: response failed",
			fmt.Errorf("%s", msg), "code", ev.Response.Error.Code)
		ch <- StreamEvent{Type: StreamEventError, Error: fmt.Errorf("openai responses: %s", msg)}
		return true

	case "error":
		// SSE-level error event.
		d.finished = true
		msg := ev.Message
		if msg == "" {
			msg = "stream error"
		}
		ch <- StreamEvent{Type: StreamEventError, Error: fmt.Errorf("openai responses stream: %s", msg)}
		return true
	}
	return false
}
