package discord

import (
	"testing"
)

func TestConvertTablesToCodeBlock_NoTable(t *testing.T) {
	input := "Hello, world!\nThis is a test."
	got := convertTablesToCodeBlock(input)
	if got != input {
		t.Errorf("expected no change, got:\n%s", got)
	}
}

func TestConvertTablesToCodeBlock_SimpleTable(t *testing.T) {
	input := "| Name | Lang |\n|------|------|\n| Tachi | Go |\n| Hermes | Python |"
	expected := "```\n| Name | Lang |\n|------|------|\n| Tachi | Go |\n| Hermes | Python |\n```"
	got := convertTablesToCodeBlock(input)
	if got != expected {
		t.Errorf("expected:\n%s\n\ngot:\n%s", expected, got)
	}
}

func TestConvertTablesToCodeBlock_TableWithSurroundingText(t *testing.T) {
	input := "Here are the stats:\n| Name | Lang |\n|------|------|\n| Tachi | Go |\n\nMore text below."
	expected := "Here are the stats:\n```\n| Name | Lang |\n|------|------|\n| Tachi | Go |\n```\n\nMore text below."
	got := convertTablesToCodeBlock(input)
	if got != expected {
		t.Errorf("expected:\n%s\n\ngot:\n%s", expected, got)
	}
}

func TestConvertTablesToCodeBlock_MultipleTables(t *testing.T) {
	input := "Table A:\n| A | B |\n|---|---|\n| 1 | 2 |\n\nTable B:\n| X | Y |\n|---|---|\n| 3 | 4 |"
	expected := "Table A:\n```\n| A | B |\n|---|---|\n| 1 | 2 |\n```\n\nTable B:\n```\n| X | Y |\n|---|---|\n| 3 | 4 |\n```"
	got := convertTablesToCodeBlock(input)
	if got != expected {
		t.Errorf("expected:\n%s\n\ngot:\n%s", expected, got)
	}
}

func TestConvertTablesToCodeBlock_AlreadyInCodeBlock(t *testing.T) {
	input := "```\n| Name | Lang |\n|------|------|\n| Tachi | Go |\n```"
	got := convertTablesToCodeBlock(input)
	if got != input {
		t.Errorf("content already in code block should not be modified, got:\n%s", got)
	}
}

func TestConvertTablesToCodeBlock_NoSeparator(t *testing.T) {
	input := "| Name | Lang |\n| Tachi | Go |"
	got := convertTablesToCodeBlock(input)
	if got != input {
		t.Errorf("no separator line, should not be converted, got:\n%s", got)
	}
}

func TestConvertTablesToCodeBlock_Empty(t *testing.T) {
	got := convertTablesToCodeBlock("")
	if got != "" {
		t.Errorf("expected empty, got: %q", got)
	}
}

func TestConvertTablesToCodeBlock_NotEnoughLines(t *testing.T) {
	input := "| just one line |"
	got := convertTablesToCodeBlock(input)
	if got != input {
		t.Errorf("single line should not be converted")
	}
}

func TestConvertTablesToCodeBlock_SeparatorVariants(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"basic", "| A | B |\n|---|---|\n| 1 | 2 |"},
		{"colon_left", "| A | B |\n|:---|---:|\n| 1 | 2 |"},
		{"colon_both", "| A | B |\n|:---:|:---:|\n| 1 | 2 |"},
		{"with_spaces", "| A | B |\n| --- | --- |\n| 1 | 2 |"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := convertTablesToCodeBlock(tt.input)
			if !stringsContains(got, "```") {
				t.Errorf("expected code block wrapper, got:\n%s", got)
			}
		})
	}
}

func TestConvertTablesToCodeBlock_EmptyCell(t *testing.T) {
	input := "| A | B | C |\n|---|---|---|\n| 1 | | 3 |"
	got := convertTablesToCodeBlock(input)
	if !stringsContains(got, "```") {
		t.Errorf("table with empty cell should still be converted, got:\n%s", got)
	}
}

func TestConvertTablesToCodeBlock_TableInMiddleOfText(t *testing.T) {
	input := "Start text.\n| Key | Value |\n|-----|-------|\n| a | 1 |\n| b | 2 |\nEnd text."
	expected := "Start text.\n```\n| Key | Value |\n|-----|-------|\n| a | 1 |\n| b | 2 |\n```\nEnd text."
	got := convertTablesToCodeBlock(input)
	if got != expected {
		t.Errorf("expected:\n%s\n\ngot:\n%s", expected, got)
	}
}

