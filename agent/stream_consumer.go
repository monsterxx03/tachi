package agent

import (
	"fmt"
	"strings"

	"github.com/monsterxx03/tachi/llm"
)

// streamAccumulator collects streaming deltas into a complete response.
type streamAccumulator struct {
	text         strings.Builder
	thinking     strings.Builder
	signature    strings.Builder
	toolCalls    []llm.ToolCall
	toolArgs     []strings.Builder
	toolIndexMap map[int]int // OpenAI tool index -> toolArgs slice index
	thinkBlocks  []llm.ThinkingBlock
	finishReason string
	usage        *llm.Usage
}

// finalize resolves all accumulated deltas into the finished structures.
func (acc *streamAccumulator) finalize() {
	for i := range acc.toolCalls {
		if i < len(acc.toolArgs) {
			acc.toolCalls[i].Function.Arguments = acc.toolArgs[i].String()
		}
	}
	if acc.thinking.Len() > 0 {
		acc.thinkBlocks = append(acc.thinkBlocks, llm.ThinkingBlock{
			Type:      "thinking",
			Thinking:  acc.thinking.String(),
			Signature: acc.signature.String(),
		})
	}
}

// assistantMessage constructs an llm.Message from the accumulated content.
func (acc *streamAccumulator) assistantMessage() llm.Message {
	return llm.Message{
		Role:           "assistant",
		Content:        acc.text.String(),
		ThinkingBlocks: acc.thinkBlocks,
		ToolCalls:      acc.toolCalls,
	}
}

// consumeStream reads all events from the LLM stream, forwards deltas to the
// event channel, and returns the accumulated result.
func consumeStream(streamCh <-chan llm.StreamEvent, ch chan<- AgentEvent, apiCallCount int) (*streamAccumulator, error) {
	acc := &streamAccumulator{
		toolIndexMap: make(map[int]int),
	}

	for event := range streamCh {
		switch event.Type {
		case llm.StreamEventTextDelta:
			acc.text.WriteString(event.TextDelta)
			ch <- AgentEvent{Type: AgentEventTextDelta, TextDelta: event.TextDelta}

		case llm.StreamEventThinkingDelta:
			acc.thinking.WriteString(event.ThinkingDelta)
			ch <- AgentEvent{Type: AgentEventThinkingDelta, ThinkingDelta: event.ThinkingDelta}

		case llm.StreamEventSignatureDelta:
			acc.signature.WriteString(event.SignatureDelta)

		case llm.StreamEventToolUseStart:
			if event.ToolCall != nil {
				sliceIdx := len(acc.toolCalls)
				acc.toolIndexMap[event.ToolIndex] = sliceIdx
				acc.toolCalls = append(acc.toolCalls, *event.ToolCall)
				acc.toolArgs = append(acc.toolArgs, strings.Builder{})
				ch <- AgentEvent{
					Type:     AgentEventToolCallStart,
					ToolName: event.ToolCall.Function.Name,
					ToolID:   event.ToolCall.ID,
				}
			}

		case llm.StreamEventInputJSONDelta:
			if idx, ok := acc.toolIndexMap[event.ToolIndex]; ok && idx < len(acc.toolArgs) {
				acc.toolArgs[idx].WriteString(event.InputDelta)
			} else if len(acc.toolArgs) > 0 {
				acc.toolArgs[len(acc.toolArgs)-1].WriteString(event.InputDelta)
			}

		case llm.StreamEventMessageDelta, llm.StreamEventDone:
			acc.finishReason = event.FinishReason
			if event.Usage != nil {
				acc.usage = event.Usage
			}

		case llm.StreamEventError:
			return nil, fmt.Errorf("stream error (iteration %d): %w", apiCallCount, event.Error)
		}
	}

	acc.finalize()
	return acc, nil
}
