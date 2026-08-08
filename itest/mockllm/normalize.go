package mockllm

import (
	"encoding/json"
	"net/http"
	"strings"
)

// ToolCall is the protocol-independent view of a requested tool invocation.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// Message is the protocol-independent view of one request message. Both
// wire formats (OpenAI string/array content, Anthropic typed content blocks)
// are folded into this shape so Require assertions and scenario assertions
// never touch protocol details.
type Message struct {
	Role       string // "system" | "user" | "assistant" | "tool"
	Content    string // concatenated text content
	ToolCallID string // tool messages: the id of the tool call answered
	ToolCalls  []ToolCall
	IsError    bool
	Thinking   string // assistant reasoning/thinking text
}

// Tool is the protocol-independent view of a tool schema advertised in the
// request.
type Tool struct {
	Name        string
	Description string
}

// RecordedRequest is one captured request: the normalized messages/tools plus
// the raw body and headers (headers carry x-tachi-session-id for session
// propagation checks). Protocol lets protocol-aware Require assertions (e.g.
// HasThinkingDisabled) branch on the wire format.
type RecordedRequest struct {
	Protocol Protocol
	Method   string
	Path     string
	Headers  http.Header
	Messages []Message
	Tools    []Tool
	RawBody  []byte
}

// maxRecordedBodySize caps the persisted raw body. Truncation only affects
// the recorded copy — Require assertions always run on the full parsed
// request (see Server.record), so huge tool outputs cannot break assertions.
const maxRecordedBodySize = 64 * 1024

// normalizeRequest parses a request body into the protocol-independent view.
// The protocol is inferred from the path (servers only route their own
// protocol, so this is deterministic).
func normalizeRequest(method, path string, headers http.Header, body []byte) (*RecordedRequest, error) {
	protocol := ProtocolOpenAI
	switch path {
	case "/v1/messages":
		protocol = ProtocolAnthropic
	case "/responses", "/v1/responses":
		protocol = ProtocolOpenAIResponses
	}
	req := &RecordedRequest{
		Protocol: protocol,
		Method:   method,
		Path:     path,
		Headers:  headers.Clone(),
	}
	if len(body) > maxRecordedBodySize {
		req.RawBody = body[:maxRecordedBodySize]
	} else {
		req.RawBody = body
	}
	var err error
	switch path {
	case "/v1/messages":
		err = req.parseAnthropic(body)
	case "/responses", "/v1/responses":
		err = req.parseOpenAIResponses(body)
	default:
		err = req.parseOpenAI(body)
	}
	if err != nil {
		return nil, err
	}
	return req, nil
}

// ── OpenAI (/v1/chat/completions) ─────────────────────────────────────────

type oaiRequestBody struct {
	Model    string       `json:"model"`
	Messages []oaiMessage `json:"messages"`
	Tools    []oaiTool    `json:"tools"`
}

type oaiMessage struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	ReasoningContent string          `json:"reasoning_content"`
	ToolCallID       string          `json:"tool_call_id"`
	ToolCalls        []oaiToolCall   `json:"tool_calls"`
}

type oaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaiTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"function"`
}

func (r *RecordedRequest) parseOpenAI(body []byte) error {
	var o oaiRequestBody
	if err := json.Unmarshal(body, &o); err != nil {
		return err
	}
	for _, om := range o.Messages {
		m := Message{
			Role:       om.Role,
			ToolCallID: om.ToolCallID,
			Thinking:   om.ReasoningContent,
		}
		// content may be a plain string or an array of typed parts.
		if len(om.Content) > 0 && om.Content[0] == '[' {
			var parts []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(om.Content, &parts); err == nil {
				for _, p := range parts {
					if p.Type == "text" {
						m.Content += p.Text
					}
				}
			}
		} else {
			_ = json.Unmarshal(om.Content, &m.Content)
		}
		for _, tc := range om.ToolCalls {
			m.ToolCalls = append(m.ToolCalls, ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
		r.Messages = append(r.Messages, m)
	}
	for _, ot := range o.Tools {
		r.Tools = append(r.Tools, Tool{
			Name:        ot.Function.Name,
			Description: ot.Function.Description,
		})
	}
	return nil
}

// ── Anthropic (/v1/messages) ──────────────────────────────────────────────

type anthropicRequestBody struct {
	Model    string           `json:"model"`
	System   []anthropicBlock `json:"system"`
	Messages []struct {
		Role    string           `json:"role"`
		Content []anthropicBlock `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"tools"`
}

type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Signature string          `json:"signature"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   *bool           `json:"is_error"`
	// tool_result content may be a string or an array of {type:"text",...}.
	Content json.RawMessage `json:"content"`
}