func TestConvertTablesToCodeBlock_MixedCodeBlockAndTable(t *testing.T) {
	input := "Here's a table:\n| A | B |\n|---|---|\n| 1 | 2 |\n\nAnd some code:\n```go\nfunc main() {}\n```"
	// Table should be wrapped, code block preserved.
	got := convertTablesToCodeBlock(input)
	if !stringsContains(got, "```\n| A | B |") {
		t.Errorf("table should be wrapped in code block, got:\n%s", got)
	}
	if !stringsContains(got, "```go\nfunc main() {}\n```") {
		t.Errorf("code block should be preserved, got:\n%s", got)
	}
}

func TestConvertTablesToCodeBlock_TableBeforeCodeBlock(t *testing.T) {
	input := "| X | Y |\n|---|---|\n| 1 | 2 |\n\n```\nraw code\n```"
	got := convertTablesToCodeBlock(input)
	if !stringsContains(got, "```\n| X | Y |\n|---|---|\n| 1 | 2 |\n```") {
		t.Errorf("table should be wrapped, got:\n%s", got)
	}
	if !stringsContains(got, "```\nraw code\n```") {
		t.Errorf("code block should be preserved, got:\n%s", got)
	}
}

// stringsContains is a simple helper.
func stringsContains(s, substr string) bool {
	return len(s) >= len(substr) && containsString(s, substr)
}

func containsString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Table → EMBED conversion tests
// ---------------------------------------------------------------------------

func TestConvertTablesToEmbeds_TwoColumn(t *testing.T) {
	input := "| 文件 | 改动 |\n|:---|:---|\n| `send.go` | rune-aware split |\n| `channel.go` | use sendText |"
	got := convertTablesToEmbeds(input)
	if !stringsContains(got, "EMBED:") {
		t.Errorf("2-column table should be converted to EMBED, got:\n%s", got)
	}
	if !stringsContains(got, "field:`send.go`|rune-aware split|true") {
		t.Errorf("field should contain row data, got:\n%s", got)
	}
	if stringsContains(got, "```") {
		t.Errorf("2-column table should NOT be wrapped in code block, got:\n%s", got)
	}
}

func TestConvertTablesToEmbeds_MultiColumn(t *testing.T) {
	input := "| A | B | C |\n|---|---|---|\n| 1 | 2 | 3 |\n| 4 | 5 | 6 |"
	got := convertTablesToEmbeds(input)
	if stringsContains(got, "EMBED:") {
		t.Errorf("multi-column table should NOT be converted to EMBED, got:\n%s", got)
	}
	// Should be left as-is for code block conversion.
	if got != input {
		t.Errorf("multi-column table should be unchanged, got:\n%s", got)
	}
}

func TestConvertTablesToEmbeds_WithTextBefore(t *testing.T) {
	input := "Here are the changes:\n| File | Change |\n|------|--------|\n| a.go | fix |\n| b.go | add |"
	got := convertTablesToEmbeds(input)
	if !stringsContains(got, "EMBED:") {
		t.Errorf("should contain EMBED, got:\n%s", got)
	}
	// Text before the table should be preserved.
	if !stringsContains(got, "Here are the changes") {
		t.Errorf("text before table should be preserved, got:\n%s", got)
	}
}

func TestConvertTablesToEmbeds_TableInsideCodeBlock(t *testing.T) {
	input := "```\n| A | B |\n|---|---|\n| 1 | 2 |\n```"
	got := convertTablesToEmbeds(input)
	if stringsContains(got, "EMBED:") {
		t.Errorf("table inside code block should NOT be converted, got:\n%s", got)
	}
	if got != input {
		t.Errorf("content unchanged, got:\n%s", got)
	}
}

func TestConvertTablesToEmbeds_Empty(t *testing.T) {
	got := convertTablesToEmbeds("")
	if got != "" {
		t.Errorf("expected empty, got: %q", got)
	}
}

func TestConvertTablesToEmbeds_NoTable(t *testing.T) {
	input := "Just plain text."
	got := convertTablesToEmbeds(input)
	if got != input {
		t.Errorf("no table should not change content, got:\n%s", got)
	}
}

// ---------------------------------------------------------------------------
// parseEmbedContent extended tests (EMBED not at start)
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
	// Remaining should include text before and after the embed.
	if !stringsContains(remaining, "Here's a summary") {
		t.Errorf("remaining should include text before EMBED, got: %q", remaining)
	}
	if !stringsContains(remaining, "More details below.") {
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
