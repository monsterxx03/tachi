package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type InputSubmitMsg string

type InputArea struct {
	textarea    textarea.Model
	width       int
	enabled     bool
	completions []Command
	selectedIdx int
}

func NewInputArea() InputArea {
	ta := textarea.New()
	ta.Placeholder = "Send a message... (Enter to send, Shift+Enter for newline)"
	ta.Prompt = "> "
	ta.CharLimit = 0
	ta.SetHeight(1)
	ta.ShowLineNumbers = false
	ta.KeyMap.InsertNewline.SetEnabled(false)
	styles := textarea.DefaultDarkStyles()
	styles.Focused.CursorLine = lipgloss.NewStyle()
	ta.SetStyles(styles)
	ta.Focus()

	return InputArea{textarea: ta, enabled: true}
}

func (i *InputArea) SetWidth(w int) {
	i.width = w
	i.textarea.SetWidth(w - 2)
}

func (i *InputArea) SetEnabled(enabled bool) {
	i.enabled = enabled
	if enabled {
		i.textarea.Placeholder = "Send a message... (Enter to send, Shift+Enter for newline)"
		i.textarea.Focus()
	} else {
		i.textarea.Placeholder = "Ctrl+C to interrupt"
		i.completions = nil
		i.selectedIdx = 0
		i.textarea.Blur()
	}
}

func (i InputArea) Height() int {
	return 2 + len(i.completions)
}

func (i InputArea) Update(msg tea.Msg) (InputArea, tea.Cmd) {
	if !i.enabled {
		return i, nil
	}

	if keyMsg, ok := msg.(tea.KeyMsg); ok {
		switch keyMsg.String() {
		case "up", "ctrl+k", "ctrl+p":
			if len(i.completions) > 0 {
				if i.selectedIdx > 0 {
					i.selectedIdx--
				}
				return i, nil
			}
		case "down", "ctrl+j", "ctrl+n":
			if len(i.completions) > 0 {
				if i.selectedIdx < len(i.completions)-1 {
					i.selectedIdx++
				}
				return i, nil
			}
		case "tab":
			if len(i.completions) > 0 {
				i.textarea.SetValue(i.completions[i.selectedIdx].Name)
				i.textarea.CursorEnd()
				i.updateCompletions()
				return i, nil
			}
		case "esc":
			if len(i.completions) > 0 {
				i.completions = nil
				i.selectedIdx = 0
				return i, nil
			}
		case "shift+enter":
			i.textarea.InsertString("\n")
			return i, nil
		case "enter":
			if len(i.completions) > 0 {
				name := i.completions[i.selectedIdx].Name
				i.textarea.Reset()
				i.completions = nil
				i.selectedIdx = 0
				return i, func() tea.Msg { return InputSubmitMsg(name) }
			}
			text := strings.TrimSpace(i.textarea.Value())
			if text == "" {
				return i, nil
			}
			i.textarea.Reset()
			i.completions = nil
			i.selectedIdx = 0
			return i, func() tea.Msg { return InputSubmitMsg(text) }
		}
	}

	var cmd tea.Cmd
	i.textarea, cmd = i.textarea.Update(msg)
	i.updateCompletions()
	return i, cmd
}

func (i *InputArea) updateCompletions() {
	val := i.textarea.Value()
	if strings.HasPrefix(val, "/") {
		i.completions = matchCommands(val)
		if i.selectedIdx >= len(i.completions) {
			i.selectedIdx = 0
		}
	} else {
		i.completions = nil
		i.selectedIdx = 0
	}
}

func (i InputArea) View() string {
	if len(i.completions) == 0 {
		return inputStyle.Width(i.width).Render(i.textarea.View())
	}

	var b strings.Builder
	maxNameLen := 0
	for _, cmd := range i.completions {
		if len(cmd.Name) > maxNameLen {
			maxNameLen = len(cmd.Name)
		}
	}
	for idx, cmd := range i.completions {
		line := fmt.Sprintf("  %-*s  %s", maxNameLen, cmd.Name, cmd.Description)
		if idx == i.selectedIdx {
			b.WriteString(completionSelectedStyle.Width(i.width).Render(line))
		} else {
			b.WriteString(completionNormalStyle.Width(i.width).Render(line))
		}
		b.WriteString("\n")
	}
	b.WriteString(inputStyle.Width(i.width).Render(i.textarea.View()))
	return b.String()
}
