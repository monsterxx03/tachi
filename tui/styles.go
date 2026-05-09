package tui

import (
	"charm.land/lipgloss/v2"
)

var (
	toolCallStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#F5A97F"))

	toolResultOKStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#A6DA95"))

	toolResultErrStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#ED8796"))

	statusBarStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#6E738D")).
			Background(lipgloss.Color("#1E2030"))

	inputStyle = lipgloss.NewStyle()

	dimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#6E738D"))

	thinkingStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#8087A2")).
			Italic(true)

	stateIdleStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#A6DA95"))

	stateWaitingStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#EED49F"))

	stateStreamingStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#8AADF4"))

	stateConfirmStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#F5A97F"))

	completionSelectedStyle = lipgloss.NewStyle().
					Background(lipgloss.Color("#363A4F")).
					Foreground(lipgloss.Color("#CAD3F5"))

	completionNormalStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#6E738D"))

	userMsgStyle = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder(), false, false, false, true).
			BorderForeground(lipgloss.Color("#7DC4E4")).
			PaddingLeft(1)

	assistantMsgStyle = lipgloss.NewStyle().
				Border(lipgloss.NormalBorder(), false, false, false, true).
				BorderForeground(lipgloss.Color("#A6DA95")).
				PaddingLeft(1)

	selectModeStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#1E2030")).
			Background(lipgloss.Color("#EED49F"))

	confirmStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#EED49F"))

	toolConfirmStyle = lipgloss.NewStyle().
				Border(lipgloss.NormalBorder(), false, false, false, true).
				BorderForeground(lipgloss.Color("#F5A97F")).
				PaddingLeft(1)

	diffDeletedStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#ED8796")) // Red for deleted

	diffAddedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#A6DA95")) // Green for added

	diffContextStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#6E738D")) // Dim for context

	diffHeaderStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#F5A97F")).
			Bold(true)

	usageNormalStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#A6DA95"))

	usageWarnStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#EED49F"))

	usageHighStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#ED8796"))

	boldStyle = lipgloss.NewStyle().
			Bold(true)

	sessionInfoStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#B8C0E0"))

	// MCP overlay styles
	mcpOverlayBorder = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("#8AADF4")).
				Padding(1, 2)

	mcpOverlayTitle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#8AADF4")).
			Bold(true)

	mcpOverlayHint = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#6E738D"))

	mcpToolHeader = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#F5A97F"))

	mcpToolName = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#C6A0F6"))

	mcpServerConnected = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#A6DA95"))

	mcpServerDisconnected = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#ED8796"))

	mcpServerSelected = lipgloss.NewStyle().
				Background(lipgloss.Color("#363A4F")).
				Foreground(lipgloss.Color("#CAD3F5"))

	mcpOAuthBadge = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#EED49F")).
			Background(lipgloss.Color("#5B4A3F"))

	mcpStatusOK = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#A6DA95"))

	mcpStatusWarn = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#EED49F"))

	// MCP tool detail styles
	mcpDetailColHeader = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#5B6078")).
				Bold(true)

	mcpDetailFieldName = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#7DC4E4"))

	mcpDetailFieldType = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#A6DA95"))

	mcpDetailFieldDesc = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#8087A2"))

	pendingMsgStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#6E738D")).
				Italic(true)

	pendingCountStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#EED49F"))
)
