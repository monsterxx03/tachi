package mockllm

import (
	"strconv"
	"strings"
)

// Matcher mirrors the single method of gomega's GomegaMatcher, so scenarios
// can pass gomega matchers (ContainSubstring, Equal, ...) directly while the
// mockllm core stays free of the gomega dependency.
type Matcher interface {
	Match(actual any) (success bool, err error)
}

// RequireFunc is a precondition checked against an incoming request before
// its scripted reply is served. It returns a failure reason ("" = pass) so
// the server can dump the offending request when a precondition fails.
type RequireFunc func(req *RecordedRequest) (reason string)

// HasSystemPrompt requires the request to carry a system message whose
// content matches m (the language/tone system prompt injected by the agent).
func HasSystemPrompt(m Matcher) RequireFunc {
	return func(req *RecordedRequest) string {
		for _, msg := range req.Messages {
			if msg.Role == "system" {
				if ok, err := m.Match(msg.Content); err == nil && ok {
					return ""
				}
			}
		}
		return "expected a system message matching the matcher"
	}
}

// HasUserMessage requires a user message matching m (the first user message
// or a steered/pending input injected mid-turn).
func HasUserMessage(m Matcher) RequireFunc {
	return func(req *RecordedRequest) string {
		for _, msg := range req.Messages {
			if msg.Role == "user" {
				if ok, err := m.Match(msg.Content); err == nil && ok {
					return ""
				}
			}
		}
		return "expected a user message matching the matcher"
	}
}

// HasToolResult requires a tool message answering tool call id whose content
// matches m. The content is the raw tool output before wrapping (Bash output,
// file content, ...).
func HasToolResult(id string, m Matcher) RequireFunc {
	return func(req *RecordedRequest) string {
		for _, msg := range req.Messages {
			if msg.Role == "tool" && msg.ToolCallID == id {
				if ok, err := m.Match(msg.Content); err == nil && ok {
					return ""
				}
			}
		}
		return "expected a tool result for " + id + " matching the matcher"
	}
}

// HasToolError requires a tool message answering id that carries an error
// (the agent failed to execute the tool — e.g. filtered by --allowed-tools).
// Error signals are protocol-dependent: the Anthropic wire carries is_error
// on tool_result blocks, while OpenAI has no such field — the agent folds
// failures into "Error: ..." content there. Accept both.
func HasToolError(id string) RequireFunc {
	return func(req *RecordedRequest) string {
		for _, msg := range req.Messages {
			if msg.Role == "tool" && msg.ToolCallID == id &&
				(msg.IsError || strings.HasPrefix(msg.Content, "Error:")) {
				return ""
			}
		}
		return "expected an error tool result for " + id
	}
}

// HasThinking requires an assistant message whose thinking content matches m
// (thinking blocks round-tripped into the history on multi-turn requests).
func HasThinking(m Matcher) RequireFunc {
	return func(req *RecordedRequest) string {
		for _, msg := range req.Messages {
			if msg.Thinking != "" {
				if ok, err := m.Match(msg.Thinking); err == nil && ok {
					return ""
				}
			}
		}
		return "expected an assistant message with matching thinking content"
	}
}

// HasTool requires the request to advertise a tool named name in its tools
// array (the agent only advertises registered tools — the --allowed-tools
// assertion surface for -p mode).
func HasTool(name string) RequireFunc {
	return func(req *RecordedRequest) string {
		for _, t := range req.Tools {
			if t.Name == name {
				return ""
			}
		}
		return "expected the request to advertise tool " + name
	}
}

// HasNoTool requires the request NOT to advertise a tool named name.
func HasNoTool(name string) RequireFunc {
	return func(req *RecordedRequest) string {
		for _, t := range req.Tools {
			if t.Name == name {
				return "expected the request NOT to advertise tool " + name
			}
		}
		return ""
	}
}

// HasNTools requires exactly n tools advertised.
func HasNTools(n int) RequireFunc {
	return func(req *RecordedRequest) string {
		if len(req.Tools) == n {
			return ""
		}
		return "expected exactly " + strconv.Itoa(n) + " tools, got " + strconv.Itoa(len(req.Tools))
	}
}

// HasModel requires the request model to match m ("mock-model").
func HasModel(m Matcher) RequireFunc {
	return func(req *RecordedRequest) string {
		// The model lives in the raw body (not the normalized view) — extract
		// it defensively; missing model simply fails the match.
		var body struct {
			Model string `json:"model"`
		}
		if err := unmarshalJSON(req.RawBody, &body); err != nil {
			return "failed to parse request body for model check"
		}
		if ok, err := m.Match(body.Model); err == nil && ok {
			return ""
		}
		return "expected model matching the matcher"
	}
}
