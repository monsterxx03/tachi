package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/llm"
)

type StatusBar struct {
	width         int
	providerInfo  string
	state         state
	totalUsage    *llm.Usage
	contextWindow int64
	copyMode      bool
	spinner       spinner.Model
	sessionTitle  string
	sessionID     string
	pendingCount  int
	mcpReady      bool   // true when MCP async init completes
	mcpEnabled    bool   // true when MCP servers are configured
	mcpError      string // non-empty when MCP async init had errors
	compacting    bool   // true when auto-compaction is in progress
	modeBadge     string // current mode badge text (e.g. "[auto]")
	reviewBadge   string // multi-round /review indicator (e.g. "⚔️ 挑战者 2/5"); empty when not reviewing
}

const (
	maxSessionTitleLen = 30
)

// statusbarTruncStyle performs true truncation (lipgloss MaxWidth) as opposed
// to statusBarStyle's Width(), which word-wraps. Used to keep the statusbar
// on a single line when the left+right halves overflow the terminal width.
var statusbarTruncStyle = lipgloss.NewStyle()

func NewStatusBar(providerInfo string, contextWindow int64) StatusBar {
	sp := spinner.New(spinner.WithSpinner(spinner.Dot))
	return StatusBar{providerInfo: providerInfo, contextWindow: contextWindow, spinner: sp}
}

func (s *StatusBar) SetWidth(w int)                  { s.width = w }
func (s *StatusBar) SetState(st state)               { s.state = st }
func (s *StatusBar) SetUsage(u *llm.Usage)           { s.totalUsage = u }
func (s *StatusBar) SetCopyMode(b bool)              { s.copyMode = b }
func (s *StatusBar) SetProviderInfo(info string)     { s.providerInfo = info }
func (s *StatusBar) SetContextWindow(cw int64)       { s.contextWindow = cw }
func (s *StatusBar) SetSessionInfo(title, id string) { s.sessionTitle = title; s.sessionID = id }
func (s *StatusBar) ProviderInfo() string            { return s.providerInfo }

func (s *StatusBar) SetPendingCount(n int) { s.pendingCount = n }
func (s *StatusBar) SetMCPReady(v bool)    { s.mcpReady = v }
func (s *StatusBar) SetMCPEnabled(v bool)  { s.mcpEnabled = v }
func (s *StatusBar) SetMCPError(v string)  { s.mcpError = v }
func (s *StatusBar) SetCompacting(v bool)  { s.compacting = v }
func (s *StatusBar) SetMode(mode string)   { s.modeBadge = modeBadgeFor(mode) }
func (s *StatusBar) SetReviewBadge(b string) {
	s.reviewBadge = b
}
func (s *StatusBar) ClearReviewBadge() { s.reviewBadge = "" }

func (s *StatusBar) Tick() tea.Cmd { return s.spinner.Tick }

func (s *StatusBar) Update(msg spinner.TickMsg) tea.Cmd {
	var cmd tea.Cmd
	s.spinner, cmd = s.spinner.Update(msg)
	return cmd
}

