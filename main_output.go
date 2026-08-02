package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/llm"
)

func runOutputLoop(aiAgent *agent.AIAgent, ch <-chan agent.AgentEvent, fmt outputFormat, quiet bool) *agent.RunResult {
	switch fmt {
	case outputJSON:
		return runOutputJSON(ch)
	case outputJSONStream:
		return runOutputJSONStream(aiAgent, ch)
	default:
		return runOutputText(aiAgent, ch, quiet)
	}
}

// runOutputText streams text delta events to stdout and progress to stderr.
func runOutputText(aiAgent *agent.AIAgent, ch <-chan agent.AgentEvent, quiet bool) *agent.RunResult {
	var result *agent.RunResult
	for event := range ch {
		switch event.Type {
		case agent.AgentEventTextDelta:
			fmt.Fprint(os.Stdout, event.TextDelta)
			_ = os.Stdout.Sync()

		case agent.AgentEventThinkingDelta:
			if !quiet {
				fmt.Fprint(os.Stderr, event.ThinkingDelta)
			}

		case agent.AgentEventToolCallStart:
			if !quiet {
				fmt.Fprintf(os.Stderr, "\n🔧 %s(", event.ToolName)
			}

		case agent.AgentEventToolCallArgs:
			if !quiet {
				trunc := event.ToolArgs
				if len(trunc) > 60 {
					trunc = trunc[:60] + "..."
				}
				fmt.Fprintf(os.Stderr, "%s)\n", trunc)
			}

		case agent.AgentEventToolResult:
			if !quiet {
				icon := "✅"
				if event.ToolIsError {
					icon = "❌"
				}
				fmt.Fprintf(os.Stderr, " %s (%v)\n", icon, event.ToolDuration.Round(time.Millisecond))
			}

		case agent.AgentEventTurnComplete:
			result = event.Result

		case agent.AgentEventError:
			result = event.Result

		case agent.AgentEventToolConfirmation:
			aiAgent.ConfirmTool(agent.ConfirmAllowOnce)
		}
	}
	return result
}

// streamEvent is a single NDJSON event in json-stream output mode.
type streamEvent struct {
	Type       string     `json:"type"`
	Content    string     `json:"content,omitempty"`
	ToolName   string     `json:"tool_name,omitempty"`
	ToolArgs   string     `json:"tool_args,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolResult string     `json:"tool_result,omitempty"`
	DurationMS int64      `json:"duration_ms,omitempty"`
	IsError    bool       `json:"is_error,omitempty"`
	ExitReason string     `json:"exit_reason,omitempty"`
	Iterations int        `json:"iterations_used,omitempty"`
	Usage      *usageJSON `json:"usage,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// usageJSON is the serializable representation of token usage.
type usageJSON struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

func usageToJSON(u *llm.Usage) *usageJSON {
	if u == nil {
		return nil
	}
	return &usageJSON{
		InputTokens:              u.InputTokens,
		OutputTokens:             u.OutputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens,
		CacheReadInputTokens:     u.CacheReadInputTokens,
	}
}

// runOutputJSON collects all events and outputs a single JSON object to stdout.
func runOutputJSON(ch <-chan agent.AgentEvent) *agent.RunResult {
	var result *agent.RunResult
	for event := range ch {
		switch event.Type {
		case agent.AgentEventTurnComplete:
			result = event.Result
		case agent.AgentEventError:
			result = event.Result
		case agent.AgentEventToolConfirmation:
		}
	}

	if result != nil {
		out := struct {
			ExitReason     string     `json:"exit_reason"`
			IterationsUsed int        `json:"iterations_used"`
			Response       string     `json:"response"`
			Usage          *usageJSON `json:"usage,omitempty"`
			Error          string     `json:"error,omitempty"`
		}{
			ExitReason:     result.ExitReason,
			IterationsUsed: result.IterationsUsed,
			Response:       result.Response,
			Usage:          usageToJSON(result.Usage),
		}
		if result.Error != nil {
			out.Error = result.Error.Error()
		}
		json.NewEncoder(os.Stdout).Encode(out)
	}
	return result
}

// runOutputJSONStream emits one JSON line per agent event to stdout.
func runOutputJSONStream(aiAgent *agent.AIAgent, ch <-chan agent.AgentEvent) *agent.RunResult {
	enc := json.NewEncoder(os.Stdout)
	var result *agent.RunResult

	for event := range ch {
		switch event.Type {
		case agent.AgentEventTextDelta:
			enc.Encode(streamEvent{Type: "text_delta", Content: event.TextDelta})

		case agent.AgentEventThinkingDelta:
			enc.Encode(streamEvent{Type: "thinking_delta", Content: event.ThinkingDelta})

		case agent.AgentEventToolCallStart:
			// Wait for args which come in the next event.
		case agent.AgentEventToolCallArgs:
			enc.Encode(streamEvent{
				Type:       "tool_call",
				ToolName:   event.ToolName,
				ToolArgs:   event.ToolArgs,
				ToolCallID: event.ToolID,
			})

		case agent.AgentEventToolResult:
			enc.Encode(streamEvent{
				Type:       "tool_result",
				ToolName:   event.ToolName,
				ToolResult: event.ToolResult,
				DurationMS: event.ToolDuration.Milliseconds(),
				IsError:    event.ToolIsError,
			})

		case agent.AgentEventTurnComplete:
			result = event.Result
			enc.Encode(streamEvent{
				Type:       "turn_complete",
				ExitReason: result.ExitReason,
				Iterations: result.IterationsUsed,
				Usage:      usageToJSON(result.Usage),
			})

		case agent.AgentEventError:
			result = event.Result
			errMsg := ""
			if result.Error != nil {
				errMsg = result.Error.Error()
			}
			enc.Encode(streamEvent{Type: "error", Error: errMsg})

		case agent.AgentEventToolConfirmation:
			aiAgent.ConfirmTool(agent.ConfirmAllowOnce)
		}
	}
	return result
}

// runChannels starts all channels declared in config.
//
// Channels are discovered via the registry (channel.Register). Each entry in
// cfg.Channel.ActiveChannels() is matched to a registered factory by name.
// For backward compatibility, the legacy cfg.Channel.Weixin.Enabled flag
// is converted by ActiveChannels() into a "weixin" entry if not already
// present in the new-style channels map.
//
// To add private channels, create a file like:
//
//	package main
//	import _ "private-repo/tachi-channel-mybots"
//
// and configure them in config.yaml:
//
//	channel:
//	  channels:
//	    mybots:
//	      enabled: true
//	      token: "xxx"
