package tui

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/monsterxx03/tachi/agent/tools"
)

// AskUserView handles the AskUserQuestion tool interaction
type AskUserView struct {
	questions   []tools.Question
	selected    map[int]map[int]bool // question index -> set of selected option indices
	curQuestion int
	cursorPos   int // index into options, or len(options) for "Other"
	width       int

	// "Other" free-text input state (per-question)
	otherSelected bool           // whether "Other" option is toggled/selected
	otherTexts    map[int]string // question index -> custom text
	otherEditing  bool           // currently typing "Other" text?
	otherCursor   int            // cursor position within otherText
}

func NewAskUserView(questions []tools.Question, width int) *AskUserView {
	v := &AskUserView{
		questions:  questions,
		selected:   make(map[int]map[int]bool),
		otherTexts: make(map[int]string),
		width:      width,
	}
	// If the first question has no pre-defined options, drop straight into
	// free-text editing mode.
	v.autoEnterFreeText()
	return v
}

// autoEnterFreeText jumps directly into free-text editing mode when the
// current question has no pre-defined options.
func (v *AskUserView) autoEnterFreeText() {
	if v.curQuestion >= len(v.questions) {
		return
	}
	q := v.questions[v.curQuestion]
	if len(q.Options) == 0 {
		v.otherSelected = true
		v.otherEditing = true
		v.otherCursor = utf8.RuneCountInString(v.otherTexts[v.curQuestion])
	}
}

// Height returns the number of lines this view will render.
func (v *AskUserView) Height() int {
	if len(v.questions) == 0 {
		return 0
	}
	q := v.questions[v.curQuestion]
	// hint(1) + progress(1) + blank(1) + question(1) + header(1) + options + "Other" row +
	// blank(1) + summary header(1) + answered questions
	h := 6 + len(q.Options) + 1 + 1 // +1 for "Other" row
	for i := range v.questions {
		if len(v.selected[i]) > 0 || v.otherTexts[i] != "" {
			h++
		}
	}
	// "Other" text is now rendered inline on the option row itself,
	// so no extra line needed for editing.
	return h
}

func (v *AskUserView) HandleKey(key string) (submit bool, cancelled bool) {
	// ---- "Other" text editing mode ----
	if v.otherEditing {
		switch key {
		case "enter":
			// Confirm "Other" text
			v.otherEditing = false
			q := v.questions[v.curQuestion]
			if q.MultiSelect {
				// Stay on current question
				return false, false
			}
			// Single select: advance
			return v.advance()
		case "esc":
			// Cancel editing, revert "Other"
			v.otherEditing = false
			v.otherSelected = false
			delete(v.otherTexts, v.curQuestion)
			return false, false
		case "backspace":
			if v.otherCursor > 0 {
				text := v.otherTexts[v.curQuestion]
				runeIdx := v.otherCursor - 1
				runes := []rune(text)
				if runeIdx < len(runes) {
					v.otherTexts[v.curQuestion] = string(runes[:runeIdx]) + string(runes[runeIdx+1:])
				}
				v.otherCursor--
			}
			return false, false
		case "left":
			if v.otherCursor > 0 {
				v.otherCursor--
			}
			return false, false
		case "right":
			text := v.otherTexts[v.curQuestion]
			if v.otherCursor < utf8.RuneCountInString(text) {
				v.otherCursor++
			}
			return false, false
		case "up", "down", "tab", "home", "end", "pgup", "pgdown":
			// Ignore navigation keys in text editing mode.
			return false, false
		case "space":
			key = " "
			fallthrough
		default:
			// Accept printable text (including multi-byte UTF-8 like Chinese).
			// Reject key combos (e.g. "ctrl+c", "shift+enter") which contain "+".
			if strings.Contains(key, "+") || utf8.RuneCountInString(key) == 0 {
				return false, false
			}
			text := v.otherTexts[v.curQuestion]
			runes := []rune(text)
			runeIdx := min(v.otherCursor, len(runes))
			insertRunes := []rune(key)
			newRunes := make([]rune, 0, len(runes)+len(insertRunes))
			newRunes = append(newRunes, runes[:runeIdx]...)
			newRunes = append(newRunes, insertRunes...)
			newRunes = append(newRunes, runes[runeIdx:]...)
			v.otherTexts[v.curQuestion] = string(newRunes)
			v.otherCursor += len(insertRunes)
			return false, false
		}
	}

	// ---- Option selection mode ----
	q := v.questions[v.curQuestion]
	otherIdx := len(q.Options) // cursor position of "Other"

	switch key {
	case "up", "k":
		if v.cursorPos > 0 {
			v.cursorPos--
		}
	case "down", "j":
		if v.cursorPos < otherIdx {
			v.cursorPos++
		}
	case "left", "backspace", "h":
		if v.curQuestion > 0 {
			v.curQuestion--
			v.cursorPos = 0
			v.restoreOtherState()
			v.autoEnterFreeText()
		}
	case "space":
		if v.cursorPos == otherIdx {
			v.toggleOther()
		} else {
			v.toggleOption(v.cursorPos)
		}
	case "tab":
		// Jump directly to "Other" free-text editing.
		v.cursorPos = otherIdx
		return v.selectOther()
	case "enter":
		if v.cursorPos == otherIdx {
			// "Other" is edited via Tab; Enter is a no-op here.
			return false, false
		}
		if q.MultiSelect {
			if len(v.selected[v.curQuestion]) == 0 {
				return false, false
			}
			return v.advance()
		}
		// single select: select current + advance
		v.selectSingle(v.cursorPos)
		return v.advance()
	case "esc":
		return false, true
	default:
		// Number shortcuts: 1-4 for options, 0 for "Other"
		if n, err := strconv.Atoi(key); err == nil {
			if n == 0 {
				// "Other" shortcut
				v.cursorPos = otherIdx
				return v.selectOther()
			}
			if n >= 1 && n <= 4 {
				optIdx := n - 1
				if optIdx < len(q.Options) {
					if q.MultiSelect {
						v.toggleOption(optIdx)
						v.cursorPos = optIdx
					} else {
						v.selectSingle(optIdx)
						v.cursorPos = optIdx
						return v.advance()
					}
				}
			}
		}
	}
	return false, false
}

