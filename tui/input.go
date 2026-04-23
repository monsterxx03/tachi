package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/monsterxx03/tachi/pkg/debuglog"
)

type InputSubmitMsg string

type InputArea struct {
	textarea    textarea.Model
	width       int
	enabled     bool
	completions []Command
	selectedIdx int

	history     []string
	historyMax  int
	histIdx     int
	histScratch string
	historyPath string
}

func NewInputArea(historyMax int, historyPath string) InputArea {
	ta := textarea.New()
	ta.Placeholder = "Send a message... (Enter to send, Shift+Enter for newline; Ctrl+P/N history)"
	ta.Prompt = "> "
	ta.CharLimit = 0
	ta.SetHeight(1)
	ta.ShowLineNumbers = false
	ta.KeyMap.InsertNewline.SetEnabled(false)
	styles := textarea.DefaultDarkStyles()
	styles.Focused.CursorLine = lipgloss.NewStyle()
	ta.SetStyles(styles)
	ta.Focus()

	history := make([]string, 0)
	if historyMax > 0 && historyPath != "" {
		history = loadInputHistoryFile(historyPath, historyMax)
	}
	return InputArea{
		textarea:    ta,
		enabled:     true,
		history:     history,
		historyMax:  historyMax,
		histIdx:     -1,
		historyPath: historyPath,
	}
}

func (i *InputArea) SetWidth(w int) {
	i.width = w
	i.textarea.SetWidth(w - 2)
}

func (i *InputArea) SetEnabled(enabled bool) {
	i.enabled = enabled
	if enabled {
		i.textarea.Placeholder = "Send a message... (Enter to send, Shift+Enter for newline; Ctrl+P/N history)"
		i.histIdx = -1
		i.histScratch = ""
		i.textarea.Focus()
	} else {
		i.textarea.Placeholder = "Ctrl+C to interrupt"
		i.completions = nil
		i.selectedIdx = 0
		i.histIdx = -1
		i.histScratch = ""
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
		case "up", "ctrl+p":
			if len(i.completions) > 0 {
				if i.selectedIdx > 0 {
					i.selectedIdx--
				}
				return i, nil
			}
			if i.historyMax > 0 && i.historyKeyPrev() {
				return i, nil
			}
		case "down", "ctrl+n":
			if len(i.completions) > 0 {
				if i.selectedIdx < len(i.completions)-1 {
					i.selectedIdx++
				}
				return i, nil
			}
			if i.historyMax > 0 && i.historyKeyNext() {
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
				i.pushHistoryLine(name)
				i.textarea.Reset()
				i.clearHistoryNav()
				i.completions = nil
				i.selectedIdx = 0
				return i, func() tea.Msg { return InputSubmitMsg(name) }
			}
			text := strings.TrimSpace(i.textarea.Value())
			if text == "" {
				return i, nil
			}
			i.pushHistoryLine(text)
			i.textarea.Reset()
			i.clearHistoryNav()
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

func (i *InputArea) clearHistoryNav() {
	i.histIdx = -1
	i.histScratch = ""
}

func (i *InputArea) setHistoryValue(s string) {
	i.textarea.SetValue(s)
	i.textarea.CursorEnd()
	i.updateCompletions()
}

func (i *InputArea) historyKeyPrev() bool {
	if len(i.history) == 0 {
		return false
	}
	if i.histIdx < 0 {
		i.histScratch = i.textarea.Value()
		i.histIdx = len(i.history) - 1
		i.setHistoryValue(i.history[i.histIdx])
		return true
	}
	if i.histIdx == 0 {
		return true
	}
	i.histIdx--
	i.setHistoryValue(i.history[i.histIdx])
	return true
}

func (i *InputArea) historyKeyNext() bool {
	if i.histIdx < 0 {
		return false
	}
	if i.histIdx < len(i.history)-1 {
		i.histIdx++
		i.setHistoryValue(i.history[i.histIdx])
		return true
	}
	i.histIdx = -1
	i.setHistoryValue(i.histScratch)
	return true
}

func (i *InputArea) pushHistoryLine(line string) {
	if i.historyMax <= 0 {
		return
	}
	if line == "" {
		return
	}
	if len(i.history) > 0 && i.history[len(i.history)-1] == line {
		return
	}
	i.history = append(i.history, line)
	if len(i.history) > i.historyMax {
		i.history = i.history[1:]
	}
	if i.historyPath != "" {
		if err := saveInputHistoryFile(i.historyPath, i.history); err != nil {
			debuglog.Log("input history: save: %v", err)
		}
	}
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
