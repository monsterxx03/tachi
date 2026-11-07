package tui

import (
	"context"
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/monsterxx03/tachi/pkg/strutil"
)

// handleTrimCommand implements /trim: it replaces this session's old tool outputs with
// a one-line note (agent.TrimToolResults) — no model call, no new session, the same
// conversation with less of it.
//
// It runs synchronously: the work is one pass over a file the TUI already reads on
// every session switch, and a spinner over it would cost more than the work does.
//
// The transcript does NOT re-render: its cards were drawn from the live events, and
// the TUI keeps them as they were. What changed is the records — the next request and
// the next reload of this session see the trimmed history, which m.history is set to
// so this turn's own continuation sends it too.
func (m *Model) handleTrimCommand() tea.Cmd {
	if m.agent == nil {
		m.chatview.AddMessage(chatMessage{Role: "error", Content: "agent 不可用"})
		return nil
	}
	m.chatview.AddMessage(chatMessage{Role: "user", Content: "/trim"})

	res, err := m.agent.TrimToolResults(context.Background())
	if err != nil {
		m.chatview.AddMessage(chatMessage{Role: "error", Content: "裁剪失败: " + err.Error()})
		return nil
	}
	if res.Blocked != "" {
		m.chatview.AddMessage(chatMessage{Role: "error", Content: res.Blocked})
		return nil
	}
	if res.History != nil {
		m.history = res.History
	}
	// The statusbar's context figure is only ever updated from here or from a usage
	// event; the trim recomputed the estimate, so this is the push that makes it
	// visible (the same update the usage handler does).
	if est := m.agent.LastInputEstimate(); est > 0 {
		m.totalUsage.LastInputTokens = est
	}

	if res.Trimmed == 0 {
		m.chatview.AddMessage(chatMessage{
			Role: "assistant",
			Content: "没有可裁剪的东西：只有较早、且较大的结果才会被裁掉；" +
				"图片另有按体积的预算，只保留较新的几张" +
				"（AskUserQuestion 的选择和子任务结论始终保留）。",
		})
		return nil
	}
	m.chatview.AddMessage(chatMessage{
		Role: "assistant",
		Content: fmt.Sprintf("✂️ 已裁剪 %d 条工具输出，释放约 %s。原文保留在会话目录的 %s 里；第一次请求会重读一遍剩余历史（前缀缓存断开），之后就小了。",
			res.Trimmed, trimFreed(res.CharsSaved, res.BytesSaved), res.Sidecar),
	})
	return nil
}

// trimFreed says what a trim freed in the units it was freed in. Images and text are
// reported apart on purpose: "释放约 121 字符" after removing a screenshot is the
// sentence that made an oversized request look inexplicable.
func trimFreed(chars, imageBytes int64) string {
	switch {
	case imageBytes > 0 && chars > 0:
		return fmt.Sprintf("%s 图片 + %d 字符", strutil.HumanBytes(imageBytes), chars)
	case imageBytes > 0:
		return strutil.HumanBytes(imageBytes) + " 图片"
	default:
		return fmt.Sprintf("%d 字符", chars)
	}
}