func (v *AskUserView) ensureSelected(qIdx int) map[int]bool {
	sel := v.selected[qIdx]
	if sel == nil {
		sel = make(map[int]bool)
		v.selected[qIdx] = sel
	}
	return sel
}

func (v *AskUserView) toggleOption(idx int) {
	sel := v.ensureSelected(v.curQuestion)
	if sel[idx] {
		delete(sel, idx)
	} else {
		sel[idx] = true
	}
}

func (v *AskUserView) selectSingle(idx int) {
	v.selected[v.curQuestion] = map[int]bool{idx: true}
}

// toggleOther toggles the "Other" option for multi-select questions.
func (v *AskUserView) toggleOther() {
	if v.otherSelected {
		v.otherSelected = false
		delete(v.otherTexts, v.curQuestion)
	} else {
		v.otherSelected = true
		v.otherEditing = true
		v.otherCursor = utf8.RuneCountInString(v.otherTexts[v.curQuestion])
	}
}

// selectOther handles selecting "Other" for single-select questions.
func (v *AskUserView) selectOther() (submit bool, cancelled bool) {
	q := v.questions[v.curQuestion]
	if q.MultiSelect {
		v.toggleOther()
		return false, false
	}
	// Single select: clear previous selections, mark "Other", enter editing
	v.selected[v.curQuestion] = nil
	v.otherSelected = true
	v.otherEditing = true
	v.otherCursor = utf8.RuneCountInString(v.otherTexts[v.curQuestion])
	return false, false
}

// restoreOtherState restores the "Other" selection state when navigating back
// to a previously answered question.
func (v *AskUserView) restoreOtherState() {
	if text, ok := v.otherTexts[v.curQuestion]; ok && text != "" {
		v.otherSelected = true
	}
}

func (v *AskUserView) selectedLabels(qIdx int) []string {
	q := v.questions[qIdx]
	sel := v.selected[qIdx]
	var labels []string
	for i, opt := range q.Options {
		if sel[i] {
			labels = append(labels, opt.Label)
		}
	}
	return labels
}

func (v *AskUserView) advance() (submit bool, cancelled bool) {
	if v.curQuestion < len(v.questions)-1 {
		v.curQuestion++
		v.cursorPos = 0
		v.otherSelected = false
		v.otherEditing = false
		v.restoreOtherState()
		v.autoEnterFreeText()
		return false, false
	}
	return true, false
}

func (v *AskUserView) GetAnswers() map[string]string {
	answers := make(map[string]string)
	for i, q := range v.questions {
		var parts []string
		if labels := v.selectedLabels(i); len(labels) > 0 {
			parts = append(parts, labels...)
		}
		if text, ok := v.otherTexts[i]; ok && text != "" {
			parts = append(parts, text)
		}
		if len(parts) > 0 {
			answers[q.Question] = strings.Join(parts, ", ")
		}
	}
	return answers
}

