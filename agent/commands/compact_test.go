package commands

import (
	"testing"

	"github.com/monsterxx03/tachi/llm"
	"github.com/stretchr/testify/assert"
)

func TestBuildCompactPrompt_SkipsSystemMessages(t *testing.T) {
	history := []llm.Message{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "帮我重构用户模块"},
		{Role: "assistant", Content: "好的，我来帮你重构用户模块。"},
		{Role: "user", Content: "把注册功能从 main.go 移到 auth.go"},
	}

	prompt := BuildCompactPrompt(history)
	assert.Contains(t, prompt, "帮我重构用户模块")
	assert.Contains(t, prompt, "把注册功能从 main.go 移到 auth.go")
	assert.NotContains(t, prompt, "You are a helpful assistant")
}

func TestBuildCompactPrompt_TruncatesLongToolResults(t *testing.T) {
	history := []llm.Message{
		{Role: "user", Content: "读取文件"},
		{Role: "assistant", Content: "让我读取"},
		{Role: "tool", Content: string(make([]byte, 1000))}, // long content
	}

	prompt := BuildCompactPrompt(history)
	assert.Contains(t, prompt, "读取文件")
	assert.Contains(t, prompt, "[Tool Result:") // Should still appear
	// Should be truncated to 500 + "..."
	assert.Contains(t, prompt, "...")
}

func TestBuildCompactPrompt_IncludesToolCalls(t *testing.T) {
	history := []llm.Message{
		{Role: "user", Content: "搜索一下"},
		{Role: "assistant", Content: "",
			ToolCalls: []llm.ToolCall{
				{
					ID:   "call_1",
					Type: "function",
					Function: llm.ToolCallFunction{
						Name:      "WebSearch",
						Arguments: `{"query":"golang error handling"}`,
					},
				},
			},
		},
	}

	prompt := BuildCompactPrompt(history)
	assert.Contains(t, prompt, "WebSearch")
	assert.Contains(t, prompt, "golang error handling")
}

func TestBuildCompactPrompt_EmptyHistory(t *testing.T) {
	prompt := BuildCompactPrompt(nil)
	assert.Contains(t, prompt, "compressed summary")
}

func TestBuildCompactPrompt_ThinkingBlocks(t *testing.T) {
	history := []llm.Message{
		{Role: "user", Content: "复杂问题"},
		{
			Role:    "assistant",
			Content: "最终答案",
			ThinkingBlocks: []llm.ThinkingBlock{
				{Type: "thinking", Thinking: "让我思考一下这个问题..."},
			},
		},
	}

	prompt := BuildCompactPrompt(history)
	assert.Contains(t, prompt, "最终答案")
	assert.Contains(t, prompt, "让我思考一下")
}

func TestBuildCompactInstruction_Structure(t *testing.T) {
	instruction := BuildCompactInstruction()
	assert.Contains(t, instruction, "compressed summary")
	assert.Contains(t, instruction, "Key actions")
	assert.Contains(t, instruction, "Do NOT call any tools")
	// Verify it does NOT embed actual conversation content:
	// the old BuildCompactPrompt injects formatted markers like [Tool Call: xxx],
	// but BuildCompactInstruction only has the summarization instructions.
	assert.NotContains(t, instruction, "[Tool Call:")
	assert.NotContains(t, instruction, "[Tool Result:")
}

func TestBuildCompactInstruction_NoHistoryEmbedding(t *testing.T) {
	history := []llm.Message{
		{Role: "user", Content: "帮我重构用户模块"},
		{Role: "assistant", Content: "好的，我来帮你重构用户模块。"},
	}

	prompt := BuildCompactPrompt(history)
	instruction := BuildCompactInstruction()

	// Old prompt embeds history; new instruction does not.
	assert.Contains(t, prompt, "帮我重构用户模块")
	assert.NotContains(t, instruction, "帮我重构用户模块")
}
