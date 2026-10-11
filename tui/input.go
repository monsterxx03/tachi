package tui

import (
	"context"
	"fmt"
	"os"
	"strings"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/monsterxx03/tachi/agent/agentref"
	"github.com/monsterxx03/tachi/agent/atfile"
	cmds "github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/agent/sessionref"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/fileindex"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

type InputSubmitMsg string

// inputPlaceholder is the hint line an empty input shows. It names the keys the TUI binds ITSELF
// (the ones a reader could not guess): Ctrl+V is the paste, and the reason it has to be named is
// that the terminal's own Cmd+V can only ever deliver text.
const inputPlaceholder = "Send a message... (Enter to send, Shift+Enter for newline; Ctrl+P/N history, Ctrl+V paste)"

// maxCompletionDescLen is the maximum rune length for a skill description
// shown in slash-command completions. Longer descriptions are truncated.
const maxCompletionDescLen = 60

// InputArea provides the TUI input with slash-command completions and @-file
// fuzzy-search completions.
type InputArea struct {
	textarea    textarea.Model
	width       int
	enabled     bool
	completions []Command
	selectedIdx int

	// @-completions: ONE popup that serves both marks — '@' names a path, '#' names an entity
	// (@a: a node, #s: a conversation, see agent/atfile). A row carries the text it inserts, so
	// the popup and the expansion can never disagree about a reference.
	atQuery       string
	atMark        byte
	atMatches     []atMatch
	atSelectedIdx int
	// files indexes the working directory for @-file completion (cached per
	// root, so keystrokes do not re-run ripgrep).
	files *fileindex.Index
	// sessions resolves and searches other conversations. nil = the session
	// namespace offers nothing (a frontend with no session store, most tests).
	sessions *sessionref.Source
	// agents resolves and searches this machine's nodes. nil = the node namespace offers
	// nothing, which is what an entry point with no node side gets — the '#' picker then offers
	// only the conversation namespace, and a "#a:" reference stays as typed.
	agents *agentref.Source
	// currentSessionID is left out of session completion: the picker opens
	// INSIDE a conversation, and referencing that same one is never what the
	// reader meant.
	currentSessionID string

	// skill completions (populated from agent skill store)
	skillNames []string
	skillDescs map[string]string // name → description

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

	logger *logger.Logger
}

func NewInputArea(historyMax int, historyPath string, l *logger.Logger) InputArea {
	ta := textarea.New()
	ta.Placeholder = inputPlaceholder
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

	ia := InputArea{
		textarea:       ta,
		enabled:        true,
		history:        make([]string, 0),
		historyMax:     historyMax,
		histIdx:        -1,
		historyPath:    historyPath,
		pasteThreshold: 5,
		files:          fileindex.New(l),
		logger:         l,
	}
	if historyMax > 0 && historyPath != "" {
		ia.history = ia.loadInputHistoryFile(historyPath, historyMax)
	}
	return ia
}

func (i *InputArea) SetWidth(w int) {
	i.width = w
	i.textarea.SetWidth(w - 2)
}

// SetSkills sets the skill names and descriptions used for slash-command
// completions. Call this after the skill store changes (e.g., after reload).
func (i *InputArea) SetSkills(names []string, descs map[string]string) {
	i.skillNames = names
	i.skillDescs = descs
}

// SetSessionSource supplies the store #s: completion searches and
// references resolve against. Without one the session namespace offers nothing.
func (i *InputArea) SetSessionSource(s *sessionref.Source) { i.sessions = s }

// SessionSource exposes the same store to the expansion path, which needs it at
// send time rather than while typing.
func (i *InputArea) SessionSource() *sessionref.Source { return i.sessions }

// SetAgentSource supplies the nodes #a: completion searches and references
// resolve against. Without one the node namespace offers nothing — and the '#'
// picker still offers it, because the namespace list is what tells the reader the
// spelling exists; it just comes back empty.
func (i *InputArea) SetAgentSource(s *agentref.Source) { i.agents = s }

// AgentSource exposes the same list to the expansion path, which needs it at send
// time rather than while typing.
func (i *InputArea) AgentSource() *agentref.Source { return i.agents }

// SetCurrentSession names the conversation this input belongs to, so session
// completion can leave it out of the list.
func (i *InputArea) SetCurrentSession(id string) { i.currentSessionID = id }

// InsertText inserts text at the cursor, for a caller that has the text to insert rather than a
// keystroke (the paste path inserting an image's @-reference).
//
// It clears the @-completions popup because it bypasses Update, which is what normally keeps the
// popup in step with the value: matches for the old value left on screen would describe something
// that is no longer there.
func (i *InputArea) InsertText(text string) {
	i.textarea.InsertString(text)
	i.clearAtCompletions()
}

// SetMaxHeight dynamically caps the total height of the input area (textarea +
// completions) to at most maxTotal. This allows the parent layout to constrain
// the input area and force internal scrolling in the textarea when screen space
// is tight.
func (i *InputArea) SetMaxHeight(maxTotal int) {
	// Reserve lines for slash-completions and @-completions
	completionLines := 0
	if i.completions != nil {
		completionLines = len(i.completions)
	}
	if i.atMatches != nil {
		completionLines += 1 + len(i.atMatches) // header + matches
	}
	maxTA := max(maxTotal-completionLines, 1)
	i.textarea.MaxHeight = maxTA
}

func (i *InputArea) SetEnabled(enabled bool) {
	i.enabled = enabled
	if enabled {
		i.textarea.Placeholder = inputPlaceholder
		i.histIdx = -1
		i.histScratch = ""
		i.clearAtCompletions()
		i.textarea.Focus()
	} else {
		i.textarea.Placeholder = "Ctrl+C to interrupt"
		i.completions = nil
		i.selectedIdx = 0
		i.clearAtCompletions()
		i.histIdx = -1
		i.histScratch = ""
		i.textarea.Blur()
	}
}

func (i InputArea) Height() int {
	h := i.textarea.Height() + len(i.completions)
	if i.atMatches != nil {
		h += 1 + len(i.atMatches) // header + matches
	}
	return h
}

// 正在上下翻历史（此时不以 / 前缀触发补全，避免与历史键冲突）。
func (i InputArea) browsingHistory() bool { return i.histIdx >= 0 }

// slash 补全列表参与 Tab / 方向键 等（与 browsingHistory 互斥）。
func (i InputArea) completionsOn() bool { return i.histIdx < 0 && len(i.completions) > 0 }

// @-补全列表参与 Tab / 方向键 等（与 browsingHistory 互斥）。文件与 session
// 两个命名空间共用一个列表：前缀决定查哪一边，交互只有一套。
func (i InputArea) atCompletionsOn() bool { return i.histIdx < 0 && i.atMatches != nil }

func (i *InputArea) clearAtCompletions() {
	i.atQuery = ""
	i.atMark = 0
	i.atMatches = nil
	i.atSelectedIdx = 0
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
		i.logger.Info(context.Background(), "input: detected dragged file(s)", "text", text, "path", wrapped)
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

	// Treat the paste as a file drop only when every part resolves to a path
	// on disk; otherwise it is a regular text paste.
	paths := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		resolved := atfile.ResolvePath(cwd, part)
		if !fileutil.Exists(resolved) {
			return "", false
		}
		paths = append(paths, resolved)
	}

	return atfile.RefsForPaths(cwd, paths)
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
			if i.atCompletionsOn() {
				if i.atSelectedIdx < len(i.atMatches)-1 {
					i.atSelectedIdx++
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
			if i.atCompletionsOn() {
				if i.atSelectedIdx > 0 {
					i.atSelectedIdx--
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
			if i.atCompletionsOn() {
				i.applyAtCompletion()
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
			if i.atCompletionsOn() {
				i.clearAtCompletions()
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
			if i.atCompletionsOn() {
				i.applyAtCompletion()
				i.clearAtCompletions()
				i.updateCompletions()
				return i, nil
			}
			if i.completionsOn() {
				i.textarea.SetValue(i.completions[i.selectedIdx].Name)
				i.textarea.CursorEnd()
				i.completions = nil
				i.selectedIdx = 0
				return i, nil
			}
			i.expandPasteBuffer()
			text := strings.TrimSpace(i.textarea.Value())
			if text == "" {
				return i, nil
			}
			i.pushHistoryLine(text)
			i.textarea.Reset()
			i.clearHistoryNav()
			i.clearAtCompletions()
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

func (i *InputArea) applyAtCompletion() {
	if !i.atCompletionsOn() {
		return
	}
	match := i.atMatches[i.atSelectedIdx]

	// Replace "@query" with the row's own reference in the textarea value
	val := i.textarea.Value()
	atPos := atfile.LastRefStart(val)
	if atPos < 0 {
		return
	}

	before := val[:atPos]
	after := val[atPos+len(i.atQuery)+1:]
	// A namespace row OPENS a reference rather than finishing it, so nothing separates it from
	// what the reader types next — and the popup stays up, re-reading the same reference with its
	// new prefix. Inserting a trailing space there would end the reference on the spot and leave
	// the reader with "#a: " and no list.
	sep := " "
	if match.Prefix {
		sep = ""
	}
	newVal := before + match.Ref + sep + after
	i.textarea.SetValue(newVal)
	i.textarea.CursorEnd()
	if match.Prefix {
		i.updateAtCompletions(newVal)
	}
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
		if err := i.saveInputHistoryFile(i.historyPath, i.history); err != nil {
			i.logger.Error(context.Background(), "input history: save", err)
		}
	}
}

func (i *InputArea) updateCompletions() {
	if i.browsingHistory() {
		i.completions = nil
		i.selectedIdx = 0
		i.clearAtCompletions()
		return
	}
	val := i.textarea.Value()
	if strings.HasPrefix(val, "/") {
		i.completions = matchCommands(val)
		// Append /thinking level options when the command is (partially) typed.
		i.appendThinkingCompletions(val)
		// Append /mcp subcommand and profile-name suggestions.
		i.appendMCPCompletions(val)
		// Append matching skill names
		prefix := strings.TrimPrefix(val, "/")
		for _, name := range i.skillNames {
			if strings.HasPrefix(name, prefix) {
				desc := i.skillDescs[name]
				if desc == "" {
					desc = "skill"
				} else if len([]rune(desc)) > maxCompletionDescLen {
					desc = strutil.TruncateFitted(desc, maxCompletionDescLen+1)
				}
				i.completions = append(i.completions, Command{
					Name:        "/" + name,
					Description: desc,
				})
			}
		}
		if i.selectedIdx >= len(i.completions) {
			i.selectedIdx = 0
		}
		i.clearAtCompletions()
	} else {
		i.completions = nil
		i.selectedIdx = 0
		i.updateAtCompletions(val)
	}
}

// appendThinkingCompletions adds /thinking level options to the completion
// list when the input is the /thinking command followed by (or containing) a
// partial level argument. E.g. "/thinking h" suggests "high".
func (i *InputArea) appendThinkingCompletions(val string) {
	rest, ok := strings.CutPrefix(val, "/thinking")
	if !ok {
		return
	}
	// Only suggest levels once the command name is fully typed: either the
	// input is exactly "/thinking" or "/thinking <partial level>".
	if rest != "" && !strings.HasPrefix(rest, " ") {
		return
	}
	partial := strings.TrimSpace(rest)
	for _, level := range cmds.ThinkingLevels {
		if !strings.HasPrefix(level, partial) {
			continue
		}
		i.completions = append(i.completions, Command{
			Name:        "/thinking " + level,
			Description: cmds.ThinkingLevelDescriptions[level],
		})
	}
}

// mcpSubcommands drives /mcp subcommand completion (order = suggestion order).
var mcpSubcommands = []struct {
	name string
	desc string
}{
	{"list", "Open the MCP server overlay"},
	{"toggle", "Enable/disable a server: /mcp toggle <name>"},
	{"reconnect", "Reconnect a server: /mcp reconnect <name>"},
	{"auth", "Authorize an HTTP server: /mcp auth <name>"},
	{"profile", "Switch MCP profile: /mcp profile <name>"},
}

// appendMCPCompletions adds /mcp suggestions to the completion list:
//   - "/mcp" or "/mcp <partial>"    → subcommand names
//   - "/mcp profile <partial>"      → available profile names (mcp.<name>.json)
func (i *InputArea) appendMCPCompletions(val string) {
	rest, ok := strings.CutPrefix(val, "/mcp")
	if !ok {
		return
	}
	// Only suggest once the command name is fully typed.
	if rest != "" && !strings.HasPrefix(rest, " ") {
		return
	}

	trimmed := strings.TrimSpace(rest)
	switch {
	case trimmed == "profile" || strings.HasPrefix(trimmed, "profile "):
		// Typing the profile name → suggest available profiles.
		partial := strings.TrimSpace(strings.TrimPrefix(trimmed, "profile"))
		for _, p := range config.ListMCPProfiles() {
			if strings.HasPrefix(p, partial) {
				i.completions = append(i.completions, Command{
					Name:        "/mcp profile " + p,
					Description: "Switch MCP profile",
				})
			}
		}
	case !strings.Contains(trimmed, " "):
		// Typing a subcommand.
		for _, sub := range mcpSubcommands {
			if strings.HasPrefix(sub.name, trimmed) {
				i.completions = append(i.completions, Command{
					Name:        "/mcp " + sub.name,
					Description: sub.desc,
				})
			}
		}
	}
}

// updateAtCompletions refreshes the reference-completion popup for the reference
// being typed. WHICH mark it is ('@' or '#') and what was typed after it pick the
// namespace, and every namespace produces the same rows, so the popup's keys and
// rendering stay one implementation.
func (i *InputArea) updateAtCompletions(val string) {
	atPos := atfile.LastRefStart(val)
	if atPos < 0 {
		i.clearAtCompletions()
		return
	}

	mark := val[atPos]
	query := val[atPos+1:]
	// Don't trigger if query contains a space (already completed)
	if strings.Contains(query, " ") {
		i.clearAtCompletions()
		return
	}

	// The mark is part of the identity of what is being typed, not just the query: "@a:x" and
	// "#a:x" are different questions with different answers.
	if query == i.atQuery && mark == i.atMark && i.atMatches != nil {
		return // No change, skip search
	}

	rows := i.atRowsFor(mark, query)
	if len(rows) == 0 {
		i.clearAtCompletions()
		return
	}

	i.atQuery = query
	i.atMark = mark
	i.atMatches = rows
	i.atSelectedIdx = 0 // best score, rendered at bottom
}

func (i InputArea) View() string {
	hasCompletions := len(i.completions) > 0
	hasAtMatches := i.atMatches != nil

	if !hasCompletions && !hasAtMatches {
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

	// Render reference completions (best score at the bottom). Every namespace shares the popup;
	// the header is what says which one is being offered.
	if hasAtMatches {
		b.WriteString(dimStyle.Render(fmt.Sprintf("  %s matching %q:", i.atHeader(), i.atQuery)))
		b.WriteString("\n")

		for idx := len(i.atMatches) - 1; idx >= 0; idx-- {
			m := i.atMatches[idx]
			icon := "  "
			switch {
			case m.IsDir:
				icon = " D"
			case m.Prefix:
				icon = " #"
			case m.Agent:
				icon = " N"
			case m.Session:
				icon = " S"
			}
			line := fmt.Sprintf("%s %s", icon, m.Label)
			if m.Note != "" {
				line += "  " + m.Note
			}
			if idx == i.atSelectedIdx {
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

// atHeader names the namespace the popup is currently offering. It is derived
// from what is being typed rather than stored beside the rows, so the header cannot
// disagree with what was searched.
func (i InputArea) atHeader() string {
	if i.atMark != atfile.HashRefMark {
		return "@ 文件"
	}
	switch ns, _ := atfile.HashQueryNamespace(i.atQuery); ns {
	case atfile.HashNamespaceAgents:
		return "# 节点"
	case atfile.HashNamespaceSessions:
		return "# 会话"
	default:
		return "# 引用"
	}
}
