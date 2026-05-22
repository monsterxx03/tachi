package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/monsterxx03/tachi/pkg/debuglog"
)

type InputSubmitMsg string

// InputArea provides the TUI input with slash-command completions and @-file
// fuzzy-search completions.
type InputArea struct {
	textarea    textarea.Model
	width       int
	enabled     bool
	completions []Command
	selectedIdx int

	// @-file completions
	atFileQuery       string
	atFileMatches     []atFileMatch
	atFileSelectedIdx int

	history     []string
	historyMax  int
	histIdx     int
	histScratch string
	historyPath string

	// Paste threshold — when a paste exceeds this many lines, the content is
	// collapsed into a placeholder to avoid flooding the chat UI. pasteBuffer
	// holds the full pasted text, which is sent to the LLM on submit.
	pasteBuffer    string
	pasteThreshold int

	logger *debuglog.Logger
}

func NewInputArea(historyMax int, historyPath string) InputArea {
	ta := textarea.New()
	ta.Placeholder = "Send a message... (Enter to send, Shift+Enter for newline; Ctrl+P/N history)"
	// Only show the "> " prompt on the first visual line. Wrapped continuation
	// lines get a 2-space indent instead to keep alignment without clutter.
	ta.Prompt = ""
	ta.SetPromptFunc(2, func(info textarea.PromptInfo) string {
		if info.LineNumber == 0 {
			return "> "
		}
		return "  "
	})
	ta.CharLimit = 0
	ta.DynamicHeight = true
	ta.MinHeight = 1
	ta.MaxHeight = 15
	ta.MaxContentHeight = 30
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
		pasteThreshold: 5,
		logger:      debuglog.DefaultLogger,
	}
}

func (i *InputArea) SetWidth(w int) {
	i.width = w
	i.textarea.SetWidth(w - 2)
}

// SetMaxHeight dynamically caps the total height of the input area (textarea +
// completions) to at most maxTotal. This allows the parent layout to constrain
// the input area and force internal scrolling in the textarea when screen space
// is tight.
func (i *InputArea) SetMaxHeight(maxTotal int) {
	// Reserve lines for slash-completions and @-file completions
	completionLines := 0
	if i.completions != nil {
		completionLines = len(i.completions)
	}
	if i.atFileMatches != nil {
		completionLines += 1 + len(i.atFileMatches) // header + matches
	}
	maxTA := maxTotal - completionLines
	if maxTA < 1 {
		maxTA = 1
	}
	i.textarea.MaxHeight = maxTA
}

func (i *InputArea) SetEnabled(enabled bool) {
	i.enabled = enabled
	if enabled {
		i.textarea.Placeholder = "Send a message... (Enter to send, Shift+Enter for newline; Ctrl+P/N history)"
		i.histIdx = -1
		i.histScratch = ""
		i.clearAtFileCompletions()
		i.textarea.Focus()
	} else {
		i.textarea.Placeholder = "Ctrl+C to interrupt"
		i.completions = nil
		i.selectedIdx = 0
		i.clearAtFileCompletions()
		i.histIdx = -1
		i.histScratch = ""
		i.textarea.Blur()
	}
}

func (i InputArea) Height() int {
	h := i.textarea.Height() + len(i.completions)
	if i.atFileMatches != nil {
		h += 1 + len(i.atFileMatches) // header + matches
	}
	return h
}

// 正在上下翻历史（此时不以 / 前缀触发补全，避免与历史键冲突）。
func (i InputArea) browsingHistory() bool { return i.histIdx >= 0 }

// slash 补全列表参与 Tab / 方向键 等（与 browsingHistory 互斥）。
func (i InputArea) completionsOn() bool { return i.histIdx < 0 && len(i.completions) > 0 }

// @-file 补全列表参与 Tab / 方向键 等（与 browsingHistory 互斥）。
func (i InputArea) atFileCompletionsOn() bool { return i.histIdx < 0 && i.atFileMatches != nil }

func (i *InputArea) clearAtFileCompletions() {
	i.atFileQuery = ""
	i.atFileMatches = nil
	i.atFileSelectedIdx = 0
}

// pastePlaceholder returns a compact, dim-styled label for a large paste so
// the input area doesn't flood the chat UI with hundreds of lines.
func (i *InputArea) pastePlaceholder(lines int) string {
	return fmt.Sprintf("[Pasted %d lines]", lines)
}

