package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

type Command struct {
	Name        string
	Description string
	handler     func(*Model) tea.Cmd
}

var commands = []Command{
	{
		Name:        "/clear",
		Description: "Clear conversation history",
		handler:     func(m *Model) tea.Cmd { m.history = nil; m.chatview.Clear(); return nil },
	},
	{
		Name:        "/quit",
		Description: "Exit tachi",
		handler:     func(m *Model) tea.Cmd { return tea.Quit },
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
