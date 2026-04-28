package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

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
}

func NewStatusBar(providerInfo string, contextWindow int64) StatusBar {
	sp := spinner.New(spinner.WithSpinner(spinner.Dot))
	return StatusBar{providerInfo: providerInfo, contextWindow: contextWindow, spinner: sp}
}

func (s *StatusBar) SetWidth(w int)               { s.width = w }
func (s *StatusBar) SetState(st state)             { s.state = st }
func (s *StatusBar) SetUsage(u *llm.Usage)         { s.totalUsage = u }
func (s *StatusBar) SetCopyMode(b bool)            { s.copyMode = b }
func (s *StatusBar) SetProviderInfo(info string)    { s.providerInfo = info }
func (s *StatusBar) SetContextWindow(cw int64)      { s.contextWindow = cw }
func (s *StatusBar) ProviderInfo() string           { return s.providerInfo }

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
	case stateSelectingModel:
		dot = stateWaitingStyle.Render("●")
	case stateAwaitingConfirmation, stateAskUserQuestion:
		dot = stateConfirmStyle.Render("●")
	}

	left := fmt.Sprintf(" %s %s | %s", dot, "tachi", s.providerInfo)
	if s.copyMode {
		left += " | " + selectModeStyle.Render("SELECT")
	}

	var right string
	if s.totalUsage != nil && s.totalUsage.InputTokens > 0 {
		right = s.buildUsageRight()
	}

	gap := s.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 0 {
		gap = 0
	}
	return statusBarStyle.Render(left + strings.Repeat(" ", gap) + right)
}

func (s StatusBar) buildUsageRight() string {
	// Context usage: show input tokens as fraction of context window
	if s.totalUsage.InputTokens > 0 && s.contextWindow > 0 {
		pct := float64(s.totalUsage.InputTokens) / float64(s.contextWindow) * 100
		ctxStr := fmt.Sprintf("ctx: %s/%s %s",
			formatTokens(s.totalUsage.InputTokens),
			formatTokens(s.contextWindow),
			formatPercent(pct))
		return usageColorStyle(pct).Render(ctxStr) + " "
	}
	return ""
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

func formatTokens(n int64) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}
