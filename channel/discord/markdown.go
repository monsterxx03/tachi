package discord

import (
	"strings"

	"github.com/mattn/go-runewidth"
)

// convertTablesToCodeBlocks rewrites GFM-style markdown tables in text as
// monospace-aligned plain-text tables wrapped in fenced code blocks, because
// Discord clients do not render markdown tables. Tables already inside fenced
// code blocks are left untouched.
//
// Example:
//
//	| Name | Age |          ```
//	| --- | ---: |    =>    Name  | Age
//	| bob  |   30 |         ------+----
//	                          bob   |  30
//	                          ```
func convertTablesToCodeBlocks(text string) string {
	if !strings.Contains(text, "|") {
		return text
	}

	lines := strings.Split(text, "\n")
	var out []string
	inFence := false

	i := 0
	for i < len(lines) {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		// Track fenced code blocks; their content is verbatim.
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			out = append(out, line)
			i++
			continue
		}

		// Table start: current line contains a pipe and some non-pipe content,
		// and the NEXT line is a delimiter row ("| --- | --- |" style).
		if !inFence && i+1 < len(lines) &&
			strings.Contains(line, "|") &&
			strings.Trim(line, "| \t") != "" &&
			isTableDelimiterRow(lines[i+1]) {
			header := splitTableRow(line)
			aligns := parseTableAlignments(lines[i+1])

			var rows [][]string
			j := i + 2
			for j < len(lines) {
				body := lines[j]
				bt := strings.TrimSpace(body)
				if bt == "" || !strings.Contains(body, "|") || strings.HasPrefix(bt, "```") {
					break
				}
				rows = append(rows, splitTableRow(body))
				j++
			}

			out = append(out, renderTableCodeBlock(header, aligns, rows)...)
			i = j
			continue
		}

		out = append(out, line)
		i++
	}

	return strings.Join(out, "\n")
}

// isTableDelimiterRow reports whether line is a GFM table delimiter row,
// e.g. "| --- | :---: |" or "--- | --:". Requires at least one pipe (so a
// plain "---" horizontal rule doesn't match) and at least one dash per cell.
func isTableDelimiterRow(line string) bool {
	t := strings.TrimSpace(line)
	if !strings.Contains(t, "|") {
		return false
	}
	for _, cell := range splitTableRow(t) {
		dashes := 0
		for _, r := range cell {
			switch r {
			case '-':
				dashes++
			case ':':
			default:
				return false
			}
		}
		if dashes == 0 {
			return false
		}
	}
	return true
}

// splitTableRow splits a table row into cells on pipes that are neither
// escaped (\|) nor inside inline code spans. Leading/trailing delimiter
// pipes are dropped. Escaped pipes are unescaped to plain '|'.
func splitTableRow(line string) []string {
	t := strings.TrimSpace(line)
	t = strings.TrimPrefix(t, "|")
	t = strings.TrimSuffix(t, "|")

	var cells []string
	var cur strings.Builder
	inCode := false
	for i := 0; i < len(t); i++ {
		c := t[i]
		switch {
		case c == '\\' && i+1 < len(t) && t[i+1] == '|':
			cur.WriteByte('|')
			i++
		case c == '`':
			inCode = !inCode
			cur.WriteByte(c)
		case c == '|' && !inCode:
			cells = append(cells, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	cells = append(cells, strings.TrimSpace(cur.String()))
	return cells
}

// columnAlign is a table column's text alignment, parsed from the delimiter row.
type columnAlign int

const (
	alignLeft columnAlign = iota
	alignRight
	alignCenter
)

// parseTableAlignments extracts per-column alignment from a delimiter row:
// ":--" left, "--:" right, ":-:" center, "---" left (default).
func parseTableAlignments(delimLine string) []columnAlign {
	var aligns []columnAlign
	for _, cell := range splitTableRow(delimLine) {
		left := strings.HasPrefix(cell, ":")
		right := strings.HasSuffix(cell, ":")
		switch {
		case left && right:
			aligns = append(aligns, alignCenter)
		case right:
			aligns = append(aligns, alignRight)
		default:
			aligns = append(aligns, alignLeft)
		}
	}
	return aligns
}

// renderTableCodeBlock renders header and rows as a monospace-aligned table
// inside a fenced code block. Columns are padded by display width (CJK-aware)
// and separated with " | "; a dashes separator under the header mirrors the
// GFM delimiter row.
func renderTableCodeBlock(header []string, aligns []columnAlign, rows [][]string) []string {
	ncols := len(header)
	for _, r := range rows {
		if len(r) > ncols {
			ncols = len(r)
		}
	}
	if ncols == 0 {
		return nil
	}

	header = padRow(header, ncols)
	for i := range rows {
		rows[i] = padRow(rows[i], ncols)
	}
	for len(aligns) < ncols {
		aligns = append(aligns, alignLeft)
	}

	widths := make([]int, ncols)
	measure := func(cells []string) {
		for i, c := range cells {
			if w := runewidth.StringWidth(c); w > widths[i] {
				widths[i] = w
			}
		}
	}
	measure(header)
	for _, r := range rows {
		measure(r)
	}

	formatRow := func(cells []string) string {
		parts := make([]string, ncols)
		for i, c := range cells {
			parts[i] = padToWidth(c, widths[i], aligns[i])
		}
		return strings.TrimRight(strings.Join(parts, " | "), " ")
	}

	sepParts := make([]string, ncols)
	for i, w := range widths {
		sepParts[i] = strings.Repeat("-", w)
	}

	out := make([]string, 0, len(rows)+4)
	out = append(out, "```")
	out = append(out, formatRow(header))
	out = append(out, strings.Join(sepParts, "-+-"))
	for _, r := range rows {
		out = append(out, formatRow(r))
	}
	out = append(out, "```")
	return out
}

// padRow extends row with empty cells until it has n columns.
func padRow(row []string, n int) []string {
	for len(row) < n {
		row = append(row, "")
	}
	return row
}

// padToWidth pads s with spaces to the given display width according to align.
// Width is measured in terminal cells (CJK characters count as 2).
func padToWidth(s string, width int, align columnAlign) string {
	pad := width - runewidth.StringWidth(s)
	if pad <= 0 {
		return s
	}
	switch align {
	case alignRight:
		return strings.Repeat(" ", pad) + s
	case alignCenter:
		l := pad / 2
		return strings.Repeat(" ", l) + s + strings.Repeat(" ", pad-l)
	default:
		return s + strings.Repeat(" ", pad)
	}
}
