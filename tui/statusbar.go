package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/monsterxx03/tachi/llm"
)

type StatusBar struct {
	width        int
	providerInfo string
	state        state
	totalUsage   *llm.Usage
	copyMode     bool
}

func NewStatusBar(providerInfo string) StatusBar {
	return StatusBar{providerInfo: providerInfo}
}

func (s *StatusBar) SetWidth(w int)            { s.width = w }
func (s *StatusBar) SetState(st state)         { s.state = st }
func (s *StatusBar) SetUsage(u *llm.Usage)     { s.totalUsage = u }
func (s *StatusBar) SetCopyMode(b bool)        { s.copyMode = b }
func (s *StatusBar) SetProviderInfo(info string) { s.providerInfo = info }
func (s *StatusBar) ProviderInfo() string        { return s.providerInfo }

func (s StatusBar) View() string {
	var dot string
	switch s.state {
	case stateIdle:
		dot = stateIdleStyle.Render("●")
	case stateWaiting:
		dot = stateWaitingStyle.Render("●")
	case stateStreaming:
		dot = stateStreamingStyle.Render("●")
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
	if s.totalUsage != nil && (s.totalUsage.InputTokens > 0 || s.totalUsage.OutputTokens > 0) {
		right = fmt.Sprintf("in: %s  out: %s ",
			formatTokens(s.totalUsage.InputTokens), formatTokens(s.totalUsage.OutputTokens))
	}

	gap := s.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 0 {
		gap = 0
	}
	return statusBarStyle.Render(left + strings.Repeat(" ", gap) + right)
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