// handlePaste intercepts a clipboard paste. If the content looks like one or
// more file paths that exist on disk (e.g. dragged-and-dropped from Finder), it
// converts them to @-references for the existing @-file expansion system.
// Otherwise it falls through to the large-paste threshold handling.
func (i *InputArea) handlePaste(text string) (InputArea, tea.Cmd) {
	// Always clear any existing pasteBuffer first — each paste replaces
	// the previous one (no stacking).
	i.expandPasteBuffer()

	// File drag-and-drop detection: if the pasted text looks like file
	// path(s) and they exist on disk, wrap them as @-references.
	if wrapped, ok := i.wrapDroppedFiles(text); ok {
		i.logger.Log("input: detected dragged file(s): %q → %q", text, wrapped)
		i.textarea.InsertString(wrapped)
		return *i, nil
	}

	lineCount := strings.Count(text, "\n") + 1
	if lineCount <= i.pasteThreshold || i.pasteThreshold <= 0 {
		// Short paste: insert directly
		i.textarea.InsertString(text)
		return *i, nil
	}

	// Large paste: stash full text and show a placeholder in the textarea.
	i.pasteBuffer = text
	i.textarea.InsertString(i.pastePlaceholder(lineCount))
	return *i, nil
}

// expandPasteBuffer restores the full pasted content into the textarea in
// place of the placeholder, then clears pasteBuffer. Safe to call even when
// there is no pending paste.
func (i *InputArea) expandPasteBuffer() {
	if i.pasteBuffer == "" {
		return
	}
	// Determine what the placeholder looks like and replace it.
	lineCount := strings.Count(i.pasteBuffer, "\n") + 1
	placeholder := i.pastePlaceholder(lineCount)
	val := i.textarea.Value()
	val = strings.Replace(val, placeholder, i.pasteBuffer, 1)
	i.textarea.SetValue(val)
	i.textarea.CursorEnd()
	i.pasteBuffer = ""
}

// wrapDroppedFiles checks if the pasted text looks like one or more file paths
// dragged from the system (Finder, file manager, etc.). If all parts resolve to
// existing files or directories on disk, it converts them to @-references (e.g.,
// "/Users/me/proj/foo.go" → "@foo.go") for the @-file expansion system.
//
// Returns the transformed text and true on success, or ("", false) to fall
// through to normal paste handling.
func (i *InputArea) wrapDroppedFiles(text string) (string, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", false
	}

	// Split by unescaped spaces. This handles file names with spaces,
	// which terminals like kitty and iTerm2 escape with backslash.
	parts := splitUnescaped(text)
	if len(parts) == 0 {
		return "", false
	}

	cwd, err := os.Getwd()
	if err != nil {
		return "", false
	}

	var wrapped []string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		// Expand ~ to home directory (some terminals may paste ~ paths)
		resolved := part
		if strings.HasPrefix(resolved, "~") {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", false
			}
			resolved = filepath.Join(home, resolved[1:])
		}

		// Verify the path actually exists on disk
		if _, err := os.Stat(resolved); err != nil {
			return "", false
		}

		// Convert to relative path if possible (cleaner @-references)
		rel, err := filepath.Rel(cwd, resolved)
		if err == nil && !strings.HasPrefix(rel, "..") {
			wrapped = append(wrapped, "@"+rel)
		} else {
			// Outside cwd — use the original text
			wrapped = append(wrapped, "@"+part)
		}
	}

	if len(wrapped) > 0 {
		return strings.Join(wrapped, " "), true
	}
	return "", false
}

