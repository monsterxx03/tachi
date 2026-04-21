package tui

import (
	"strings"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type InputSubmitMsg string

type InputArea struct {
	textarea textarea.Model
	width    int
	enabled  bool
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
		i.textarea.Focus()
	} else {
		i.textarea.Blur()
	}
}

func (i InputArea) Update(msg tea.Msg) (InputArea, tea.Cmd) {
	if !i.enabled {
		return i, nil
	}

	if keyMsg, ok := msg.(tea.KeyMsg); ok {
		switch keyMsg.String() {
		case "shift+enter":
			i.textarea.InsertString("\n")
			return i, nil
		case "enter":
			text := strings.TrimSpace(i.textarea.Value())
			if text == "" {
				return i, nil
			}
			i.textarea.Reset()
			return i, func() tea.Msg { return InputSubmitMsg(text) }
		}
	}

	var cmd tea.Cmd
	i.textarea, cmd = i.textarea.Update(msg)
	return i, cmd
}

func (i InputArea) View() string {
	return inputBorderStyle.Width(i.width).Render(i.textarea.View())
}