func (r *RecordedRequest) parseAnthropic(body []byte) error {
	var a anthropicRequestBody
	if err := json.Unmarshal(body, &a); err != nil {
		return err
	}
	for _, sb := range a.System {
		if sb.Type == "text" {
			r.Messages = append(r.Messages, Message{Role: "system", Content: sb.Text})
		}
	}
	for _, om := range a.Messages {
		// A user message may carry multiple tool_result blocks (parallel
		// tool calls — the agent's collectToolMessages merges consecutive
		// tool messages into one user message). Each block becomes its own
		// tool message so HasToolResult(id, ...) can match every result
		// individually; text blocks stay in the user message.
		m := Message{Role: om.Role}
		var toolMsg *Message // pending tool message for the current tool_result block
		flushTool := func() {
			if toolMsg != nil {
				r.Messages = append(r.Messages, *toolMsg)
				toolMsg = nil
			}
		}
		for _, b := range om.Content {
			switch b.Type {
			case "text":
				m.Content += b.Text
			case "thinking":
				m.Thinking += b.Thinking
			case "tool_use":
				args := string(b.Input)
				if args == "" || args == "null" {
					args = "{}"
				}
				m.ToolCalls = append(m.ToolCalls, ToolCall{ID: b.ID, Name: b.Name, Arguments: args})
			case "tool_result":
				flushTool()
				tm := Message{Role: "tool", ToolCallID: b.ToolUseID}
				if b.IsError != nil {
					tm.IsError = *b.IsError
				}
				if len(b.Content) > 0 {
					if b.Content[0] == '[' {
						var parts []struct {
							Type string `json:"type"`
							Text string `json:"text"`
						}
						if err := json.Unmarshal(b.Content, &parts); err == nil {
							for _, p := range parts {
								if p.Type == "text" {
									tm.Content += p.Text
								}
							}
						}
					} else {
						_ = json.Unmarshal(b.Content, &tm.Content)
					}
				}
				toolMsg = &tm
			}
		}
		flushTool()
		// A user message whose blocks were ALL tool_results leaves an empty
		// stub — drop it (the results already became their own tool
		// messages). Messages with text/thinking/tool_calls are kept even
		// when the content field itself is empty.
		if m.Content != "" || m.Thinking != "" || len(m.ToolCalls) > 0 {
			r.Messages = append(r.Messages, m)
		}
	}
	for _, t := range a.Tools {
		r.Tools = append(r.Tools, Tool{Name: t.Name, Description: t.Description})
	}
	return nil
}

// ── OpenAI Responses (/responses) ─────────────────────────────────────────

// oaiResponsesRequestBody mirrors the official openai-go Responses API
// request (llm/openai_responses.go buildParams): a flat input item array plus
// instructions and tools. input items are a union of message / function_call
// / function_call_output, folded into the protocol-independent view below.
type oaiResponsesRequestBody struct {
	Model        string                  `json:"model"`
	Instructions string                  `json:"instructions"`
	Input        []oaiResponsesInputItem `json:"input"`
	// Responses API tools are FLAT: {"type":"function","name":...,"parameters":...}
	// (unlike chat completions which nests them under "function").
	Tools []struct {
		Type        string `json:"type"`
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"tools"`
}

type oaiResponsesInputItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Output    json.RawMessage `json:"output"`
}

// concatText folds a content/output value (plain string or array of
// {type:"text"/"input_text"/"output_text", text:...} parts) into one string.
func concatText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '[' {
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &parts); err == nil {
			var sb strings.Builder
			for _, p := range parts {
				if p.Text != "" {
					sb.WriteString(p.Text)
				}
			}
			return sb.String()
		}
	}
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

// parseOpenAIResponses normalizes a POST /responses request. The flat item
// array maps onto the same protocol-independent Message view as the other
// wires: function_call items are attached to the preceding assistant message
// (the Responses API serializes tool calls as standalone items, unlike chat
// completions which nests them), and function_call_output becomes a tool
// message.
func (r *RecordedRequest) parseOpenAIResponses(body []byte) error {
	var o oaiResponsesRequestBody
	if err := json.Unmarshal(body, &o); err != nil {
		return err
	}
	if o.Instructions != "" {
		r.Messages = append(r.Messages, Message{Role: "system", Content: o.Instructions})
	}
	for _, item := range o.Input {
		switch item.Type {
		case "message":
			r.Messages = append(r.Messages, Message{
				Role:    item.Role,
				Content: concatText(item.Content),
			})
		case "function_call":
			// Attach to the preceding assistant message, mirroring the
			// nested tool_calls shape of the chat-completions wire so
			// protocol-agnostic assertions stay uniform.
			if len(r.Messages) > 0 && r.Messages[len(r.Messages)-1].Role == "assistant" {
				last := &r.Messages[len(r.Messages)-1]
				last.ToolCalls = append(last.ToolCalls, ToolCall{
					ID:        item.CallID,
					Name:      item.Name,
					Arguments: item.Arguments,
				})
			} else {
				// Defensive: no preceding assistant message — emit a bare one.
				r.Messages = append(r.Messages, Message{
					Role: "assistant",
					ToolCalls: []ToolCall{{
						ID:        item.CallID,
						Name:      item.Name,
						Arguments: item.Arguments,
					}},
				})
			}
		case "function_call_output":
			r.Messages = append(r.Messages, Message{
				Role:       "tool",
				ToolCallID: item.CallID,
				Content:    concatText(item.Output),
			})
		}
	}
	for _, t := range o.Tools {
		r.Tools = append(r.Tools, Tool{Name: t.Name, Description: t.Description})
	}
	return nil
}
