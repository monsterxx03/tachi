package discord

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	gest "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

var gm = goldmark.New(
	goldmark.WithExtensions(extension.Table),
)

// goldmarkResult holds the result of parsing content with goldmark.
type goldmarkResult struct {
	textContent string                   // non-table text + reconstructed multi-col tables in code blocks
	embed       *discordgo.MessageEmbed  // embed for first 2-col table
}

// parseContent parses markdown with goldmark AST and:
// - Converts the first 2-column pipe table to a Discord embed
// - Wraps multi-column tables in code blocks
// - Preserves non-table content verbatim
func parseContent(source string) *goldmarkResult {
	if source == "" {
		return &goldmarkResult{textContent: source}
	}
	src := []byte(source)
	doc := gm.Parser().Parse(text.NewReader(src))

	// Walk document children to find tables and non-table blocks
	type blockSection struct {
		text    string // original source for non-table, reconstructed for table
		isTable bool
		isEmbed bool // first 2-col table → converted to embed
	}

	var (
		sections []blockSection
		embed    *discordgo.MessageEmbed
		embedIdx = -1
	)

	for child := doc.FirstChild(); child != nil; child = child.NextSibling() {
		if child.Kind() != gest.KindTable {
			// Non-table: use original source
			segs := child.Lines()
			if segs.Len() > 0 {
				var b bytes.Buffer
				for i := 0; i < segs.Len(); i++ {
					s := segs.At(i)
					b.Write(src[s.Start:s.Stop])
				}
				sections = append(sections, blockSection{text: b.String()})
			}
			continue
		}

		// Table node
		tb := child.(*gest.Table)
		var headers []string
		var rows [][]string

		for rowNode := tb.FirstChild(); rowNode != nil; rowNode = rowNode.NextSibling() {
			var row []string
			for cell := rowNode.FirstChild(); cell != nil; cell = cell.NextSibling() {
				if cell.Kind() != gest.KindTableCell {
					continue
				}
				row = append(row, cellText(cell, src))
			}
			switch rowNode.Kind() {
			case gest.KindTableHeader:
				headers = row
			case gest.KindTableRow:
				if len(row) > 0 && (len(row) > 1 || row[0] != "") {
					rows = append(rows, row)
				}
			}
		}

		colCount := len(headers)
		for _, r := range rows {
			if len(r) > colCount {
				colCount = len(r)
			}
		}
		if colCount < 2 || len(rows) == 0 {
			// Not a valid table — add as-is
			segs := child.Lines()
			if segs.Len() > 0 {
				var b bytes.Buffer
				for i := 0; i < segs.Len(); i++ {
					s := segs.At(i)
					b.Write(src[s.Start:s.Stop])
				}
				sections = append(sections, blockSection{text: b.String()})
			}
			continue
		}

		if colCount == 2 && embed == nil {
			// First 2-col table → embed
			embed = buildEmbed(headers, rows)
			sections = append(sections, blockSection{isTable: true, isEmbed: true})
			embedIdx = len(sections) - 1
		} else {
			// Multi-col or second table → code block
			tblText := renderTable(headers, rows, colCount)
			sections = append(sections, blockSection{text: tblText, isTable: true})
		}
	}

	// Build text content (excluding embed's table)
	var buf bytes.Buffer
	for i, sec := range sections {
		if i == embedIdx {
			// Skip table that's now an embed
			continue
		}
		if buf.Len() > 0 && !strings.HasPrefix(sec.text, "\n") {
			buf.WriteByte('\n')
		}
		if sec.isTable {
			buf.WriteString("```\n")
			buf.WriteString(sec.text)
			buf.WriteString("\n```")
		} else {
			buf.WriteString(sec.text)
		}
	}

	return &goldmarkResult{
		textContent: buf.String(),
		embed:       embed,
	}
}

