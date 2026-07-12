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

	sections := splitCodeBlockSections(content)
	if len(sections) <= 1 {
		return convertPipeTablesToCodeBlock(content)
	}

	var result strings.Builder
	for i, sec := range sections {
		if i%2 == 0 {
			result.WriteString(convertPipeTablesToCodeBlock(sec))
		} else {
			result.WriteString(sec)
		}
	}
	return result.String()
}

// convertTablesToEmbeds scans content for pipe tables and converts those
// suitable for embed display to EMBED: format. Currently converts:
//   - 2-column tables → EMBED with inline fields (each row = one field)
//   - Multi-column tables → left unchanged (handled by convertTablesToCodeBlock)
//
// Only converts tables in non-code-block sections. Returns the modified content.
func convertTablesToEmbeds(content string) string {
	if content == "" {
		return content
	}

	sections := splitCodeBlockSections(content)
	if len(sections) <= 1 {
		return convertPipeTablesToEmbeds(content)
	}

	var result strings.Builder
	for i, sec := range sections {
		if i%2 == 0 {
			result.WriteString(convertPipeTablesToEmbeds(sec))
		} else {
			result.WriteString(sec)
		}
	}
	return result.String()
}

// ---------------------------------------------------------------------------
// Pipe table parser — parses a markdown pipe table into rows of cells.
// ---------------------------------------------------------------------------

// parsedTable holds the parsed structure of a pipe table.
type parsedTable struct {
	headers   []string   // first row (header), may be empty if row is all dashes
	separator int        // line index of separator row within rawLines
	rows      [][]string // data rows (content after separator)
	rawLines  []string   // original lines (for fallback)
	colCount  int
}

// parsePipeTable attempts to parse a pipe table starting at lineIdx in lines.
// Returns the parsed table and the index after the last table line.
// Returns nil table if no valid table is found.
func parsePipeTable(lines []string, startIdx int) (*parsedTable, int) {
	if startIdx >= len(lines) || !isPipeTableRow(strings.TrimSpace(lines[startIdx])) {
		return nil, startIdx
	}

	// Collect consecutive pipe rows.
	var pipeLines []string
	idx := startIdx
	for idx < len(lines) && isPipeTableRow(strings.TrimSpace(lines[idx])) {
		pipeLines = append(pipeLines, strings.TrimSpace(lines[idx]))
		idx++
	}

	if len(pipeLines) < 3 {
		return nil, startIdx // need at least header + separator + 1 data row
	}

	// Find the separator row.
	sepIdx := -1
	for i, line := range pipeLines {
		if looksLikeTableSeparator(line) {
			sepIdx = i
			break
		}
	}
	if sepIdx < 0 || sepIdx == len(pipeLines)-1 {
		return nil, startIdx // no separator, or separator is the last row
	}

	// Parse cells from a pipe row.
	parseRow := func(line string) []string {
		s := line
		if strings.HasPrefix(s, "|") {
			s = s[1:]
		}
		if strings.HasSuffix(s, "|") {
			s = s[:len(s)-1]
		}
		parts := strings.Split(s, "|")
		cells := make([]string, len(parts))
		for i, p := range parts {
			cells[i] = strings.TrimSpace(p)
		}
		return cells
	}

	headers := parseRow(pipeLines[sepIdx-1]) // line before separator
	dataRows := make([][]string, 0, len(pipeLines)-sepIdx-1)
	for i := sepIdx + 1; i < len(pipeLines); i++ {
		row := parseRow(pipeLines[i])
		if len(row) > 0 && (len(row) > 1 || row[0] != "") {
			dataRows = append(dataRows, row)
		}
	}

	if len(dataRows) == 0 {
		return nil, startIdx
	}

	// Determine column count (max of header and data rows).
	colCount := len(headers)
	for _, row := range dataRows {
		if len(row) > colCount {
			colCount = len(row)
		}
	}
	if colCount < 2 {
		return nil, startIdx
	}

	return &parsedTable{
		headers:  headers,
		separator: sepIdx,
		rows:     dataRows,
		rawLines: pipeLines,
		colCount: colCount,
	}, idx
}

