package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// API request recording — per-call capture of the system prompt and tool
// schemas sent to the LLM, persisted to the session's api_requests.jsonl.
// The /transcript report consumes these to show what the model actually saw
// on each turn. Recording is strictly best-effort: a failure is logged and
// never breaks the agent loop.

// recordAPIRequest persists one LLM API call's request context (system prompt
// + tool schemas). Call right before Provider.CreateChatStream.
//
// Main sessions write to the session's api_requests.jsonl. Side-channel runs
// (one-off tasks, ambient — flagged SkipSessionWrites) keep their trail in
// the one-off sidecar as "api_request" lines when one is attached. Sub-agents
// have no one-off recorder, so their requests are not recorded anywhere.
func (a *AIAgent) recordAPIRequest(ctx context.Context, rs *RunState, provider llm.Provider, opts llm.ChatOptions, tools []llm.Tool) {
	systemPrompt := extractSystemPrompt(rs.Messages)
	req := &session.APIRequest{
		Timestamp:    time.Now(),
		Iteration:    rs.APICalls,
		Seq:          rs.Seq, // session-wide request number shared with request-bound messages
		SystemPrompt: systemPrompt,
		UserPrompt:   extractUserPrompt(rs.Messages),
		Model:        requestModel(provider),
		Provider:     requestProvider(provider),
		Thinking:     requestThinking(opts),
		Tools:        toAPITools(tools),
	}

	if rs.SkipSessionWrites {
		if rs.OneoffRec != nil {
			rs.OneoffRec.recordAPIRequest(req)
		}
		return
	}

	if a.Config.SessionManager == nil {
		return
	}
	cur := a.Config.SessionManager.Current()
	if cur == nil {
		return
	}

	if err := a.Config.SessionManager.AppendAPIRequest(req); err != nil {
		a.logWarn(ctx, "agent: failed to record api request", err)
	}
}

// requestModel returns the model name a request was sent to ("" when the
// provider is unknown).
func requestModel(p llm.Provider) string {
	if p == nil {
		return ""
	}
	return p.Model()
}

// requestProvider returns the config provider name backing the request
// ("" when unknown — see llm.Provider.ProviderName).
func requestProvider(p llm.Provider) string {
	if p == nil {
		return ""
	}
	return p.ProviderName()
}

// requestThinking resolves the thinking mode actually used for a request:
// "none" when explicitly disabled, the reasoning effort when set, or ""
// (provider default) otherwise.
func requestThinking(opts llm.ChatOptions) string {
	if opts.Thinking != nil && !*opts.Thinking {
		return "none"
	}
	if opts.ThinkingEffort != "" {
		return opts.ThinkingEffort
	}
	return ""
}

// extractSystemPrompt returns the system message content from the message
// slice the LLM call is about to receive (messages[0] when it is a system
// message). Empty string when there is no system message.
func extractSystemPrompt(messages []llm.Message) string {
	if len(messages) > 0 && messages[0].Role == "system" {
		return messages[0].Content
	}
	return ""
}

// extractUserPrompt returns the latest user (or steer) message content from
// the message slice the LLM call is about to receive — the input this call is
// answering (may be a reminder-wrapped user message, a steer injection, or a
// length-continuation prompt). Empty string when none is present.
func extractUserPrompt(messages []llm.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" || messages[i].Role == llm.RoleSteer {
			return messages[i].Content
		}
	}
	return ""
}

// toAPITools converts llm.Tool definitions to their session-storable form,
// serializing each parameter schema into raw JSON so the full schema survives
// the round-trip.
func toAPITools(tools []llm.Tool) []session.APITool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]session.APITool, 0, len(tools))
	for _, t := range tools {
		schema, err := json.Marshal(t.Parameters)
		if err != nil {
			continue
		}
		out = append(out, session.APITool{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  schema,
		})
	}
	return out
}
