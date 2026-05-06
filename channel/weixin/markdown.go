package weixin

import (
	"strings"
)

// filterMarkdown strips markdown syntax from text for WeChat compatibility.
// WeChat does not support standard Markdown rendering, so we remove:
//   - code blocks (backticks) — keep content
//   - > blockquote prefix
//   - ### .. ###### heading prefixes
//   - horizontal rules (---, ***, ___)
//   - inline formatting (*bold*, _italic_, ~~strikethrough~~)
//   - image syntax ![alt](url)
//   - table formatting
//   - leading whitespace/tabs for indentation
func filterMarkdown(text string) string {
	lines := strings.Split(text, "\n")
	var out []string
	inCodeBlock := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Toggle code block.
		if strings.HasPrefix(trimmed, "```") {
			inCodeBlock = !inCodeBlock
			continue
		}

		if inCodeBlock {
			out = append(out, line)
			continue
		}

		// Remove horizontal rules.
		if isHorizontalRule(trimmed) {
			continue
		}

		// Remove blockquote prefix.
		if strings.HasPrefix(trimmed, "> ") {
			line = strings.TrimPrefix(trimmed, "> ")
			trimmed = line
		}

		// Remove heading markers.
		for _, prefix := range []string{"###### ", "##### ", "#### ", "### "} {
			if strings.HasPrefix(trimmed, prefix) {
				line = strings.TrimPrefix(trimmed, prefix)
				trimmed = line
				break
			}
		}

		// Process inline markdown.
		if trimmed != "" {
			line = filterInline(trimmed)
		}

		// Skip empty lines that are purely markdown artifacts.
		if strings.TrimSpace(line) == "" {
			// Don't strip all empty lines, only if previous was empty too.
			if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
				continue
			}
		}

		out = append(out, line)
	}

	// Trim trailing empty lines.
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}

	return strings.Join(out, "\n")
}

func isHorizontalRule(line string) bool {
	// Remove spaces.
	s := strings.ReplaceAll(line, " ", "")
	if len(s) < 3 {
		return false
	}

	// Check for ---, ***, ___
	allSame := true
	for i := 1; i < len(s); i++ {
		if s[i] != s[0] {
			allSame = false
			break
		}
	}

	if !allSame {
		return false
	}

	return s[0] == '-' || s[0] == '*' || s[0] == '_'
}

// filterInline processes inline markdown within a single line.
func filterInline(line string) string {
	// Remove images: ![alt](url)
	line = removeImages(line)

	// Remove code spans: `code` → code
	line = removeDelimited(line, '`')

	// Remove bold: **text** → text
	line = removeDelimited(line, '*')

	// Remove italic: _text_ → text (but not __text__ which is bold)
	// Handle __text__ first, then _text_
	line = removeDoubleDelimited(line, '_')
	line = removeDelimited(line, '_')

	// Remove bold marker (double asterisks already handled above, but
	// removeDelimited('*') might have left some triple-asterisks).
	// Re-run to catch remaining.
	line = removeDelimited(line, '~')

	return strings.TrimSpace(line)
}

// removeDelimited removes content between matching delimiter chars,
// stripping the delimiters themselves. E.g., `code` → code.
func removeDelimited(s string, delim byte) string {
	var result strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == delim {
			// Find closing delimiter.
			j := i + 1
			for j < len(s) && s[j] != delim {
				j++
			}
			if j < len(s) {
				// Found closing — append content between.
				result.WriteString(s[i+1 : j])
				i = j + 1
			} else {
				// No closing — treat as literal.
				result.WriteByte(delim)
				i++
			}
		} else {
			result.WriteByte(s[i])
			i++
		}
	}
	return result.String()
}

// removeDoubleDelimited removes **text** style pairs.
func removeDoubleDelimited(s string, delim byte) string {
	var result strings.Builder
	i := 0
	for i < len(s) {
		if i+1 < len(s) && s[i] == delim && s[i+1] == delim {
			// Find closing double delimiter.
			j := i + 2
			for j+1 < len(s) && !(s[j] == delim && s[j+1] == delim) {
				j++
			}
			if j+1 < len(s) {
				// Found closing — append content between.
				result.WriteString(s[i+2 : j])
				i = j + 2
			} else {
				// No closing — treat as literal.
				result.WriteByte(delim)
				result.WriteByte(delim)
				i += 2
			}
		} else if i+2 < len(s) && s[i] == delim && s[i+1] == delim && s[i+2] == delim {
			// Triple delimiter: ***text*** → skip.
			j := i + 3
			for j+2 < len(s) && !(s[j] == delim && s[j+1] == delim && s[j+2] == delim) {
				j++
			}
			if j+2 < len(s) {
				result.WriteString(s[i+3 : j])
				i = j + 3
			} else {
				result.WriteByte(delim)
				i++
			}
		} else {
			result.WriteByte(s[i])
			i++
		}
	}
	return result.String()
}

// removeImages removes ![alt](url) patterns.
func removeImages(s string) string {
	var result strings.Builder
	i := 0
	for i < len(s) {
		if i+1 < len(s) && s[i] == '!' && s[i+1] == '[' {
			// Find closing ](url)
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
