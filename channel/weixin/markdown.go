package weixin

import (
	"strings"
)

// filterMarkdown strips unsupported markdown syntax from text for WeChat compatibility.
//
// Constructs PASSED THROUGH (markers preserved):
//   - Code fences (```) with content
//   - Inline code (`)
//   - Tables (|...|)
//   - Horizontal rules (---, ***, ___)
//   - Bold (**)
//   - Italic (*, _) wrapping non-CJK content
//   - Bold-italic (***, ___) wrapping non-CJK content
//   - Strikethrough (~~)
//   - Blockquote (>)
//   - Headings H1-H4 (#, ##, ###, ####)
//   - Lists (-, *, 1.)
//
// Constructs FILTERED (markers stripped, content kept):
//   - Headings H5/H6 (#####, ######) — prefix removed
//   - Italic (*, _) wrapping CJK content — markers removed
//   - Bold-italic (***, ___) wrapping CJK content — markers removed
//   - Images (![alt](url)) — removed entirely
func filterMarkdown(text string) string {
	lines := strings.Split(text, "\n")
	var out []string
	inCodeBlock := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Code fence: toggle and preserve the marker line.
		if strings.HasPrefix(trimmed, "```") {
			inCodeBlock = !inCodeBlock
			out = append(out, line)
			continue
		}

		// Inside code block — pass through verbatim.
		if inCodeBlock {
			out = append(out, line)
			continue
		}

		// Process heading markers (H5/H6 stripped, H1-H4 preserved).
		line = processHeading(line)

		// Inline processing: images and CJK-aware italic/bold-italic.
		if trimmed != "" {
			line = filterInline(line)
		}

		out = append(out, line)
	}

	// Trim trailing empty lines.
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}

	return strings.Join(out, "\n")
}

// processHeading strips H5/H6 prefixes but preserves H1-H4.
func processHeading(line string) string {
	trimmed := strings.TrimSpace(line)
	for _, prefix := range []string{"###### ", "##### "} {
		if after, ok := strings.CutPrefix(trimmed, prefix); ok {
			return after
		}
	}
	return line
}

// filterInline processes inline markdown within a single line.
// Applies: image removal, then CJK-aware italic/bold-italic handling.
// Bold (**/__), strikethrough (~~), and inline code (`) are preserved.
func filterInline(line string) string {
	// Remove images first: ![alt](url)
	line = removeImages(line)

	var result strings.Builder
	i := 0
	for i < len(line) {
		c := line[i]

		switch c {
		case '*':
			remaining := line[i:]
			if hasPrefix(remaining, "***") {
				// Triple asterisk: CJK-aware bold-italic.
				i += processTripleDelim(&result, line, i, '*')
				continue
			}
			if hasPrefix(remaining, "**") {
				// Double asterisk: bold — always preserve.
				result.WriteString("**")
				i += 2
				continue
			}
			// Single asterisk: italic — CJK-aware, not if followed by space.
			if i+1 < len(line) && line[i+1] != ' ' && line[i+1] != '\n' {
				if n := processSingleDelim(&result, line, i, '*'); n > 0 {
					i += n
					continue
				}
			}
			result.WriteByte(c)
			i++

		case '_':
			remaining := line[i:]
			if hasPrefix(remaining, "___") {
				i += processTripleDelim(&result, line, i, '_')
				continue
			}
			if hasPrefix(remaining, "__") {
				result.WriteString("__")
				i += 2
				continue
			}
			// Single underscore: italic — CJK-aware.
			if i+1 < len(line) && line[i+1] != ' ' && line[i+1] != '\n' {
				if n := processSingleDelim(&result, line, i, '_'); n > 0 {
					i += n
					continue
				}
			}
			result.WriteByte(c)
			i++

		default:
			result.WriteByte(c)
			i++
		}
	}

	return result.String()
}

// hasPrefix checks if s starts with prefix without allocating.
func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// processTripleDelim handles ***text*** or ___text___ with CJK awareness.
// Returns the number of bytes consumed.
func processTripleDelim(result *strings.Builder, line string, start int, delim byte) int {
	delim3 := strings.Repeat(string(delim), 3)
	closing := strings.Index(line[start+3:], delim3)
	if closing == -1 {
		// No closing — output opening delimiter and move on.
		result.WriteString(delim3)
		return 3
	}
	content := line[start+3 : start+3+closing]
	if containsCJK(content) {
		// CJK content — strip markers, keep content.
		result.WriteString(content)
	} else {
		// Non-CJK — preserve markers and content.
		result.WriteString(delim3)
		result.WriteString(content)
		result.WriteString(delim3)
	}
	return 6 + closing // skip past ***content***
}

// processSingleDelim handles *text* or _text_ with CJK awareness.
// Returns the number of bytes consumed, or 0 if no matching closing delimiter found.
func processSingleDelim(result *strings.Builder, line string, start int, delim byte) int {
	closing := strings.IndexByte(line[start+1:], delim)
	if closing == -1 {
		return 0 // no closing delimiter found
	}
	content := line[start+1 : start+1+closing]
	if containsCJK(content) {
		// CJK content — strip markers, keep content.
		result.WriteString(content)
	} else {
		// Non-CJK — preserve markers and content.
		result.WriteByte(delim)
		result.WriteString(content)
		result.WriteByte(delim)
	}
	return 2 + closing // skip past *content*
}

// containsCJK reports whether text contains any CJK characters.
func containsCJK(text string) bool {
	for _, r := range text {
		if isCJK(r) {
			return true
		}
	}
	return false
}

// isCJK checks if a rune is a CJK character.
// Covers: CJK Radicals Supplement .. CJK Unified Ideographs,
// Hangul Syllables, and CJK Compatibility Ideographs.
func isCJK(r rune) bool {
	return (r >= 0x2E80 && r <= 0x9FFF) || // CJK Radicals Supplement .. CJK Unified Ideographs
		(r >= 0xAC00 && r <= 0xD7AF) || // Hangul Syllables
		(r >= 0xF900 && r <= 0xFAFF) // CJK Compatibility Ideographs
}

// removeImages removes ![alt](url) patterns.
func removeImages(s string) string {
	var result strings.Builder
	i := 0
	for i < len(s) {
		if i+1 < len(s) && s[i] == '!' && s[i+1] == '[' {
			// Find closing ]
			j := i + 2
			for j < len(s) && s[j] != ']' {
				j++
			}
			if j+1 < len(s) && s[j] == ']' && s[j+1] == '(' {
				k := j + 2
				for k < len(s) && s[k] != ')' {
					k++
				}
				if k < len(s) {
					// Found complete image syntax — skip entirely.
					i = k + 1
					continue
				}
			}
		}
		result.WriteByte(s[i])
		i++
	}
	return result.String()
}
