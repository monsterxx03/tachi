package discord

import "strings"

// convertTablesToCodeBlock detects markdown pipe tables in content and wraps
// each table block in a code block (```...```) so they render consistently
// across all Discord clients (desktop, web, mobile).
//
// It only converts tables in non-code-block sections — content already inside
// ``` fences is left untouched to avoid double-wrapping.
func convertTablesToCodeBlock(content string) string {
	if content == "" {
		return content
	}

	// Split into code-block and non-code-block sections.
	// Sections alternate: [text] [code-block] [text] [code-block] ...
	// Odd indices are code-block content (between ``` fences).
	sections := splitCodeBlockSections(content)
	if len(sections) <= 1 {
		// No code blocks at all — convert the whole thing.
		return convertPipeTables(content)
	}

	var result strings.Builder
	for i, sec := range sections {
		if i%2 == 0 {
			// Outside code block — convert tables.
			result.WriteString(convertPipeTables(sec))
		} else {
			// Inside code block — leave as-is.
			result.WriteString(sec)
		}
	}
	return result.String()
}

// splitCodeBlockSections splits content into alternating sections split by
// ``` fences. Odd-indexed sections are inside code blocks and should be
// preserved verbatim. Even-indexed sections are outside.
func splitCodeBlockSections(content string) []string {
	const fence = "```"
	var sections []string
	remaining := content

	for {
		idx := strings.Index(remaining, fence)
		if idx < 0 {
			sections = append(sections, remaining)
			break
		}

		// Text before this fence.
		if idx > 0 {
			sections = append(sections, remaining[:idx])
		} else {
			sections = append(sections, "")
		}
		remaining = remaining[idx:]

		// Find closing fence.
		closeIdx := -1
		searchFrom := len(fence) // skip the opening fence itself
		for {
			ci := strings.Index(remaining[searchFrom:], fence)
			if ci < 0 {
				break
			}
			closeIdx = searchFrom + ci
			// Make sure this isn't a fence with trailing content (e.g. ```go is fine,
			// the closing fence is just ``` at the start of a line).
			// We accept the first ``` after the opening one.
			break
		}

		if closeIdx >= 0 {
			// The code block content (between ``` and ```, including the opening fence line).
			sections = append(sections, remaining[:closeIdx+len(fence)])
			remaining = remaining[closeIdx+len(fence):]
		} else {
			// Unclosed fence — treat rest as code block.
			sections = append(sections, remaining)
			remaining = ""
			break
		}
	}

	return sections
}

// convertPipeTables detects pipe tables in the given text and wraps each
// in a code block. The input should not contain ``` fences.
func convertPipeTables(content string) string {
	lines := strings.Split(content, "\n")
	if len(lines) < 3 {
		// A table needs at least: header, separator, data row.
		return content
	}

	var result []string
	var tableLines []string
	inTable := false

	flushTable := func() {
		if len(tableLines) == 0 {
			return
		}
		if hasTableSeparator(tableLines) {
			result = append(result, "```")
			result = append(result, tableLines...)
			result = append(result, "```")
		} else {
			result = append(result, tableLines...)
		}
		tableLines = nil
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		isPipeRow := isPipeTableRow(trimmed)

		if isPipeRow {
			tableLines = append(tableLines, line)
			inTable = true
		} else {
			if inTable {
				flushTable()
				inTable = false
			}
			result = append(result, line)
		}
	}

	// Flush remaining table at end of content.
	if inTable {
		flushTable()
	}

	return strings.Join(result, "\n")
}

// isPipeTableRow checks if a trimmed line looks like a pipe table row:
// starts with | and ends with |.
func isPipeTableRow(line string) bool {
	if line == "" {
		return false
	}
	return strings.HasPrefix(line, "|") && strings.HasSuffix(line, "|")
}

// hasTableSeparator checks if the accumulated pipe rows contain a valid
// markdown table separator line (a row where the content between pipes
// is mostly dashes, like |---|---| or |:---|---:|).
func hasTableSeparator(lines []string) bool {
	if len(lines) < 2 {
		return false
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if looksLikeTableSeparator(trimmed) {
			return true
		}
	}
	return false
}

// looksLikeTableSeparator checks if a line is a markdown table separator,
// e.g. |---|---| or |:---|---:|, possibly with spaces and colons.
func looksLikeTableSeparator(line string) bool {
	if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
		return false
	}
	// Strip outer pipes and split by |.
	inner := line[1 : len(line)-1]
	cells := strings.Split(inner, "|")
	if len(cells) < 2 {
		return false
	}
	// Each cell should contain only dashes, colons, spaces, and optional leading pipe.
	for _, cell := range cells {
		trimmed := strings.TrimSpace(cell)
		if trimmed == "" {
			return false
		}
		for _, ch := range trimmed {
			if ch != '-' && ch != ':' && ch != ' ' {
				return false
			}
		}
	}
	return true
}