// splitUnescaped splits text by spaces, respecting backslash-escaped spaces.
// This handles file paths with spaces as produced by kitty, iTerm2, and other
// terminals when dragging files (e.g., "/path/to/my\ file.txt").
func splitUnescaped(text string) []string {
	var parts []string
	var current strings.Builder
	escaped := false
	for _, ch := range text {
		if escaped {
			current.WriteRune(ch)
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if ch == ' ' {
			parts = append(parts, current.String())
			current.Reset()
			continue
		}
		current.WriteRune(ch)
	}
	if current.Len() > 0 {
		parts = append(parts, current.String())
	}
	return parts
}

func (i InputArea) Update(msg tea.Msg) (InputArea, tea.Cmd) {
	if !i.enabled {
		return i, nil
	}

	// Intercept PasteMsg: if content exceeds threshold, collapse into a
	// placeholder to avoid flooding the chat UI.
	if pasteMsg, ok := msg.(tea.PasteMsg); ok {
		return i.handlePaste(pasteMsg.Content)
	}

	if keyMsg, ok := msg.(tea.KeyMsg); ok {
		switch keyMsg.String() {
		case "up", "ctrl+p":
			if i.atFileCompletionsOn() {
				if i.atFileSelectedIdx < len(i.atFileMatches)-1 {
					i.atFileSelectedIdx++
				}
				return i, nil
			}
			if i.completionsOn() {
				if i.selectedIdx > 0 {
					i.selectedIdx--
				}
				return i, nil
			}
			if i.historyMax > 0 && i.historyKeyPrev() {
				return i, nil
			}
		case "down", "ctrl+n":
			if i.atFileCompletionsOn() {
				if i.atFileSelectedIdx > 0 {
					i.atFileSelectedIdx--
				}
				return i, nil
			}
			if i.completionsOn() {
				if i.selectedIdx < len(i.completions)-1 {
					i.selectedIdx++
				}
				return i, nil
			}
			if i.historyMax > 0 && i.historyKeyNext() {
				return i, nil
			}
		case "tab":
			if i.atFileCompletionsOn() {
				i.applyAtFileCompletion()
				i.updateCompletions()
				return i, nil
			}
			if i.completionsOn() {
				i.textarea.SetValue(i.completions[i.selectedIdx].Name)
				i.textarea.CursorEnd()
				i.updateCompletions()
				return i, nil
			}
		case "esc":
			if i.atFileCompletionsOn() {
				i.clearAtFileCompletions()
				return i, nil
			}
			if i.completionsOn() {
				i.completions = nil
				i.selectedIdx = 0
				return i, nil
			}
		case "shift+enter":
			i.textarea.InsertString("\n")
			return i, nil
		case "enter":
			if i.atFileCompletionsOn() {
				i.applyAtFileCompletion()
				i.clearAtFileCompletions()
				i.updateCompletions()
				return i, nil
			}
			if i.completionsOn() {
				name := i.completions[i.selectedIdx].Name
				i.expandPasteBuffer()
				i.pushHistoryLine(name)
				i.textarea.Reset()
				i.clearHistoryNav()
				i.completions = nil
				i.selectedIdx = 0
				return i, func() tea.Msg { return InputSubmitMsg(name) }
			}
			i.expandPasteBuffer()
			text := strings.TrimSpace(i.textarea.Value())
			if text == "" {
				return i, nil
			}
			i.pushHistoryLine(text)
			i.textarea.Reset()
			i.clearHistoryNav()
			i.clearAtFileCompletions()
			i.completions = nil
			i.selectedIdx = 0
			return i, func() tea.Msg { return InputSubmitMsg(text) }
		}
	}

	var cmd tea.Cmd
	i.textarea, cmd = i.textarea.Update(msg)
	if i.browsingHistory() {
		i.clearHistoryNav()
	}
	i.updateCompletions()
	return i, cmd
}

func (i *InputArea) applyAtFileCompletion() {
	if !i.atFileCompletionsOn() {
		return
	}
	match := i.atFileMatches[i.atFileSelectedIdx]

	// Replace "@query" with "@match.Path " in the textarea value
	val := i.textarea.Value()
	atPos := findLastAt(val)
	if atPos < 0 {
		return
	}

	before := val[:atPos]
	after := val[atPos+len(i.atFileQuery)+1:]
	newVal := before + "@" + match.Path + " " + after
	i.textarea.SetValue(newVal)
	i.textarea.CursorEnd()
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
			i.logger.Log("input history: save: %v", err)
		}
	}
}

func (i *InputArea) updateCompletions() {
	if i.browsingHistory() {
		i.completions = nil
		i.selectedIdx = 0
		i.clearAtFileCompletions()
		return
	}
	val := i.textarea.Value()
	if strings.HasPrefix(val, "/") {
		i.completions = matchCommands(val)
		if i.selectedIdx >= len(i.completions) {
			i.selectedIdx = 0
		}
		i.clearAtFileCompletions()
	} else {
		i.completions = nil
		i.selectedIdx = 0
		i.updateAtFileCompletions(val)
	}
}

func (i *InputArea) updateAtFileCompletions(val string) {
	atPos := findLastAt(val)
	if atPos < 0 {
		i.clearAtFileCompletions()
		return
	}

	query := val[atPos+1:]
	// Don't trigger if query contains a space (already completed)
	if strings.Contains(query, " ") {
		i.clearAtFileCompletions()
		return
	}

	if query == i.atFileQuery && i.atFileMatches != nil {
		return // No change, skip search
	}

	matches, err := searchAtFiles(query)
	if err != nil || len(matches) == 0 {
		i.clearAtFileCompletions()
		return
	}

	i.atFileQuery = query
	i.atFileMatches = matches
	i.atFileSelectedIdx = 0 // best score, rendered at bottom
}

func (i InputArea) View() string {
	hasCompletions := len(i.completions) > 0
	hasAtFiles := i.atFileMatches != nil

	if !hasCompletions && !hasAtFiles {
		return inputStyle.Width(i.width).Render(i.textarea.View())
	}

	var b strings.Builder

	// Render slash command completions first
	if hasCompletions {
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
	}

	// Render @-file completions (best score at the bottom)
	if hasAtFiles {
		b.WriteString(dimStyle.Render(fmt.Sprintf("  @ files matching %q:", i.atFileQuery)))
		b.WriteString("\n")

		for idx := len(i.atFileMatches) - 1; idx >= 0; idx-- {
			m := i.atFileMatches[idx]
			icon := "  "
			if m.IsDir {
				icon = " D"
			}
			line := fmt.Sprintf("%s %s", icon, m.Path)
			if idx == i.atFileSelectedIdx {
				b.WriteString(completionSelectedStyle.Width(i.width).Render(line))
			} else {
				b.WriteString(completionNormalStyle.Width(i.width).Render(line))
			}
			b.WriteString("\n")
		}
	}

	b.WriteString(inputStyle.Width(i.width).Render(i.textarea.View()))
	return b.String()
}
