package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/debuglog"
)

// InitPromptTemplate is the prompt sent to LLM to generate .tachi.md
const InitPromptTemplate = `Please analyze this codebase and create a .tachi.md file, which will be given to future instances of tachi to operate in this repository.

What to add:
1. Commands that will be commonly used, such as how to build, lint, and run tests. Include the necessary commands to develop in this codebase, such as how to run a single test.
2. High-level code architecture and structure so that future instances can be productive more quickly. Focus on the "big picture" architecture that requires reading multiple files to understand.

Usage notes:
- If there's already a .tachi.md, suggest improvements to it.
- When you make the initial .tachi.md, do not repeat yourself and do not include obvious instructions like "Provide helpful error messages to users", "Write unit tests for all new utilities", "Never include sensitive information (API keys, tokens) in code or commits".
- Avoid listing every component or file structure that can be easily discovered.
- Don't include generic development practices.
- If there are Cursor rules (in .cursor/rules/ or .cursorrules) or Copilot rules (in .github/copilot-instructions.md), make sure to include the important parts.
- If there is a README.md, make sure to include the important parts.
- Do not make up information such as "Common Development Tasks", "Tips for Development", "Support and Documentation" unless this is expressly included in other files that you read.

## Context to gather

Run these commands to understand the codebase:
- "git status" - Current git status
- "git branch --show-current" - Current branch
- "git log --oneline -5" - Recent commits
- "find . -name 'Makefile' -o -name 'go.mod' -o -name 'package.json' -o -name 'README.md' -o -name '.cursorrules' -o -name 'CLAUDE.md' 2>/dev/null | head -20" - Find key project files
- "ls -la" - List root directory contents

## Your task

Analyze the codebase structure, read key files (README.md, Makefile, go.mod, package.json, etc.), and create a comprehensive .tachi.md file at the root of the repository. Use the WriteFile tool to create the file.

If .tachi.md already exists, read it first and suggest improvements.

The file should be concise but informative - focus on practical information needed to work effectively in this codebase.`

type Command struct {
	Name        string
	Description string
	handler     func(*Model) tea.Cmd
}

var commands = []Command{
	{
		Name:        "/clear",
		Description: "Clear conversation history",
		handler: func(m *Model) tea.Cmd {
			m.history = nil
			m.chatview.Clear()
			m.agent.ClearSession()
			return nil
		},
	},
	{
		Name:        "/quit",
		Description: "Exit tachi",
		handler:     func(m *Model) tea.Cmd { return tea.Quit },
	},
	{
		Name:        "/model",
		Description: "Switch provider/model",
		handler: func(m *Model) tea.Cmd {
			cfg := m.cfg
			if cfg == nil {
				freshCfg, err := config.Load()
				if err != nil {
					m.chatview.AddMessage(chatMessage{
						Role:    "assistant",
						Content: "No providers configured in ~/.tachi/config.yaml",
					})
					return nil
				}
				cfg = freshCfg
				m.cfg = cfg
			}
			if len(cfg.Providers) == 0 {
				m.chatview.AddMessage(chatMessage{
					Role:    "assistant",
					Content: "No providers configured in ~/.tachi/config.yaml",
				})
				return nil
			}
			m.providerItems = cfg.Providers
			m.providerSelIdx = 0
			m.setState(stateSelectingModel)
			m.layout()
			return nil
		},
	},
	{
		Name:        "/commit",
		Description: "Ask LLM to write commit message and commit via Bash (git)",
		handler: func(m *Model) tea.Cmd {
			return m.sendCommitCommand()
		},
	},
	{
		Name:        "/init",
		Description: "Generate .tachi.md project context file via LLM",
		handler: func(m *Model) tea.Cmd {
			return m.sendInitCommand()
		},
	},
	{
		Name:        "/mcp",
		Description: "Manage MCP servers (list, toggle, reconnect)",
		handler: func(m *Model) tea.Cmd {
			return m.handleMCPCommand()
		},
	},
}