// convertTablesToCodeBlock finds tables via goldmark AST and wraps each
// in a code block. Uses reconstructed tables from cell data.
func convertTablesToCodeBlock(content string) string {
	if content == "" {
		return content
	}
	src := []byte(content)
	doc := gm.Parser().Parse(text.NewReader(src))

	type blockSection struct {
		text    string
		isTable bool
	}

	var sections []blockSection

	for child := doc.FirstChild(); child != nil; child = child.NextSibling() {
		if child.Kind() != gest.KindTable {
			// Non-table: original source
			segs := child.Lines()
			if segs.Len() > 0 {
				var b bytes.Buffer
				for i := 0; i < segs.Len(); i++ {
					s := segs.At(i)
					b.Write(src[s.Start:s.Stop])
				}
				sections = append(sections, blockSection{text: b.String()})
			}
			continue
		}

		tb := child.(*gest.Table)
		var headers []string
		var rows [][]string

		for rowNode := tb.FirstChild(); rowNode != nil; rowNode = rowNode.NextSibling() {
			var row []string
			for cell := rowNode.FirstChild(); cell != nil; cell = cell.NextSibling() {
				if cell.Kind() != gest.KindTableCell {
					continue
				}
				row = append(row, cellText(cell, src))
			}
			switch rowNode.Kind() {
			case gest.KindTableHeader:
				headers = row
			case gest.KindTableRow:
				if len(row) > 0 && (len(row) > 1 || row[0] != "") {
					rows = append(rows, row)
				}
			}
		}

		colCount := len(headers)
		for _, r := range rows {
			if len(r) > colCount {
				colCount = len(r)
			}
		}
		if colCount < 2 || len(rows) == 0 {
			// Invalid table, keep original
			segs := child.Lines()
			if segs.Len() > 0 {
				var b bytes.Buffer
				for i := 0; i < segs.Len(); i++ {
					s := segs.At(i)
					b.Write(src[s.Start:s.Stop])
				}
				sections = append(sections, blockSection{text: b.String()})
			}
			continue
		}

		sections = append(sections, blockSection{
			text:    renderTable(headers, rows, colCount),
			isTable: true,
		})
	}

	var buf bytes.Buffer
	for _, sec := range sections {
		if buf.Len() > 0 && !strings.HasPrefix(sec.text, "\n") {
			buf.WriteByte('\n')
		}
		if sec.isTable {
			buf.WriteString("```\n")
			buf.WriteString(sec.text)
			buf.WriteString("\n```")
		} else {
			buf.WriteString(sec.text)
		}
	}
	return buf.String()
}

// --- helpers ----------------------------------------------------------------

// cellText recursively extracts text from a table cell's AST subtree.
func cellText(n ast.Node, src []byte) string {
	var parts []string
	for child := n.FirstChild(); child != nil; child = child.NextSibling() {
		switch child.Kind() {
		case ast.KindText:
			parts = append(parts, string(child.(*ast.Text).Value(src)))
		case ast.KindString:
			parts = append(parts, string(child.(*ast.String).Value))
		case ast.KindCodeSpan:
			var code []byte
			for c := child.FirstChild(); c != nil; c = c.NextSibling() {
				if c.Kind() == ast.KindText {
					code = append(code, c.(*ast.Text).Value(src)...)
				}
			}
			parts = append(parts, "`"+string(code)+"`")
		default:
			parts = append(parts, cellText(child, src))
		}
	}
	return strings.TrimSpace(strings.Join(parts, ""))
}

// renderTable reconstructs a pipe table from cell data for code block display.
func renderTable(headers []string, rows [][]string, colCount int) string {
	var buf bytes.Buffer
	buf.WriteByte('|')
	for _, h := range headers {
		buf.WriteString(" " + h + " |")
	}
	buf.WriteByte('\n')
	buf.WriteByte('|')
	for i := 0; i < colCount; i++ {
		buf.WriteString(" --- |")
	}
	buf.WriteByte('\n')
	for _, row := range rows {
		buf.WriteByte('|')
		for _, cell := range row {
			buf.WriteString(" " + cell + " |")
		}
		buf.WriteByte('\n')
	}
	return buf.String()
}

// buildEmbed creates a Discord embed from 2-column table rows.
func buildEmbed(headers []string, rows [][]string) *discordgo.MessageEmbed {
	title := ""
	if len(headers) >= 1 && headers[0] != "" {
		title = headers[0]
	} else if len(rows) > 0 && len(rows[0]) >= 1 && rows[0][0] != "" {
		title = rows[0][0]
	}
	desc := ""
	if len(rows) > 0 {
		desc = fmt.Sprintf("共 %d 项", len(rows))
	}
	embed := &discordgo.MessageEmbed{
		Title:       title,
		Description: desc,
		Color:       0x2ECC71,
	}
	for _, row := range rows {
		name := ""
		value := ""
		if len(row) >= 1 {
			name = row[0]
		}
		if len(row) >= 2 {
			value = row[1]
		}
		if name == "" && value == "" {
			continue
		}
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:   name,
			Value:  value,
			Inline: true,
		})
	}
	return embed
}
