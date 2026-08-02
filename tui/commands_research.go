package tui

import (
	"context"
	"fmt"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"

	cmds "github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/session"
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

		// Register the report as a session artifact (best-effort; only when
		// the file exists). A missing session (fresh window) must not drop
		// the reminder — the reminder is always stashed and spliced into
		// m.history by researchDoneMsg, so the current window can follow up
		// even without a session.
		var reminder string
		if p := engine.ReportPath(); p != "" {
			if _, statErr := os.Stat(p); statErr == nil {
				ref := session.ArtifactRef{
					Kind:  session.ArtifactKindResearch,
					Title: parsed.Topic,
					Path:  p,
				}
				if sm := m.agent.SessionManager(); sm != nil {
					if err := sm.AppendArtifact(ref); err != nil {
						m.logger.Warn(context.Background(), "TUI: research artifact: append failed", "err", err)
					}
				}
				reminder = session.FormatArtifactReminder([]session.ArtifactRef{ref})
			} else {
				m.logger.Warn(context.Background(), "TUI: research report missing on disk", "path", p, "err", statErr)
			}
		}
		m.researchReminder = reminder // happens-before: ch send below

		ch <- fmt.Sprintf("✅ **研究完成**\n\n---\n\n%s", report)
	}()

	return readNextResearchStatus(ch)
}
