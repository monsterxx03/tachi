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
