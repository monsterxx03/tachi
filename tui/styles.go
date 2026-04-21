package tui

import (
	"charm.land/lipgloss/v2"
)

var (
	userLabelStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#7DC4E4"))

	assistantLabelStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("#A6DA95"))

	toolCallStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#F5A97F"))

	toolResultOKStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#A6DA95"))

	toolResultErrStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#ED8796"))

	statusBarStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#6E738D")).
			Background(lipgloss.Color("#1E2030")).
			Padding(0, 1)

	inputBorderStyle = lipgloss.NewStyle().
				Border(lipgloss.NormalBorder(), true, false, false, false).
				BorderForeground(lipgloss.Color("#363A4F"))

	dimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#6E738D"))

	thinkingStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#8087A2")).
			Italic(true)
)
