package commands

import (
	"fmt"
	"strings"

	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

// BuildCompactInstruction builds the instruction portion of the compact prompt.
// Unlike BuildCompactPrompt, it does NOT embed conversation history as text —
// the history is expected to be passed as structured conversation context
// (e.g. via RunConversationStream with the session's prior messages).
// System messages are skipped in the instruction since they are already
// transmitted as structured context.
func BuildCompactInstruction() string {
	return "Please compress the above conversation into a concise yet comprehensive summary.\n\n" +
		"The summary MUST include:\n" +
		"1. The user's main goals and questions\n" +
		"2. Key actions and modifications completed (specific file names, commands, config changes)\n" +
		"3. Important findings and conclusions\n" +
		"4. Current state and remaining issues\n" +
		"5. Working environment (directory, branch, etc.)\n\n" +
		"Constraints:\n" +
		"- Maintain information density — remove repetition and irrelevant content\n" +
		"- Preserve specific file names, commands, config values and other critical details\n" +
		"- Do NOT add new analysis or suggestions — only summarize what happened\n" +
		"- Do NOT call any tools — output the summary text directly\n\n" +
		"Output the compressed summary:"
}

// BuildCompactPrompt builds the prompt asking the LLM to produce a conversation summary.
// history is the in-memory conversation. System messages are skipped.
// Tool result content exceeding 500 chars is truncated in the prompt.
//
// Deprecated: prefer BuildCompactInstruction() and pass history as structured
// context via RunOneOffStream's history parameter. This function embeds history
// as text in the prompt, which wastes tokens compared to structured context.
func BuildCompactPrompt(history []llm.Message) string {
	var sb strings.Builder
	sb.WriteString("You are a conversation compression assistant. Please compress the following conversation history into a concise yet comprehensive summary.\n\n")
	sb.WriteString("The summary MUST include:\n")
	sb.WriteString("1. The user's main goals and questions\n")
	sb.WriteString("2. Key actions and modifications completed (specific file names, commands, config changes)\n")
	sb.WriteString("3. Important findings and conclusions\n")
	sb.WriteString("4. Current state and remaining issues\n")
	sb.WriteString("5. Working environment (directory, branch, etc.)\n\n")
	sb.WriteString("Constraints:\n")
	sb.WriteString("- Maintain information density — remove repetition and irrelevant content\n")
	sb.WriteString("- Preserve specific file names, commands, config values and other critical details\n")
	sb.WriteString("- Do NOT add new analysis or suggestions — only summarize what happened\n\n")
	sb.WriteString("---Conversation History---\n\n")

	const maxToolResultLen = 500

	for _, msg := range history {
		if msg.Role == "system" {
			continue
		}

		switch msg.Role {
		case "user":
			fmt.Fprintf(&sb, "User: %s\n", strutil.Truncate(msg.Content, 1000))
		case "assistant":
			content := msg.Content
			if content != "" {
				fmt.Fprintf(&sb, "Assistant: %s\n", strutil.Truncate(content, 2000))
			}
			if len(msg.ThinkingBlocks) > 0 {
				for _, tb := range msg.ThinkingBlocks {
					fmt.Fprintf(&sb, "[Thinking: %s]\n", strutil.Truncate(tb.Thinking, 500))
				}
			}
			for _, tc := range msg.ToolCalls {
				fmt.Fprintf(&sb, "[Tool Call: %s(%s)]\n", tc.Function.Name, strutil.Truncate(tc.Function.Arguments, 200))
			}
		case "tool":
			content := strutil.Truncate(msg.Content, maxToolResultLen)
			fmt.Fprintf(&sb, "[Tool Result: %s]\n", content)
		default:
			content := strutil.Truncate(msg.Content, 500)
			fmt.Fprintf(&sb, "[%s]: %s\n", msg.Role, content)
		}
	}

	sb.WriteString("\n---\nOutput the compressed summary:")
	return sb.String()
}
