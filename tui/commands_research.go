package tui

import (
	"context"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"

	cmds "github.com/monsterxx03/tachi/agent/commands"
)

func (m *Model) handleResearchCommand() tea.Cmd {
	parsed := cmds.ParseResearchArgs(m.subcommandInput)
	if parsed.Topic == "" {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "Usage: `/research <topic> [--depth N] [--breadth N]`",
		})
		return nil
	}

	cfg := m.cfg
	if cfg == nil {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "No configuration found. Run `tachi init` first.",
		})
		return nil
	}

	if parsed.Depth <= 0 {
		parsed.Depth = cfg.DeepResearch.DefaultDepth
	}
	if parsed.Breadth <= 0 {
		parsed.Breadth = cfg.DeepResearch.DefaultBreadth
	}

	engine, err := m.agent.NewDeepResearch(cfg)
	if err != nil {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("Failed to create research engine: %v", err),
		})
		return nil
	}
	if engine == nil {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "Deep Research is not available (engine creation returned nil).",
		})
		return nil
	}

	m.chatview.AddMessage(chatMessage{
		Role:    "user",
		Content: fmt.Sprintf("/research %s", parsed.Topic),
	})
	m.chatview.AddMessage(chatMessage{
		Role: "assistant",
		Content: fmt.Sprintf("🔬 **深度研究已启动**\n\n**主题**: %s\n**深度**: %d | **广度**: %d\n\n正在生成搜索查询、并行搜索、提取信息... 这可能需要几分钟，请稍候。\n\n进度消息会实时显示在此处。",
			parsed.Topic, parsed.Depth, parsed.Breadth),
	})

	ch := make(chan string, 100)
	researchCtx, researchCancel := context.WithTimeout(context.Background(), cfg.DeepResearch.Timeout+time.Minute)

	m.isResearching = true
	m.cancelFunc = researchCancel

	go func() {
		defer researchCancel()
		defer close(ch)

		report, runErr := engine.Run(researchCtx, parsed.Topic, parsed.Depth, parsed.Breadth, func(format string, args ...any) {
			msg := fmt.Sprintf(format, args...)
			select {
			case ch <- msg:
			default:
			}
		})
		if runErr != nil {
			ch <- fmt.Sprintf("❌ **研究失败**: %v", runErr)
			return
		}

		ch <- fmt.Sprintf("✅ **研究完成**\n\n---\n\n%s", report)
	}()

	return readNextResearchStatus(ch)
}
