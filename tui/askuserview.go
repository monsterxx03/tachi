package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/monsterxx03/tachi/agent/tools"
)

// AskUserView handles the AskUserQuestion tool interaction
type AskUserView struct {
	questions   []tools.Question
	selected    map[int]map[int]bool // question index -> set of selected option indices
	curQuestion int
	cursorPos   int
	width       int
}

func NewAskUserView(questions []tools.Question, width int) *AskUserView {
	return &AskUserView{
		questions: questions,
		selected:  make(map[int]map[int]bool),
		width:     width,
	}
}

// Height returns the number of lines this view will render.
func (v *AskUserView) Height() int {
	if len(v.questions) == 0 {
		return 0
	}
	q := v.questions[v.curQuestion]
	// hint(1) + progress(1) + blank(1) + question(1) + header(1) + options + blank(1) + summary header(1) + answered questions
	h := 6 + len(q.Options) + 1
	for i := 0; i < len(v.questions); i++ {
		if len(v.selected[i]) > 0 {
			h++
		}
	}
	return h
}

func (v *AskUserView) HandleKey(key string) (submit bool, cancelled bool) {
	q := v.questions[v.curQuestion]

	switch key {
	case "up", "k":
		if v.cursorPos > 0 {
			v.cursorPos--
		}
	case "down", "j":
		if v.cursorPos < len(q.Options)-1 {
			v.cursorPos++
		}
	case "left", "backspace", "h":
		if v.curQuestion > 0 {
			v.curQuestion--
			v.cursorPos = 0
		}
	case "space":
		v.toggleOption(v.cursorPos)
	case "enter":
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
		if n, err := strconv.Atoi(key); err == nil && n >= 1 && n <= 4 {
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
		return false, false
	}
	return true, false
}

func (v *AskUserView) GetAnswers() map[string]string {
	answers := make(map[string]string)
	for i, q := range v.questions {
		if labels := v.selectedLabels(i); len(labels) > 0 {
			answers[q.Question] = strings.Join(labels, ", ")
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

	// Hint line
	hint := "↑↓ navigate  "
	if q.MultiSelect {
		hint += "Space toggle  Enter confirm  "
	} else {
		hint += "1-4/Enter select  "
	}
	if v.curQuestion > 0 {
		hint += "← back  "
	}
	hint += "Esc cancel"
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
		isCursor := i == v.cursorPos

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

	// Summary of answered questions
	b.WriteString("\n")
	var hasSummary bool
	for i := range v.questions {
		labels := v.selectedLabels(i)
		if len(labels) == 0 {
			continue
		}
		if !hasSummary {
			b.WriteString(dimStyle.Render("Answers:") + "\n")
			hasSummary = true
		}
		b.WriteString(dimStyle.Render(fmt.Sprintf("  %s → %s", v.questions[i].Header, strings.Join(labels, ", "))) + "\n")
	}

	return b.String()
}
