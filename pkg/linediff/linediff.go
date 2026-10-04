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

// DiffContext diffs two WHOLE texts into the hunks a reader wants when the question is "what
// changed in this file": every change, `keep` lines of context around it, and the unchanged
// stretches between changes dropped.
//
// Fragments is the other tool and answers a different question. It trims only the common PREFIX and
// SUFFIX, which is exactly right for what it was written for — one tool call's old block against
// its new block, where the change IS the content and the two ends are what stayed — and wrong for a
// file with two changes in it: no common prefix, no common suffix, and the whole file comes back as
// one delete run against one add run (that is the shape the 多处替换，整段对照 note labels). The
// settings page's preview of a generated config.yaml is the case that needs the real thing.
//
// The alignment is a longest-common-subsequence table: O(n·m) time and memory, which is chosen for
// the input this is for (a config file, a few hundred lines). A caller with an unbounded file wants
// Myers' algorithm, not this one.
//
// Within one change region the DELETIONS come first and the insertions after them: the 左右 layout
// pairs a deletion run with the insertion run that follows (see splitRows in the frontend), so an
// interleaved region would pair the wrong lines side by side.
func DiffContext(oldText, newText string, keep int) []Hunk {
	oldLines, newLines := splitLines(oldText), splitLines(newText)
	oldLines, newLines = trimTerminator(oldLines, newLines)
	n, m := len(oldLines), len(newLines)

	// lcs[i][j] is the length of the longest common subsequence of oldLines[i:] and newLines[j:] —
	// filled from the end so the walk below can ask "which side can still match more?".
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if oldLines[i] == newLines[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}

	raw := make([]Hunk, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case oldLines[i] == newLines[j]:
			raw = append(raw, Hunk{Kind: KindContext, OldLine: i + 1, NewLine: j + 1, Text: oldLines[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			raw = append(raw, Hunk{Kind: KindDel, OldLine: i + 1, Text: oldLines[i]})
			i++
		default:
			raw = append(raw, Hunk{Kind: KindAdd, NewLine: j + 1, Text: newLines[j]})
			j++
		}
	}
	for ; i < n; i++ {
		raw = append(raw, Hunk{Kind: KindDel, OldLine: i + 1, Text: oldLines[i]})
	}
	for ; j < m; j++ {
		raw = append(raw, Hunk{Kind: KindAdd, NewLine: j + 1, Text: newLines[j]})
	}
	return TrimContext(groupRegions(raw), keep)
}

// groupRegions rewrites every change region as all its deletions followed by all its insertions (see
// DiffContext's last paragraph). A region is a maximal run of non-context hunks, so the order is
// settled without touching where anything sits: each hunk keeps its line number.
func groupRegions(hunks []Hunk) []Hunk {
	out := make([]Hunk, 0, len(hunks))
	for i := 0; i < len(hunks); {
		if hunks[i].Kind == KindContext {
			out = append(out, hunks[i])
			i++
			continue
		}
		j := i
		for j < len(hunks) && hunks[j].Kind != KindContext {
			j++
		}
		for _, h := range hunks[i:j] {
			if h.Kind == KindDel {
				out = append(out, h)
			}
		}
		for _, h := range hunks[i:j] {
			if h.Kind == KindAdd {
				out = append(out, h)
			}
		}
		i = j
	}
	return out
}

// TrimContext keeps only `keep` lines of context on each side of every change, so a caller that
// wants a DIFF (rather than the whole text) can show what changed instead of a wall with the
// change buried in it. It is the equivalent of `git diff -U<keep>`, and DiffContext applies it to
// everything it produces.
//
// Fragments returns the entire text — every unchanged line is a context hunk — because a tool
// card's fragment IS the thing the reader is looking at, and it is small. A caller diffing two
// whole texts is the other case: there the change is what the reader asked for, and the context is
// what tells them where they are.
//
// Unchanged RUNS are what get shortened, never a change: a run keeps its last `keep` lines when a
// change precedes it and its first `keep` when one follows, and the middle is dropped. Dropping
// lines means the caller renders a jump — the line numbers say so, and that is the same signal a
// `git diff` reader reads — so nothing here invents a marker for it.
func TrimContext(hunks []Hunk, keep int) []Hunk {
	if keep < 0 {
		keep = 0
	}
	out := make([]Hunk, 0, len(hunks))
	for i := 0; i < len(hunks); {
		if hunks[i].Kind != KindContext {
			out = append(out, hunks[i])
			i++
			continue
		}
		j := i
		for j < len(hunks) && hunks[j].Kind == KindContext {
			j++
		}
		run := hunks[i:j]
		head, tail := 0, 0
		if i > 0 {
			head = keep // a change sits above this run
		}
		if j < len(hunks) {
			tail = keep // …and one below it
		}
		if head+tail >= len(run) {
			out = append(out, run...)
		} else {
			out = append(out, run[:head]...)
			out = append(out, run[len(run)-tail:]...)
		}
		i = j
	}
	return out
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

// GitPrefixArgs are the flags that pin a `git diff`'s path prefixes to the a/ and b/
// this parser strips. Pass them to EVERY `git diff` whose output feeds ParseUnified.
//
// `git diff` honours the user's `diff.mnemonicprefix` (a common setting): the prefixes
// become i/ and w/ (index/worktree), c/ and w/ (commit/worktree) or c/ and i/, none of
// which diffPath knows — so the header would keep its letter and the panel would show
// "w/src/main.go". The prefixes are overridden rather than the parser loosened, because
// a bare path may legitimately begin with "b/" (a repo can have a b/ directory), and
// stripping that would be silent corruption.
//
// A fresh slice each call, so a caller may append without sharing storage.
func GitPrefixArgs() []string {
	return []string{"--src-prefix=a/", "--dst-prefix=b/"}
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