func matchCommands(prefix string) []Command {
	if prefix == "/" {
		out := make([]Command, len(commands))
		copy(out, commands)
		return out
	}
	var out []Command
	for _, cmd := range commands {
		if strings.HasPrefix(cmd.Name, prefix) {
			out = append(out, cmd)
		}
	}
	return out
}

func findCommand(name string) *Command {
	for i := range commands {
		if commands[i].Name == name {
			return &commands[i]
		}
	}
	return nil
}

// findCommandByPrefix matches commands that are prefixes of the input
// (e.g., "/mcp" matches "/mcp list", "/mcp toggle foo").
// Exact matches are preferred; this is used as a fallback.
func findCommandByPrefix(input string) *Command {
	for i := range commands {
		if input == commands[i].Name || strings.HasPrefix(input, commands[i].Name+" ") {
			return &commands[i]
		}
	}
	return nil
}

// mcpCommandTimeout is the timeout for MCP connect/reconnect operations
// triggered by slash commands.
const mcpCommandTimeout = 10 * time.Second

// handleMCPCommand dispatches to the appropriate subcommand handler based on
// the raw input stored in m.subcommandInput.
func (m *Model) handleMCPCommand() tea.Cmd {
	parts := strings.Fields(m.subcommandInput)
	sub := ""
	if len(parts) > 1 {
		sub = parts[1]
	}
	arg := ""
	if len(parts) > 2 {
		arg = parts[2]
	}

	switch sub {
	case "toggle":
		return m.mcpToggle(arg)
	case "reconnect":
		return m.mcpReconnect(arg)
	default:
		// "list" or bare "/mcp"
		return m.mcpList()
	}
}

// mcpList shows all configured MCP servers with their status.
func (m *Model) mcpList() tea.Cmd {
	if len(m.mcpServers) == 0 {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "No MCP servers configured in ~/.tachi/config.yaml",
		})
		return nil
	}

	var sb strings.Builder
	sb.WriteString("**MCP Servers:**\n\n")

	for _, srv := range m.mcpServers {
		enabled := srv.IsEnabled()
		connected := false
		if m.mcpManager != nil {
			connected = m.mcpManager.IsConnected(srv.Name)
		}

		status := "⚪ Disabled"
		if enabled {
			if connected {
				status = "🟢 Connected"
			} else {
				status = "🔴 Disconnected"
			}
		}

		transport := "?"
		switch srv.Type {
		case config.MCPTransportStdio:
			transport = fmt.Sprintf("stdio `%s`", srv.Command)
		case config.MCPTransportHTTP:
			transport = fmt.Sprintf("http `%s`", srv.URL)
		}

		fmt.Fprintf(&sb, "- **%s** (%s)\n  Transport: %s\n",
			srv.Name, status, transport)
	}

	m.chatview.AddMessage(chatMessage{
		Role:    "assistant",
		Content: sb.String(),
	})
	return nil
}

// mcpToggle enables or disables an MCP server by name.
func (m *Model) mcpToggle(name string) tea.Cmd {
	if name == "" {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "Usage: `/mcp toggle <name>` — specify a server name",
		})
		return nil
	}

	// Find the server config
	idx := -1
	for i := range m.mcpServers {
		if m.mcpServers[i].Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("MCP server **%s** not found in config", name),
		})
		return nil
	}

	srv := &m.mcpServers[idx]
	wasEnabled := srv.Enabled
	if wasEnabled == nil || *wasEnabled {
		// Currently enabled → disable it
		disabled := false
		srv.Enabled = &disabled

		// Disconnect and unregister tools
		if m.mcpManager != nil {
			_ = m.mcpManager.Disconnect(srv.Name)
			m.unregisterMCPTools(srv.Name)
		}

		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("MCP server **%s** disabled", name),
		})
		return nil
	}

	// Currently disabled → enable it asynchronously
	enabled := true
	srv.Enabled = &enabled

	if m.mcpManager == nil {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("MCP server **%s** enabled (no manager available)", name),
		})
		return nil
	}

	m.chatview.AddMessage(chatMessage{
		Role:    "assistant",
		Content: fmt.Sprintf("Enabling MCP server **%s**...", name),
	})

	ch := make(chan string, 1)
	go m.connectAndRegisterMCP(srv, ch)
	return func() tea.Msg {
		content, ok := <-ch
		if !ok {
			return nil
		}
		return mcpStatusMsg{content: content}
	}
}

