package discord

import "strings"

// convertTablesToCodeBlock detects markdown pipe tables in content and wraps
// each table block in a code block (```...```) so they render consistently
// across all Discord clients (desktop, web, mobile).
//
// It skips content that already contains code block fences to avoid
// double-wrapping, and only converts tables that have a valid separator
// line (|---|---|---|) followed by data rows.
func convertTablesToCodeBlock(content string) string {
	if content == "" {
		return content
	}

	// Skip if already inside code blocks (avoid double-wrapping).
	if strings.Contains(content, "```") {
		return content
	}

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
// starts with | and ends with | (or is just |---| separator).
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