// ---------------------------------------------------------------------------
// EMBED conversion — 2-column tables become Discord embed inline fields.
// ---------------------------------------------------------------------------

// tableToEmbed converts a 2-column parsedTable to EMBED: format string.
// Returns empty string if the table is not suitable for embed conversion.
func tableToEmbed(t *parsedTable) string {
	if t.colCount != 2 {
		return ""
	}

	// Derive title: use first header cell if available and non-empty.
	title := ""
	if len(t.headers) > 0 && t.headers[0] != "" {
		title = t.headers[0]
	}

	var b strings.Builder
	b.WriteString("EMBED:" + title + "|")

	// Count actual data to decide description.
	dataCount := len(t.rows)
	desc := ""
	if dataCount > 0 {
		desc = "共 " + itoa(dataCount) + " 项"
	}
	color := "green" // default
	b.WriteString(desc + "|" + color + "\n")

	for _, row := range t.rows {
		name := ""
		value := ""
		if len(row) >= 1 {
			name = row[0]
		}
		if len(row) >= 2 {
			value = row[1]
		}
		b.WriteString("field:" + name + "|" + value + "|true\n")
	}

	return b.String()
}

// convertPipeTablesToEmbeds scans text for pipe tables and replaces
// 2-column tables with EMBED: format. Multi-column tables are left unchanged.
// The input should not contain ``` fences.
func convertPipeTablesToEmbeds(content string) string {
	lines := strings.Split(content, "\n")
	if len(lines) < 3 {
		return content
	}

	var result []string
	i := 0
	for i < len(lines) {
		t, end := parsePipeTable(lines, i)
		if t != nil && t.colCount == 2 {
			embedStr := tableToEmbed(t)
			if embedStr != "" {
				result = append(result, embedStr)
				i = end
				continue
			}
		}
		if t != nil {
			// Valid table but not suitable for embed — keep raw (code block will handle it).
			for j := i; j < end; j++ {
				result = append(result, lines[j])
			}
			i = end
			continue
		}
		result = append(result, lines[i])
		i++
	}

	return strings.Join(result, "\n")
}

// convertPipeTablesToCodeBlock detects pipe tables in the given text and wraps each
// in a code block. The input should not contain ``` fences.
func convertPipeTablesToCodeBlock(content string) string {
	lines := strings.Split(content, "\n")
	if len(lines) < 3 {
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

	if inTable {
		flushTable()
	}

	return strings.Join(result, "\n")
}

// ---------------------------------------------------------------------------
// Code block section splitter
// ---------------------------------------------------------------------------

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

		if idx > 0 {
			sections = append(sections, remaining[:idx])
		} else {
			sections = append(sections, "")
		}
		remaining = remaining[idx:]

		// Find closing fence.
		searchFrom := len(fence)
		ci := strings.Index(remaining[searchFrom:], fence)
		if ci >= 0 {
			closeIdx := searchFrom + ci
			sections = append(sections, remaining[:closeIdx+len(fence)])
			remaining = remaining[closeIdx+len(fence):]
		} else {
			sections = append(sections, remaining)
			remaining = ""
			break
		}
	}

	return sections
}

// ---------------------------------------------------------------------------
// Pipe row detection helpers
// ---------------------------------------------------------------------------

// isPipeTableRow checks if a trimmed line looks like a pipe table row:
// starts with | and ends with |.
func isPipeTableRow(line string) bool {
	if line == "" {
		return false
	}
	return strings.HasPrefix(line, "|") && strings.HasSuffix(line, "|")
}

// hasTableSeparator checks if the accumulated pipe rows contain a valid
// markdown table separator line (e.g. |---|---| or |:---|---:|).
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

// looksLikeTableSeparator checks if a line is a markdown table separator.
func looksLikeTableSeparator(line string) bool {
	if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
		return false
	}
	inner := line[1 : len(line)-1]
	cells := strings.Split(inner, "|")
	if len(cells) < 2 {
		return false
	}
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

// itoa converts an int to string without importing strconv.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// trimPipeCell trims whitespace from a pipe table cell, handling edge cases.
func trimPipeCell(cell string) string {
	return strings.TrimSpace(cell)
}
