package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/monsterxx03/tachi/config"
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
