package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/monsterxx03/tachi/dream"
	"github.com/monsterxx03/tachi/session"
)

func (m *Model) handleDreamCommandDispatch() tea.Cmd {
	parts := strings.Fields(m.subcommandInput)
	sub := ""
	if len(parts) > 1 {
		sub = parts[1]
	}

	switch sub {
	case "status":
		return m.handleDreamStatusCommand()
	default:
		// /dream or /dream run — trigger dream.
		return m.handleDreamCommand()
	}
}

// handleDreamStatusCommand shows the current dream orchestrator status.
func (m *Model) handleDreamStatusCommand() tea.Cmd {
	if m.dreamOrch == nil {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "🧠 **当前没有正在运行的 AutoDream**\n\n使用 `/dream` 触发新的记忆整合。",
		})
		return nil
	}

	status := m.dreamOrch.Status()
	if status.Running == 0 {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "🧠 **AutoDream 空闲中**\n\n没有正在运行的 domain，可能是等待 goroutine 启动或已结束。",
		})
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "🧠 **AutoDream 状态** — %d 个 domain 正在处理：\n\n", status.Running)

	for i, d := range status.Domains {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "**%s**", d.Domain)
		if d.Root != "" {
			fmt.Fprintf(&b, " — `%s`", d.Root)
		}
		b.WriteString("\n")

		runningSince := time.Since(d.StartedAt).Round(time.Second)
		fmt.Fprintf(&b, "- 状态：运行中（已进行 %v）\n", runningSince)
		fmt.Fprintf(&b, "- 处理中：%d 个 session\n", d.ActiveCount)

		last := d.LastState
		if !last.LastDreamAt.IsZero() {
			lastDreamAgo := time.Since(last.LastDreamAt).Round(time.Minute)
			fmt.Fprintf(&b, "- 上次完成：%v 前\n", lastDreamAgo)
			fmt.Fprintf(&b, "- 上次结果：%d sessions, %d facts, %d superseded, %d pruned\n",
				last.SessionsDreamed, last.FactsAdded, last.FactsSuperseded, last.FactsPruned)
		} else {
			b.WriteString("- 上次完成：首次运行\n")
		}
	}

	m.chatview.AddMessage(chatMessage{
		Role:    "assistant",
		Content: b.String(),
	})
	return nil
}

// handleDreamCommand triggers AutoDream memory consolidation synchronously
// (not via SystemScheduler). It lists all sessions, runs the dream
// orchestrator, and streams progress/results back to the chat view asynchronously.
func (m *Model) handleDreamCommand() tea.Cmd {
	sm := m.agent.SessionManager()
	if sm == nil {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "No session manager available — start a conversation first",
		})
		return nil
	}

	sessions, err := sm.List()
	if err != nil {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("Failed to list sessions: %v", err),
		})
		return nil
	}

	if len(sessions) == 0 {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "No sessions found — nothing to consolidate yet",
		})
		return nil
	}

	provider := m.agent.Provider()
	if provider == nil {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "No LLM provider configured — cannot run dream",
		})
		return nil
	}

	// Check if dream is already running.
	if m.dreamOrch != nil {
		if s := m.dreamOrch.Status(); s.Running > 0 {
			m.chatview.AddMessage(chatMessage{
				Role:    "assistant",
				Content: "🧠 **AutoDream 正在运行中**\n\n请等待当前 dream 完成后再触发，或使用 `/dream status` 查看进度。",
			})
			return nil
		}
	}

	m.chatview.AddMessage(chatMessage{
		Role:    "assistant",
		Content: fmt.Sprintf("🧠 **AutoDream 已触发** — 正在整合 %d 个 session 的记忆...\n\n使用 `/dream status` 查看实时进度。", len(sessions)),
	})

	ch := make(chan string, 5) // buffer 5 for status + sentinel
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)

	// Store orchestrator reference before goroutine starts so /dream status can query it.
	m.dreamOrch = dream.NewOrchestrator(dream.Config{
		Logger:        m.logger,
		MaxConcurrent: m.cfg.Dream.MaxConcurrent,
	})
	o := m.dreamOrch // local reference for goroutine

	go func() {
		defer cancel()

		cfg := m.cfg
		var dreamProvider string
		maxIter := 30
		maxMessageChars := 2000
		if cfg != nil {
			dreamProvider = cfg.Dream.Provider
			if cfg.Dream.SubagentMaxIter > 0 {
				maxIter = cfg.Dream.SubagentMaxIter
			}
			if cfg.Dream.MaxMessageChars > 0 {
				maxMessageChars = cfg.Dream.MaxMessageChars
			}
		}

		runFn := func(ctx context.Context, plan dream.Plan) (dream.State, error) {
			// Use a fresh session manager so Load(id) doesn't mutate
			// the TUI's current-session pointer.
			dreamSM, smErr := session.NewManager(nil)
			loadMessages := func(id string) ([]session.Message, error) {
				if smErr != nil {
					return nil, smErr
				}
				if _, err := dreamSM.Load(id); err != nil {
					return nil, err
				}
				return dreamSM.LoadMessages()
			}

			return dream.RunDream(ctx, plan, dream.RunConfig{
				FallbackProvider: provider,
				DreamProvider:    dreamProvider,
				Config:           cfg,
				MaxIter:          maxIter,
				MaxTokens:        m.chatOpts.MaxTokens,
				MaxMessageChars:  maxMessageChars,
				Logger:           m.logger,
			}, loadMessages)
		}

		if err := o.Run(ctx, sessions, runFn); err != nil {
			ch <- fmt.Sprintf("🧠 **Dream 失败**: %v", err)
		} else {
			ch <- "🧠 **Dream 完成** — 记忆已整合"
		}

		// Signal completion so the TUI can clean up the orchestrator reference.
		ch <- dreamDoneSentinel
		close(ch)
	}()

	return readNextDreamStatus(ch)
}

// dreamDoneSentinel is a sentinel message sent through the dream status channel
// to signal that the orchestrator has completed and should be cleaned up.
// It contains a null byte which cannot appear in normal status messages.
const dreamDoneSentinel = "\x00"

// readNextDreamStatus reads the next message from the channel and returns a
// dreamStatusMsg. If the channel is closed, returns nil.
func readNextDreamStatus(ch <-chan string) tea.Cmd {
	return func() tea.Msg {
		content, ok := <-ch
		if !ok {
			return nil
		}
		return dreamStatusMsg{content: content, nextCh: ch}
	}
}

// readNextResearchStatus reads the next message from the channel and returns a
// researchStatusMsg. When the channel is closed (research complete), returns
// researchDoneMsg so the model can reset isResearching and allow input.
func readNextResearchStatus(ch <-chan string) tea.Cmd {
	return func() tea.Msg {
		content, ok := <-ch
		if !ok {
			return researchDoneMsg{}
		}
		return researchStatusMsg{content: content, nextCh: ch}
	}
}

// handleResearchCommand handles /research <topic> [--depth N] [--breadth N]
