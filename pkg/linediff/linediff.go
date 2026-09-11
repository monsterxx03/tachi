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

import (
	"strconv"
	"strings"
)

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

// FileDiff is one file's changes in a unified diff — the shape `git diff` emits, and
// what the desktop's changes panel renders. Unlike a fragment diff (Fragments), the
// line numbers here are REAL file coordinates: that is the whole point of parsing
// git's output instead of re-diffing two fragments ourselves.
type FileDiff struct {
	// Path is the post-image path (the file as it is now), relative to the repo.
	Path string
	// OldPath is the pre-image path of a rename/copy ("" otherwise).
	OldPath string
	// Created/Deleted mark the /dev/null sides: the whole file appeared or went away.
	Created bool
	Deleted bool
	// Binary marks a file git will not diff as text (no hunks).
	Binary bool
	// Hunks carry real line numbers; empty for a binary or a mode-only change.
	Hunks []Hunk
	// Added/Removed count the hunk lines.
	Added   int
	Removed int
}

// ParseUnified parses unified diff text (what `git diff` prints, with or without
// -U<n>) into per-file changes. Anything that is not a file or a hunk header is
// ignored, so mode changes, index lines and "\ No newline at end of file" markers
// pass through harmlessly. An empty or hunk-less input yields nil.
//
// Paths in git's output are QUOTED when they contain spaces or non-ASCII bytes
// (core.quotePath), so the header values go through unquoting — a diff of a file
// named "报告 2026.md" arrives as a C-escaped string, and a parser that skipped this
// would show the escapes to the user.
func ParseUnified(diffText string) []FileDiff {
	var files []FileDiff
	var cur *FileDiff
	var oldLine, newLine int
	inHunk := false

	flush := func() {
		if cur == nil {
			return
		}
		// A deleted file's post-image is /dev/null, but the panel still needs a name:
		// report the file under its pre-image path.
		if cur.Path == "" {
			cur.Path = cur.OldPath
		}
		cur.Added, cur.Removed = Counts(cur.Hunks)
		files = append(files, *cur)
		cur = nil
	}

	for line := range strings.SplitSeq(diffText, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			cur = &FileDiff{}
			inHunk = false

		case cur == nil:
			// Anything before the first file header is noise.

		case strings.HasPrefix(line, "new file mode"):
			cur.Created = true
		case strings.HasPrefix(line, "deleted file mode"):
			cur.Deleted = true
		case strings.HasPrefix(line, "rename from "), strings.HasPrefix(line, "copy from "):
			cur.OldPath = unquotePath(strings.SplitN(line, " ", 3)[2])
		case strings.HasPrefix(line, "Binary files ") || strings.HasPrefix(line, "GIT binary patch"):
			cur.Binary = true

		case strings.HasPrefix(line, "--- "):
			// The pre-image path. /dev/null means the file did not exist before.
			if p := diffPath(line[4:], "a/"); p == "/dev/null" {
				cur.Created = true
			} else if cur.OldPath == "" {
				cur.OldPath = p
			}
		case strings.HasPrefix(line, "+++ "):
			p := diffPath(line[4:], "b/")
			if p == "/dev/null" {
				cur.Deleted = true
			} else {
				cur.Path = p
			}

		case strings.HasPrefix(line, "@@"):
			oldLine, newLine = parseHunkHeader(line)
			inHunk = true

		case inHunk:
			if line == "" {
				// The trailing newline of the diff, or the blank separator between
				// file sections — never a line of content (a real empty context line
				// carries a space prefix).
				continue
			}
			switch line[0] {
			case ' ':
				cur.Hunks = append(cur.Hunks, Hunk{Kind: KindContext, OldLine: oldLine, NewLine: newLine, Text: line[1:]})
				oldLine++
				newLine++
			case '-':
				cur.Hunks = append(cur.Hunks, Hunk{Kind: KindDel, OldLine: oldLine, Text: line[1:]})
				oldLine++
			case '+':
				cur.Hunks = append(cur.Hunks, Hunk{Kind: KindAdd, NewLine: newLine, Text: line[1:]})
				newLine++
			}
		}
	}
	flush()
	return files
}

// diffPath extracts a path from a "--- a/x" / "+++ b/x" header value: unquote it,
// then drop the a/ or b/ prefix git puts on. A path that does not carry the prefix
// (some tools omit it) is returned as it is.
func diffPath(value, prefix string) string {
	// git appends a tab-separated timestamp in some (non-git) unified diffs.
	if i := strings.IndexByte(value, '\t'); i >= 0 {
		value = value[:i]
	}
	p := unquotePath(value)
	if p != "/dev/null" && strings.HasPrefix(p, prefix) {
		p = p[len(prefix):]
	}
	return p
}

// unquotePath undoes git's C-style quoting of a path.
func unquotePath(value string) string {
	value = strings.TrimSpace(value)
	if len(value) < 2 || value[0] != '"' {
		return value
	}
	if unquoted, err := strconv.Unquote(value); err == nil {
		return unquoted
	}
	return value
}

// parseHunkHeader reads the starting line numbers out of "@@ -12,3 +12,4 @@ ctx".
func parseHunkHeader(line string) (oldLine, newLine int) {
	rest := strings.TrimPrefix(line, "@@")
	if i := strings.Index(rest, "@@"); i >= 0 {
		rest = rest[:i]
	}
	for _, field := range strings.Fields(rest) {
		start := strings.TrimPrefix(field, "-")
		start = strings.TrimPrefix(start, "+")
		if i := strings.IndexByte(start, ','); i >= 0 {
			start = start[:i]
		}
		n, err := strconv.Atoi(start)
		if err != nil {
			continue
		}
		if field[0] == '-' {
			oldLine = n
		} else {
			newLine = n
		}
	}
	return oldLine, newLine
}
