package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

type StatusBar struct {
	width        int
	providerInfo string
	state        state
}

func NewStatusBar(providerInfo string) StatusBar {
	return StatusBar{providerInfo: providerInfo}
}

func (s *StatusBar) SetWidth(w int)    { s.width = w }
func (s *StatusBar) SetState(st state) { s.state = st }

func (s StatusBar) View() string {
	left := fmt.Sprintf(" tachi | %s", s.providerInfo)
	var right string
	switch s.state {
	case stateWaiting:
		right = "waiting... "
	case stateStreaming:
		right = "streaming... "
	case stateIdle:
		right = "ready "
	}
	gap := s.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 0 {
		gap = 0
	}
	return statusBarStyle.Width(s.width).Render(
		left + strings.Repeat(" ", gap) + right,
	)
}