func (s StatusBar) View() string {
	var dot string
	switch s.state {
	case stateIdle:
		dot = stateIdleStyle.Render("●")
	case stateWaiting:
		dot = stateWaitingStyle.Render(s.spinner.View())
	case stateStreaming:
		dot = stateStreamingStyle.Render(s.spinner.View())
	case stateSelectingModel, stateSelectingSession:
		dot = stateWaitingStyle.Render("●")
	case stateAwaitingConfirmation, stateAskUserQuestion:
		dot = stateConfirmStyle.Render("●")
	}

	left := fmt.Sprintf(" %s %s", dot, "tachi")

	// Session info: title · #shortID
	if s.sessionID != "" {
		title := s.sessionTitle
		if title == "" {
			title = "(untitled)"
		}
		// Truncate title with rune awareness
		title = s.truncateTitle(title)
		// Session ID format: YYYY-MM-DD-HHMMSS-uuid8 — show only the uuid suffix
		id := s.sessionID
		if idx := strings.LastIndex(id, "-"); idx >= 0 && idx+1 < len(id) {
			id = id[idx+1:]
		}
		left += " " + sessionInfoStyle.Render(fmt.Sprintf("· %s · #%s", title, id))
	}

	left += " | " + s.providerInfo
	if s.modeBadge != "" {
		left += " " + modeBadgeStyleFor(s.modeBadge).Render(s.modeBadge)
	}
	if s.mcpEnabled && !s.mcpReady {
		left += " | " + mcpConnectingStyle.Render("MCP: connecting...")
	} else if s.mcpEnabled && s.mcpReady && s.mcpError != "" {
		left += " | " + mcpErrorStyle.Render("MCP: "+s.mcpError)
	} else if s.mcpEnabled && s.mcpReady {
		left += " | " + mcpReadyStyle.Render("MCP: ready")
	}
	if s.pendingCount > 0 {
		left += " | " + pendingCountStyle.Render(fmt.Sprintf("⏳ %d pending", s.pendingCount))
	}
	if s.reviewBadge != "" {
		left += " | " + reviewBadgeStyle.Render(s.reviewBadge)
	}
	if s.compacting {
		left += " | " + mcpConnectingStyle.Render("compacting...")
	}
	if s.copyMode {
		left += " | " + selectModeStyle.Render("SELECT")
	}

	var right string
	if s.totalUsage != nil && s.totalUsage.LastInputTokens > 0 {
		right = s.buildUsageRight()
	}

	gap := max(s.width-lipgloss.Width(left)-lipgloss.Width(right), 0)
	// The gap math above keeps the line within the terminal width whenever
	// left+right fit. When they overflow (long title/provider/badge), lipgloss
	// .Width() would word-WRAP the line into two rows — it does not truncate —
	// so truncate the LEFT side instead (MaxWidth) to keep the right side's
	// usage/cost info visible on a single line.
	if avail := max(s.width-lipgloss.Width(right), 0); lipgloss.Width(left) > avail {
		left = statusbarTruncStyle.MaxWidth(avail).Render(left)
	}
	return statusBarStyle.Width(s.width).Render(left + strings.Repeat(" ", gap) + right)
}

// truncateTitle returns the session title truncated to no more than
// maxSessionTitleLen runes. Uses rune-aware truncation to safely
// handle multi-byte characters (e.g. CJK).
func (s StatusBar) truncateTitle(title string) string {
	runes := []rune(title)
	if len(runes) > maxSessionTitleLen {
		return string(runes[:maxSessionTitleLen-1]) + "…"
	}
	return title
}

func (s StatusBar) buildUsageRight() string {
	var parts []string

	// Context usage: show the most recent per-call input token estimate as a
	// percentage of the context window only (no n/m breakdown). Unlike
	// InputTokens (which accumulates across all API calls in the session and
	// grows monotonically), LastInputTokens reflects the true per-call context
	// size — the number of tokens sent in the most recent API request,
	// estimated locally before the call.
	if s.totalUsage != nil && s.totalUsage.LastInputTokens > 0 && s.contextWindow > 0 {
		pct := float64(s.totalUsage.LastInputTokens) / float64(s.contextWindow) * 100
		ctxStr := fmt.Sprintf("ctx: %s", formatPercent(pct))
		parts = append(parts, usageColorStyle(pct).Render(ctxStr))
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ") + " "
}

func usageColorStyle(pct float64) lipgloss.Style {
	switch {
	case pct >= 80:
		return usageHighStyle
	case pct >= 50:
		return usageWarnStyle
	default:
		return usageNormalStyle
	}
}

func formatPercent(pct float64) string {
	if pct >= 99.95 {
		return "~100%"
	}
	return fmt.Sprintf("%.0f%%", pct)
}

// modeBadgeFor returns the display text for a session mode badge.
func modeBadgeFor(mode string) string {
	switch mode {
	case agent.ModeAuto:
		return "[auto]"
	case agent.ModePlan:
		return "[plan]"
	case agent.ModeChat:
		return "[chat]"
	default:
		return ""
	}
}

// modeBadgeStyleFor returns the lipgloss style for a mode badge.
func modeBadgeStyleFor(badge string) lipgloss.Style {
	switch badge {
	case "[plan]":
		return modePlanStyle
	case "[chat]":
		return modeChatStyle
	default:
		return modeAutoStyle
	}
}
