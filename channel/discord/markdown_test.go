package discord

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// markdown.go — convertTablesToCodeBlocks
// ---------------------------------------------------------------------------

func TestConvertTablesToCodeBlocks(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "no table",
			input: "hello **world**\nplain | pipe but no delimiter",
			want:  "hello **world**\nplain | pipe but no delimiter",
		},
		{
			name:  "pipe text without delimiter row",
			input: "a | b\n---",
			want:  "a | b\n---",
		},
		{
			name:  "simple table",
			input: "before\n| Name | Age |\n| --- | --- |\n| alice | 30 |\n| bob | 4 |\nafter",
			want:  "before\n```\nName  | Age\n------+----\nalice | 30\nbob   | 4\n```\nafter",
		},
		{
			name:  "table without leading/trailing pipes",
			input: "Name | Age\n--- | ---\nalice | 30",
			want:  "```\nName  | Age\n------+----\nalice | 30\n```",
		},
		{
			name:  "right-aligned column",
			input: "| Item | Price |\n| --- | ---: |\n| apple | 3 |\n| pear | 100 |",
			want:  "```\nItem  | Price\n------+------\napple |     3\npear  |   100\n```",
		},
		{
			name:  "CJK cells aligned by display width",
			input: "| 名字 | 年龄 |\n| --- | --- |\n| 小明 | 30 |",
			want:  "```\n名字 | 年龄\n-----+-----\n小明 | 30\n```",
		},
		{
			name:  "pipe inside inline code span",
			input: "| `a|b` | c |\n| --- | --- |\n| d | e |",
			want:  "```\n`a|b` | c\n------+--\nd     | e\n```",
		},
		{
			name:  "escaped pipe in cell",
			input: "| a \\| b | c |\n| --- | --- |\n| d | e |",
			want:  "```\na | b | c\n------+--\nd     | e\n```",
		},
		{
			name:  "table inside fenced code block untouched",
			input: "```\n| a | b |\n| - | - |\n| 1 | 2 |\n```",
			want:  "```\n| a | b |\n| - | - |\n| 1 | 2 |\n```",
		},
		{
			name:  "multiple tables",
			input: "| a |\n| - |\n| 1 |\nmiddle\n| b |\n| - |\n| 2 |",
			want:  "```\na\n-\n1\n```\nmiddle\n```\nb\n-\n2\n```",
		},
		{
			name:  "ragged rows padded to max columns",
			input: "| a | b |\n| - | - |\n| 1 |\n| 2 | 3 | 4 |",
			want:  "```\na | b |\n--+---+--\n1 |   |\n2 | 3 | 4\n```",
		},
		{
			name:  "body stops at blank line",
			input: "| a |\n| - |\n| 1 |\n\n| not a row",
			want:  "```\na\n-\n1\n```\n\n| not a row",
		},
		{
			name:  "delimiter without dashes rejected",
			input: "| a | b |\n| x | y |\n| 1 | 2 |",
			want:  "| a | b |\n| x | y |\n| 1 | 2 |",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := convertTablesToCodeBlocks(tt.input)
			if got != tt.want {
				t.Errorf("convertTablesToCodeBlocks(%q)\n  got:\n%s\n  want:\n%s", tt.input, got, tt.want)
			}
		})
	}
}

// TestConvertTablesAlignment verifies that within a rendered code block, the
// column separator pipe lands on the same display column for every row
// (critical for CJK content where byte length != display width).
func TestConvertTablesAlignment(t *testing.T) {
	input := "| 名字 | score |\n| --- | --- |\n| 小明 | 9 |\n| alice | 100 |"
	out := convertTablesToCodeBlocks(input)

	lines := strings.Split(out, "\n")
	if len(lines) < 6 {
		t.Fatalf("expected code block with header, separator and 2 rows, got:\n%s", out)
	}
	// Skip fence lines; check header, separator, rows.
	body := lines[1 : len(lines)-1]
	pipeCol := -1
	for i, line := range body {
		idx := strings.Index(line, "|")
		if idx < 0 {
			// Separator row uses "+" instead of "|".
			idx = strings.Index(line, "+")
		}
		if idx < 0 {
			t.Fatalf("line %d has no column separator: %q", i, line)
		}
		// Compare display width (not byte index) of the prefix.
		w := displayWidthOf(line[:idx])
		if pipeCol < 0 {
			pipeCol = w
		} else if w != pipeCol {
			t.Errorf("line %d separator at display column %d, want %d: %q", i, w, pipeCol, line)
		}
	}
}

// displayWidthOf mirrors runewidth.StringWidth for test readability.
func displayWidthOf(s string) int {
	w := 0
	for _, r := range s {
		if r >= 0x2E80 && r <= 0x9FFF {
			w += 2
		} else {
			w++
		}
	}
	return w
}

func TestSplitTableRow(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{"with outer pipes", "| a | b |", []string{"a", "b"}},
		{"without outer pipes", "a | b", []string{"a", "b"}},
		{"single cell", "| a |", []string{"a"}},
		{"code span pipe", "| `x|y` | b |", []string{"`x|y`", "b"}},
		{"escaped pipe", `a \| b | c`, []string{"a | b", "c"}},
		{"empty cells", "| a || b |", []string{"a", "", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitTableRow(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("splitTableRow(%q) = %v, want %v", tt.input, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("splitTableRow(%q) = %v, want %v", tt.input, got, tt.want)
				}
			}
		})
	}
}

func TestIsTableDelimiterRow(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"| --- | --- |", true},
		{"--- | ---", true},
		{"| :--- | ---: |", true},
		{"| :---: |", true},
		{"---", false},         // horizontal rule, no pipe
		{"| a | b |", false},   // regular row, no dashes
		{"| --- | x |", false}, // non-delimiter cell
		{"", false},            // empty
		{"| - | - |", true},    // single dash per cell
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := isTableDelimiterRow(tt.input); got != tt.want {
				t.Errorf("isTableDelimiterRow(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}
