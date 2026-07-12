package discord

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// convertTablesToCodeBlock tests (goldmark version)
// ---------------------------------------------------------------------------

func TestConvertTablesToCodeBlock_NoTable(t *testing.T) {
	input := "Hello, world!\nThis is a test."
	got := convertTablesToCodeBlock(input)
	if got != input {
		t.Errorf("expected no change, got:\n%s", got)
	}
}

func TestConvertTablesToCodeBlock_SimpleTable(t *testing.T) {
	input := "| Name | Lang |\n|------|------|\n| Tachi | Go |\n| Hermes | Python |"
	got := convertTablesToCodeBlock(input)
	if !strings.Contains(got, "```") {
		t.Errorf("expected code block wrapper, got:\n%s", got)
	}
	// Original table should be inside code block
	if !strings.Contains(got, "| Name | Lang |") || !strings.Contains(got, "| Tachi | Go |") {
		t.Errorf("table content missing, got:\n%s", got)
	}
}

func TestConvertTablesToCodeBlock_TableWithSurroundingText(t *testing.T) {
	input := "Here are the stats:\n| Name | Lang |\n|------|------|\n| Tachi | Go |\n\nMore text below."
	got := convertTablesToCodeBlock(input)
	if !strings.Contains(got, "```") {
		t.Errorf("expected code block wrapper, got:\n%s", got)
	}
	if !strings.Contains(got, "Here are the stats") {
		t.Errorf("text before table missing, got:\n%s", got)
	}
	if !strings.Contains(got, "More text below") {
		t.Errorf("text after table missing, got:\n%s", got)
	}
}

func TestConvertTablesToCodeBlock_MultipleTables(t *testing.T) {
	input := "Table A:\n| A | B |\n|---|---|\n| 1 | 2 |\n\nTable B:\n| X | Y |\n|---|---|\n| 3 | 4 |"
	got := convertTablesToCodeBlock(input)
	if !strings.Contains(got, "```") {
		t.Errorf("expected code block wrapper, got:\n%s", got)
	}
	// Should have two code blocks
	count := strings.Count(got, "```")
	if count != 4 { // 2 opening + 2 closing
		t.Errorf("expected 4 fence markers for 2 tables, got %d", count)
	}
}

func TestConvertTablesToCodeBlock_Empty(t *testing.T) {
	got := convertTablesToCodeBlock("")
	if got != "" {
		t.Errorf("expected empty, got: %q", got)
	}
}

func TestConvertTablesToCodeBlock_NoSeparator(t *testing.T) {
	input := "| Name | Lang |"
	got := convertTablesToCodeBlock(input)
	if got != input {
		t.Errorf("single-row 'table' (no separator) should not be converted, got:\n%s", got)
	}
}

func TestConvertTablesToCodeBlock_MultiColumn(t *testing.T) {
	input := "| A | B | C | D |\n|---|---|---|---|\n| 1 | 2 | 3 | 4 |"
	got := convertTablesToCodeBlock(input)
	if !strings.Contains(got, "```") {
		t.Errorf("multi-column table should still be wrapped, got:\n%s", got)
	}
}

// ---------------------------------------------------------------------------
// parseContent tests (goldmark-based table → embed conversion)
// ---------------------------------------------------------------------------

func TestParseContent_NoTable(t *testing.T) {
	input := "Just plain text."
	res := parseContent(input)
	if res.embed != nil {
		t.Error("expected no embed for plain text")
	}
	if res.textContent != input {
		t.Errorf("expected unchanged text, got:\n%s", res.textContent)
	}
}

func TestParseContent_Empty(t *testing.T) {
	res := parseContent("")
	if res.embed != nil {
		t.Error("expected no embed for empty")
	}
}

func TestParseContent_TwoColumnTable(t *testing.T) {
	input := "| 文件 | 改动 |\n|:---|:---|\n| `send.go` | rune-aware split |\n| `channel.go` | use sendText |"
	res := parseContent(input)
	if res.embed == nil {
		t.Fatal("expected embed for 2-column table")
	}
	if res.embed.Title != "文件" {
		t.Errorf("expected title '文件', got %q", res.embed.Title)
	}
	if len(res.embed.Fields) != 2 {
		t.Errorf("expected 2 fields, got %d", len(res.embed.Fields))
	}
	if len(res.embed.Fields) >= 1 {
		f := res.embed.Fields[0]
		if !strings.Contains(f.Name, "send.go") {
			t.Errorf("field 1 name should contain send.go, got %q", f.Name)
		}
		if !strings.Contains(f.Value, "rune-aware split") {
			t.Errorf("field 1 value should contain 'rune-aware split', got %q", f.Value)
		}
	}
	// Text content should not contain the table (it's an embed now)
	if strings.Contains(res.textContent, "send.go") {
		t.Log("text content still contains table data (that's fine)")
	}
}