// mcpReconnect reconnects to a disconnected MCP server by name.
func (m *Model) mcpReconnect(name string) tea.Cmd {
	if name == "" {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "Usage: `/mcp reconnect <name>` — specify a server name",
		})
		return nil
	}

	if m.mcpManager == nil {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "No MCP manager available",
		})
		return nil
	}

	// Find server config
	var srv *config.MCPServerConfig
	for i := range m.mcpServers {
		if m.mcpServers[i].Name == name {
			srv = &m.mcpServers[i]
			break
		}
	}
	if srv == nil {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("MCP server **%s** not found in config", name),
		})
		return nil
	}

	if !srv.IsEnabled() {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("MCP server **%s** is disabled. Use `/mcp toggle %s` to enable it first", name, name),
		})
		return nil
	}

	// Unregister old tools, then reconnect asynchronously
	m.unregisterMCPTools(name)

	m.chatview.AddMessage(chatMessage{
		Role:    "assistant",
		Content: fmt.Sprintf("Reconnecting to MCP server **%s**...", name),
	})

	ch := make(chan string, 1)
	go m.reconnectAndRegisterMCP(srv, ch)
	return func() tea.Msg {
		content, ok := <-ch
		if !ok {
			return nil
		}
		return mcpStatusMsg{content: content}
	}
}

// connectAndRegisterMCP connects to a server and registers its tools.
// Sends the result message to ch for safe delivery in the TUI update loop.
func (m *Model) connectAndRegisterMCP(srv *config.MCPServerConfig, ch chan<- string) {
	defer close(ch)
	ctx, cancel := context.WithTimeout(context.Background(), mcpCommandTimeout)
	defer cancel()

	tools, err := m.mcpManager.Reconnect(ctx, srv)
	if err != nil {
		debuglog.Log("MCP: failed to connect %q: %v", srv.Name, err)
		ch <- fmt.Sprintf("Failed to connect to **%s**: %v", srv.Name, err)
		return
	}

	for _, t := range tools {
		m.agent.RegisterTool(t)
		debuglog.Log("MCP: registered tool %s (%s)", t.Name(), t.Description())
	}

	ch <- fmt.Sprintf("MCP server **%s** connected with %d tool(s)", srv.Name, len(tools))
}

// reconnectAndRegisterMCP reconnects to a server and registers its tools.
// Sends the result message to ch for safe delivery in the TUI update loop.
func (m *Model) reconnectAndRegisterMCP(srv *config.MCPServerConfig, ch chan<- string) {
	defer close(ch)
	ctx, cancel := context.WithTimeout(context.Background(), mcpCommandTimeout)
	defer cancel()

	tools, err := m.mcpManager.Reconnect(ctx, srv)
	if err != nil {
		ch <- fmt.Sprintf("Failed to reconnect to **%s**: %v", srv.Name, err)
		return
	}

	for _, t := range tools {
		m.agent.RegisterTool(t)
		debuglog.Log("MCP: registered tool %s (%s)", t.Name(), t.Description())
	}

	ch <- fmt.Sprintf("MCP server **%s** reconnected with %d tool(s)", srv.Name, len(tools))
}

// unregisterMCPTools removes all tools belonging to a server from the
// agent's tool registry. Tool names follow the pattern mcp__<server>__<tool>.
func (m *Model) unregisterMCPTools(serverName string) {
	prefix := fmt.Sprintf("mcp__%s__", serverName)
	for _, schema := range m.agent.ToolSchemas() {
		if strings.HasPrefix(schema.Name, prefix) {
			m.agent.UnregisterTool(schema.Name)
		}
	}
}