func (v *AskUserView) Render() string {
	if len(v.questions) == 0 {
		return ""
	}

	var b strings.Builder
	q := v.questions[v.curQuestion]
	w := v.width
	otherIdx := len(q.Options)

	// Hint line
	hint := "↑↓ navigate  "
	if v.otherEditing {
		hint = "Type your answer  Enter confirm  Esc cancel"
	} else if len(q.Options) == 0 {
		hint = "Type your answer  Enter confirm  Esc cancel"
	} else if q.MultiSelect {
		hint += "Space toggle  Enter confirm  Tab free input  "
	} else {
		hint += "1-4/Enter select  Tab free input  "
	}
	if v.curQuestion > 0 && !v.otherEditing {
		hint += "← back  "
	}
	if !v.otherEditing {
		hint += "Esc cancel"
	}
	b.WriteString(dimStyle.Render(hint) + "\n")

	// Progress
	progress := fmt.Sprintf("Question %d/%d", v.curQuestion+1, len(v.questions))
	b.WriteString(confirmStyle.Render(progress) + "\n")
	b.WriteString("\n")

	// Question text
	b.WriteString(boldStyle.Render(q.Question) + "\n")

	// Header tag
	b.WriteString(toolCallStyle.Render("["+q.Header+"]") + "\n")

	// Options
	for i, opt := range q.Options {
		isSelected := v.selected[v.curQuestion][i]
		isCursor := i == v.cursorPos && !v.otherEditing

		var marker string
		if q.MultiSelect {
			if isSelected {
				marker = "[x] "
			} else {
				marker = "[ ] "
			}
		} else {
			if isSelected {
				marker = " ● "
			} else {
				marker = " ○ "
			}
		}

		line := fmt.Sprintf(" %s%d. %s — %s", marker, i+1, opt.Label, opt.Description)
		if isCursor {
			b.WriteString(completionSelectedStyle.Width(w).Render(line))
		} else {
			b.WriteString(completionNormalStyle.Width(w).Render(line))
		}
		b.WriteString("\n")
	}

	// "Other" option line — becomes inline input box when editing
	{
		isCursor := v.cursorPos == otherIdx && !v.otherEditing
		var marker string
		if q.MultiSelect {
			if v.otherSelected {
				marker = "[x] "
			} else {
				marker = "[ ] "
			}
		} else {
			if v.otherSelected {
				marker = " ● "
			} else {
				marker = " ○ "
			}
		}

		if v.otherEditing {
			// Inline editing: the "Other" row itself becomes the input box.
			// Cursor is rendered inline with styling so it visually replaces the
			// static label. No extra line below.
			text := v.otherTexts[v.curQuestion]
			cursor := v.otherCursor
			runes := []rune(text)
			before := string(runes[:cursor])
			at := "_"
			if cursor < len(runes) {
				at = string(runes[cursor])
			}
			after := ""
			if cursor+1 < len(runes) {
				after = string(runes[cursor+1:])
			}
			line := fmt.Sprintf(" %s0. %s%s%s", marker, before,
				toolCallStyle.Render(at), after)
			b.WriteString(completionSelectedStyle.Width(w).Render(line) + "\n")
		} else {
			label := "Tab 自由输入"
			if text, ok := v.otherTexts[v.curQuestion]; ok && text != "" {
				label = fmt.Sprintf("Tab: %s", text)
			}
			line := fmt.Sprintf(" %s0. %s", marker, label)
			if isCursor {
				b.WriteString(completionSelectedStyle.Width(w).Render(line))
			} else {
				b.WriteString(completionNormalStyle.Width(w).Render(line))
			}
			b.WriteString("\n")
		}
	}

	// Summary of answered questions
	b.WriteString("\n")
	var hasSummary bool
	for i := range v.questions {
		labels := v.selectedLabels(i)
		otherText := v.otherTexts[i]
		if len(labels) == 0 && otherText == "" {
			continue
		}
		if !hasSummary {
			b.WriteString(dimStyle.Render("Answers:") + "\n")
			hasSummary = true
		}
		var answerParts []string
		answerParts = append(answerParts, labels...)
		if otherText != "" {
			answerParts = append(answerParts, otherText)
		}
		b.WriteString(dimStyle.Render(fmt.Sprintf("  %s → %s", v.questions[i].Header, strings.Join(answerParts, ", "))) + "\n")
	}

	return b.String()
}