func TestParseContent_MultiColumnTable(t *testing.T) {
	input := "| A | B | C | D |\n|---|---|---|---|\n| 1 | 2 | 3 | 4 |\n| 5 | 6 | 7 | 8 |"
	res := parseContent(input)
	if res.embed != nil {
		t.Error("multi-column table should NOT get an embed")
	}
	// Should be wrapped in code block
	if !strings.Contains(res.textContent, "```") {
		t.Errorf("multi-column table should be code-block-wrapped, got:\n%s", res.textContent)
	}
}

func TestParseContent_MixedTableAndCodeBlock(t *testing.T) {
	input := "A table:\n| X | Y |\n|---|---|\n| 1 | 2 |\n\nSome code:\n```\nfmt.Println(\"hi\")\n```"
	res := parseContent(input)
	// 2-column table → embed
	if res.embed == nil {
		t.Fatal("expected embed for 2-column table")
	}
	// Code block should be preserved in text content
	if !strings.Contains(res.textContent, "fmt.Println") {
		t.Errorf("code block should be preserved, got:\n%s", res.textContent)
	}
}

func TestParseContent_WithTextBeforeTable(t *testing.T) {
	input := "Summary of changes:\n| File | Change |\n|------|--------|\n| a.go | fix |\n| b.go | add |"
	res := parseContent(input)
	if res.embed == nil {
		t.Fatal("expected embed")
	}
	if !strings.Contains(res.textContent, "Summary of changes") {
		t.Errorf("text before table should be preserved, got:\n%s", res.textContent)
	}
}

func TestParseContent_PipeInCellContent(t *testing.T) {
	// Table with | inside a cell — goldmark handles this correctly
	input := "| Command | Syntax |\n|---------|--------|\n| pipe | `cat foo | grep bar` |\n| or | `a || b` |"
	res := parseContent(input)
	if res.embed == nil {
		t.Fatal("expected embed even with pipes in cells")
	}
	if len(res.embed.Fields) != 2 {
		t.Errorf("expected 2 fields, got %d", len(res.embed.Fields))
	}
}

func TestParseContent_EmptyTable(t *testing.T) {
	// No data rows → not a valid table
	input := "| H1 | H2 |\n|---|---|"
	res := parseContent(input)
	if res.embed != nil {
		t.Error("table with no data rows should not get embed")
	}
}

// ---------------------------------------------------------------------------
// parseEmbedContent tests (still used for LLM-generated EMBED: text)
// ---------------------------------------------------------------------------

func TestParseEmbedContent_MiddleOfContent(t *testing.T) {
	input := "Here's a summary:\nEMBED:Changes|Overview|green\nfield:File|a.go|true\nfield:Status|done|true\n\nMore details below."
	remaining, embed, ok := parseEmbedContent(input)
	if !ok {
		t.Fatal("expected embed to be found")
	}
	if embed == nil {
		t.Fatal("embed should not be nil")
	}
	if embed.Title != "Changes" {
		t.Errorf("expected title 'Changes', got %q", embed.Title)
	}
	if len(embed.Fields) != 2 {
		t.Errorf("expected 2 fields, got %d", len(embed.Fields))
	}
	if !strings.Contains(remaining, "Here's a summary") {
		t.Errorf("remaining should include text before EMBED, got: %q", remaining)
	}
	if !strings.Contains(remaining, "More details below.") {
		t.Errorf("remaining should include text after EMBED, got: %q", remaining)
	}
}

func TestParseEmbedContent_AtStart(t *testing.T) {
	input := "EMBED:Test||blue\nfield:X|1|true"
	remaining, embed, ok := parseEmbedContent(input)
	if !ok || embed == nil {
		t.Fatal("expected embed")
	}
	if embed.Title != "Test" {
		t.Errorf("expected 'Test', got %q", embed.Title)
	}
	if remaining != "" {
		t.Errorf("expected empty remaining, got: %q", remaining)
	}
}

func TestParseEmbedContent_NoEmbeds(t *testing.T) {
	input := "Just regular text."
	remaining, embed, ok := parseEmbedContent(input)
	if ok || embed != nil {
		t.Error("expected no embed")
	}
	if remaining != input {
		t.Errorf("remaining should be original text, got: %q", remaining)
	}
}

func TestParseEmbedContent_EmbedOnlyNoFields(t *testing.T) {
	input := "EMBED:Hello|World|green"
	remaining, embed, ok := parseEmbedContent(input)
	if !ok || embed == nil {
		t.Fatal("expected embed")
	}
	if embed.Title != "Hello" {
		t.Errorf("expected 'Hello', got %q", embed.Title)
	}
	if embed.Description != "World" {
		t.Errorf("expected 'World', got %q", embed.Description)
	}
	if remaining != "" {
		t.Errorf("expected empty remaining, got: %q", remaining)
	}
}
