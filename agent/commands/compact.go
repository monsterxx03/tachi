package commands

import (
	"fmt"
	"strings"

	"github.com/monsterxx03/tachi/llm"
)

// BuildCompactInstruction builds the instruction portion of the compact prompt.
// Unlike BuildCompactPrompt, it does NOT embed conversation history as text —
// the history is expected to be passed as structured conversation context
// (e.g. via RunConversationStream with the session's prior messages).
// System messages are skipped in the instruction since they are already
// transmitted as structured context.
//
// language controls the output language of the instruction:
//   - "Chinese" (default): returns a Chinese instruction
//   - "English": returns an English instruction
//   - any other value falls back to Chinese
func BuildCompactInstruction(language string) string {
	if strings.EqualFold(language, "English") {
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

	return "请将以上对话历史压缩成一份简洁但全面的综合摘要。\n\n" +
		"摘要必须包含：\n" +
		"1. 用户的主要目标和问题\n" +
		"2. 已完成的关键操作和修改（具体文件名、命令、配置变更）\n" +
		"3. 重要发现和结论\n" +
		"4. 当前状态和待解决问题\n" +
		"5. 工作环境（目录、分支等）\n\n" +
		"约束：\n" +
		"- 保持信息密度，删除重复和无关内容\n" +
		"- 保留具体的文件名、命令、配置值等关键细节\n" +
		"- 不要添加新的分析或建议——只总结已经发生的事情\n" +
		"- 不要调用任何工具，直接输出摘要文本\n\n" +
		"请输出压缩摘要："
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
	sb.WriteString("你是一个对话压缩助手。请将以下对话历史压缩成一份简洁但全面的综合摘要。\n\n")
	sb.WriteString("摘要必须包含：\n")
	sb.WriteString("1. 用户的主要目标和问题\n")
	sb.WriteString("2. 已完成的关键操作和修改（具体文件名、命令、配置变更）\n")
	sb.WriteString("3. 重要发现和结论\n")
	sb.WriteString("4. 当前状态和待解决问题\n")
	sb.WriteString("5. 工作环境（目录、分支等）\n\n")
	sb.WriteString("约束：\n")
	sb.WriteString("- 保持信息密度，删除重复和无关内容\n")
	sb.WriteString("- 保留具体的文件名、命令、配置值等关键细节\n")
	sb.WriteString("- 不要添加新的分析或建议——只总结已经发生的事情\n\n")
	sb.WriteString("---对话历史---\n\n")

	const maxToolResultLen = 500

	for _, msg := range history {
		if msg.Role == "system" {
			continue
		}

		switch msg.Role {
		case "user":
			sb.WriteString(fmt.Sprintf("用户: %s\n", truncateContent(msg.Content, 1000)))
		case "assistant":
			content := msg.Content
			if content != "" {
				sb.WriteString(fmt.Sprintf("助手: %s\n", truncateContent(content, 2000)))
			}
			if len(msg.ThinkingBlocks) > 0 {
				for _, tb := range msg.ThinkingBlocks {
					sb.WriteString(fmt.Sprintf("[思考: %s]\n", truncateContent(tb.Thinking, 500)))
				}
			}
			for _, tc := range msg.ToolCalls {
				sb.WriteString(fmt.Sprintf("[工具调用: %s(%s)]\n", tc.Function.Name, truncateContent(tc.Function.Arguments, 200)))
			}
		case "tool":
			content := msg.Content
			runes := []rune(content)
			if len(runes) > maxToolResultLen {
				content = string(runes[:maxToolResultLen]) + "..."
			}
			sb.WriteString(fmt.Sprintf("[工具结果: %s]\n", content))
		default:
			content := msg.Content
			runes := []rune(content)
			if len(runes) > 500 {
				content = string(runes[:500]) + "..."
			}
			sb.WriteString(fmt.Sprintf("[%s]: %s\n", msg.Role, content))
		}
	}

	sb.WriteString("\n---\n请输出压缩摘要：")
	return sb.String()
}

// truncateContent truncates s to at most maxLen characters (runes).
// A "..." suffix is appended when truncation occurs.
func truncateContent(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}
