package tui

import (
	"fmt"
	"strings"

	"github.com/monsterxx03/tachi/tools"
)

// AskUserView handles the AskUserQuestion tool interaction
type AskUserView struct {
	questions   []tools.Question
	selected     map[int]int // question index -> selected option index (-1 = not selected)
	curQuestion int
	cursorPos   int // cursor position within options
	width       int
	height      int
}

func NewAskUserView(questions []tools.Question) *AskUserView {
	selected := make(map[int]int)
	for i := range questions {
		selected[i] = -1 // -1 means no selection
	}
	return &AskUserView{
		questions:   questions,
		selected:   selected,
		curQuestion: 0,
		cursorPos:   0,
	}
}

func (v *AskUserView) SetSize(width, height int) {
	v.width = width
	v.height = height
}

// HandleKey handles keyboard input for the AskUserView
// Returns: submit (true to submit answers, false to cancel)
func (v *AskUserView) HandleKey(key string) (submit bool, cancelled bool) {
	switch key {
	case "up", "k":
		if v.cursorPos > 0 {
			v.cursorPos--
		}
	case "down", "j":
		if v.cursorPos < len(v.questions[v.curQuestion].Options)-1 {
			v.cursorPos++
		}
	case "enter":
		// Select current option and either move to next or submit
		if v.cursorPos >= 0 && v.cursorPos < len(v.questions[v.curQuestion].Options) {
			v.selected[v.curQuestion] = v.cursorPos
		}
		if v.curQuestion < len(v.questions)-1 {
			v.curQuestion++
			v.cursorPos = 0
		} else {
			return true, false
		}
	case "esc":
		return false, true
	}
	// Number keys 1-4 to select
	for i := 1; i <= 4; i++ {
		if key == fmt.Sprintf("%d", i) {
			optIdx := i - 1
			if optIdx >= 0 && optIdx < len(v.questions[v.curQuestion].Options) {
				v.selected[v.curQuestion] = optIdx
				v.cursorPos = optIdx
				// Move to next question or auto-submit if last
				if v.curQuestion < len(v.questions)-1 {
					v.curQuestion++
					v.cursorPos = 0
				} else {
					return true, false
				}
			}
		}
	}
	return false, false
}

// GetAnswers returns the selected answers
func (v *AskUserView) GetAnswers() map[string]string {
	answers := make(map[string]string)
	for qIdx, optIdx := range v.selected {
		if optIdx >= 0 && qIdx < len(v.questions) && optIdx < len(v.questions[qIdx].Options) {
			answers[v.questions[qIdx].Question] = v.questions[qIdx].Options[optIdx].Label
		}
	}
	return answers
}

// Render renders the AskUserView
func (v *AskUserView) Render() string {
	if len(v.questions) == 0 {
		return ""
	}

	var b strings.Builder

	// Instructions
	b.WriteString(dimStyle.Render("Use ↑↓ to navigate, number to select, Enter to submit, Esc to cancel") + "\n")
	b.WriteString(dimStyle.Render(fmt.Sprintf("Question %d of %d:", v.curQuestion+1, len(v.questions))) + "\n")
	b.WriteString("\n")

	q := v.questions[v.curQuestion]
	b.WriteString(boldStyle.Render(q.Question) + "\n")
	b.WriteString(fmt.Sprintf("(%s)\n", q.Header))

	// Options
	for i, opt := range q.Options {
		prefix := "  "
		if i == v.cursorPos {
			prefix = " >"
		}
		selected := ""
		if v.selected[v.curQuestion] == i {
			selected = " [x]"
		}
		b.WriteString(fmt.Sprintf("%s%d. %s%s - %s\n", prefix, i+1, opt.Label, selected, opt.Description))
	}

	// Show summary of all answers so far at the bottom
	b.WriteString("\n")
	b.WriteString(dimStyle.Render("Your answers:"))
	for qIdx, optIdx := range v.selected {
		if optIdx >= 0 && qIdx < len(v.questions) {
			q := v.questions[qIdx]
			if optIdx < len(q.Options) {
				b.WriteString(fmt.Sprintf(" %s: %s;", q.Question, q.Options[optIdx].Label))
			}
		}
	}
	b.WriteString("\n")

	return b.String()
}