package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	cmds "github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// InitPromptTemplate is the prompt sent to LLM to generate .tachi.md.
// Deprecated: use cmds.InitPromptTemplate instead.
var InitPromptTemplate = cmds.InitPromptTemplate

type Command struct {
	Name        string
	Description string
	handler     func(*Model) tea.Cmd
}

var commandHandlers = map[string]func(*Model) tea.Cmd{
	"new": func(m *Model) tea.Cmd {
		m.pendingQueue = nil
		m.chatview.RemovePendingItems()
		m.statusbar.SetPendingCount(0)
		m.history = nil
		m.chatview.Clear()
		m.agent.ClearSession()
		// Reset usage so statusbar shows clean state.
		m.totalUsage = llm.Usage{}
		m.statusbar.SetUsage(nil)
		m.statusbar.SetSessionInfo("", "")
		return nil
	},
	"quit": func(m *Model) tea.Cmd {
		return tea.Quit
	},
	"model": func(m *Model) tea.Cmd {
		cfg := m.cfg
		if cfg == nil {
			freshCfg, err := config.Load()
			if err != nil {
				cfgPath, _ := config.ConfigPath()
				m.chatview.AddMessage(chatMessage{
					Role:    "assistant",
					Content: fmt.Sprintf("No providers configured in %s", cfgPath),
				})
				return nil
			}
			cfg = freshCfg
			m.cfg = cfg
		}
		if len(cfg.Providers) == 0 {
			cfgPath, _ := config.ConfigPath()
			m.chatview.AddMessage(chatMessage{
				Role:    "assistant",
				Content: fmt.Sprintf("No providers configured in %s", cfgPath),
			})
			return nil
		}
		m.providerItems = cfg.Providers
		m.providerSelIdx = 0
		m.setState(stateSelectingModel)
		m.layout()
		return nil
	},
	"thinking": func(m *Model) tea.Cmd {
		return m.handleThinkingCommand()
	},
	"commit": func(m *Model) tea.Cmd {
		return m.sendCommitCommand()
	},
	"compact": func(m *Model) tea.Cmd {
		return m.handleCompactCommand()
	},
	"init": func(m *Model) tea.Cmd {
		return m.sendInitCommand()
	},
	"mcp": func(m *Model) tea.Cmd {
		return m.handleMCPCommand()
	},
	"sessions": func(m *Model) tea.Cmd {
		sm := m.agent.SessionManager()
		if sm == nil {
			m.chatview.AddMessage(chatMessage{
				Role:    "assistant",
				Content: "No session manager available",
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
				Content: "No sessions found",
			})
			return nil
		}
		m.sessionList = sessions
		m.sessionSelIdx = 0
		m.sessionScrollOff = 0
		// Pre-select the current session if it's in the list
		if curr := sm.Current(); curr != nil {
			if idx := slices.IndexFunc(sessions, func(s *session.Session) bool {
				return s.ID == curr.ID
			}); idx >= 0 {
				m.sessionSelIdx = idx
			}
		}
		// Ensure the pre-selected session is visible
		m.clampSessionScroll()
		m.setState(stateSelectingSession)
		m.layout()
		return nil
	},
	"usage": func(m *Model) tea.Cmd {
		return m.handleUsageCommand()
	},
	"review": func(m *Model) tea.Cmd {
		return m.sendReviewCommand()
	},
	"skill": func(m *Model) tea.Cmd {
		return m.handleSkillCommand()
	},
	"transcript": func(m *Model) tea.Cmd {
		return m.handleTranscriptCommand()
	},
	"dream": func(m *Model) tea.Cmd {
		return m.handleDreamCommandDispatch()
	},
	"research": func(m *Model) tea.Cmd {
		return m.handleResearchCommand()
	},
}

func matchCommands(prefix string) []Command {
	stripped := strings.TrimPrefix(prefix, "/")
	defs := cmds.MatchPrefixForMode(stripped, cmds.ModeTUI)
	var out []Command
	for _, d := range defs {
		if h, ok := commandHandlers[d.Name]; ok {
			out = append(out, Command{
				Name:        "/" + d.Name,
				Description: d.Description,
				handler:     h,
			})
		}
	}
	return out
}

func findCommand(name string) *Command {
	if !strings.HasPrefix(name, "/") {
		return nil
	}
	stripped := strings.TrimPrefix(name, "/")
	def := cmds.Find(stripped)
	if def == nil || !slices.Contains(def.Modes, cmds.ModeTUI) {
		return nil
	}
	h, ok := commandHandlers[stripped]
	if !ok {
		return nil
	}
	return &Command{
		Name:        "/" + def.Name,
		Description: def.Description,
		handler:     h,
	}
}

// findCommandByPrefix matches commands that are prefixes of the input
// (e.g., "/mcp" matches "/mcp list", "/mcp toggle foo").
// Exact matches are preferred; this is used as a fallback.
func findCommandByPrefix(input string) *Command {
	if !strings.HasPrefix(input, "/") {
		return nil
	}
	stripped := strings.TrimPrefix(input, "/")
	def := cmds.FindByPrefix(stripped)
	if def == nil || !slices.Contains(def.Modes, cmds.ModeTUI) {
		return nil
	}
	h, ok := commandHandlers[def.Name]
	if !ok {
		return nil
	}
	return &Command{
		Name:        "/" + def.Name,
		Description: def.Description,
		handler:     h,
	}
}

// mcpCommandTimeout is the timeout for MCP connect/reconnect operations
// triggered by slash commands.
const mcpCommandTimeout = 10 * time.Second
