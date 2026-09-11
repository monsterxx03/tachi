// Package linediff turns two text fragments into render-ready diff hunks.
//
// It is deliberately small: the fragments it diffs are the ones a tool call
// carried (an EditFile's old_string/new_string, a WriteFile's content), not whole
// files, so a line diff does not need a general algorithm. The approach is
// common-prefix / common-suffix trimming — the middle becomes a delete run followed
// by an add run — which is predictable, allocation-light and unit-testable. Its one
// visible consequence is that a multi-occurrence replacement (replace_all) reads as
// one block against another rather than as paired changes; callers that care label
// it in the UI.
//
// Line numbers are 1-based WITHIN THE FRAGMENT (and 0 on the side where a line does
// not exist). They are not file coordinates: a fragment has no position, which is
// why the desktop renderer does not show them (they are kept for locating a comment
// on a change later).
package linediff

import "strings"

// Kind classifies one hunk line.
type Kind string

const (
	// KindContext is a line both fragments share.
	KindContext Kind = "context"
	// KindAdd is a line only the new fragment has.
	KindAdd Kind = "add"
	// KindDel is a line only the old fragment has.
	KindDel Kind = "del"
)

// Hunk is one render-ready line of a fragment diff. The JSON tags matter: this type
// crosses into the desktop frontend (via FileChangeVO), where every other field is
// camelCase.
type Hunk struct {
	Kind    Kind   `json:"kind"`
	OldLine int    `json:"oldLine"`
	NewLine int    `json:"newLine"`
	Text    string `json:"text"`
}

// Fragments diffs two text fragments line by line. Identical inputs yield no hunks
// (not a list of context lines): "nothing changed" is a fact the renderer needs, and
// an empty result says it without the reader having to count.
func Fragments(oldText, newText string) []Hunk {
	if oldText == newText {
		return nil
	}
	oldLines, newLines := splitLines(oldText), splitLines(newText)
	oldLines, newLines = trimTerminator(oldLines, newLines)

	// Common prefix, then common suffix of what is left.
	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(oldLines)-prefix && suffix < len(newLines)-prefix &&
		oldLines[len(oldLines)-1-suffix] == newLines[len(newLines)-1-suffix] {
		suffix++
	}

	hunks := make([]Hunk, 0, len(oldLines)+len(newLines))
	for i := range prefix {
		hunks = append(hunks, Hunk{Kind: KindContext, OldLine: i + 1, NewLine: i + 1, Text: oldLines[i]})
	}
	for i := prefix; i < len(oldLines)-suffix; i++ {
		hunks = append(hunks, Hunk{Kind: KindDel, OldLine: i + 1, Text: oldLines[i]})
	}
	for i := prefix; i < len(newLines)-suffix; i++ {
		hunks = append(hunks, Hunk{Kind: KindAdd, NewLine: i + 1, Text: newLines[i]})
	}
	for i := range suffix {
		hunks = append(hunks, Hunk{
			Kind:    KindContext,
			OldLine: len(oldLines) - suffix + i + 1,
			NewLine: len(newLines) - suffix + i + 1,
			Text:    newLines[len(newLines)-suffix+i],
		})
	}
	return hunks
}

// Counts returns the added and removed line totals of a hunk list. They are FRAGMENT
// line counts, not git numstat: a multi-line fragment with one changed line reports
// more than git would.
func Counts(hunks []Hunk) (added, removed int) {
	for _, h := range hunks {
		switch h.Kind {
		case KindAdd:
			added++
		case KindDel:
			removed++
		}
	}
	return added, removed
}

// splitLines splits a fragment into lines. An empty fragment has no lines at all
// (so creating a file does not render a phantom empty line being deleted).
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// trimTerminator removes the empty element a trailing newline leaves behind, in the
// cases where it is a terminator rather than a line of its own:
//
//   - both sides end with a newline → it is the same terminator twice;
//   - one side has no content at all → a create (or a full delete) must not render a
//     blank line being added (or removed).
//
// When only ONE of two non-empty sides ends with a newline the element stays: that
// difference is real, and dropping it would hide an edit whose entire point was to
// add or remove the final newline.
func trimTerminator(oldLines, newLines []string) ([]string, []string) {
	endsEmpty := func(lines []string) bool {
		return len(lines) > 0 && lines[len(lines)-1] == ""
	}
	oldEmpty, newEmpty := endsEmpty(oldLines), endsEmpty(newLines)
	switch {
	case oldEmpty && newEmpty:
		return oldLines[:len(oldLines)-1], newLines[:len(newLines)-1]
	case oldEmpty && len(newLines) == 0:
		return oldLines[:len(oldLines)-1], newLines
	case newEmpty && len(oldLines) == 0:
		return oldLines, newLines[:len(newLines)-1]
	default:
		return oldLines, newLines
	}
}
