package weixin

import (
	"strings"
	"testing"
)

// =============================================================================
// Preserved constructs
// =============================================================================

func TestFilterMarkdown_PlainText(t *testing.T) {
	input := "Plain text without any markdown"
	expected := "Plain text without any markdown"
	got := filterMarkdown(input)
	if got != expected {
		t.Errorf("got %q, want %q", got, expected)
	}
}

func TestFilterMarkdown_CodeFencePreserved(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "basic fence",
			input:    "```\ncode block\nline 2\n```",
			expected: "```\ncode block\nline 2\n```",
		},
		{
			name:     "fence with language tag",
			input:    "```go\nfmt.Println(\"hi\")\n```",
			expected: "```go\nfmt.Println(\"hi\")\n```",
		},
		{
			name:     "text before and after fence",
			input:    "before\n```\ncode\n```\nafter",
			expected: "before\n```\ncode\n```\nafter",
		},
		{
			name:     "markdown inside fence verbatim",
			input:    "```\n**bold** *italic* ~~strike~~\n```",
			expected: "```\n**bold** *italic* ~~strike~~\n```",
		},
		{
			name:     "multiple fenced blocks",
			input:    "```\nblock1\n```\ntext\n```\nblock2\n```",
			expected: "```\nblock1\n```\ntext\n```\nblock2\n```",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterMarkdown(tt.input)
			if got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestFilterMarkdown_InlineCodePreserved(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"Use `ls -la` to list files", "Use `ls -la` to list files"},
		{"run `rm -rf /` carefully", "run `rm -rf /` carefully"},
		{"`short`", "`short`"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := filterMarkdown(tt.input)
			if got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestFilterMarkdown_BoldPreserved(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"This is **bold** text", "This is **bold** text"},
		{"**bold**", "**bold**"},
		{"**a** and **b**", "**a** and **b**"},
		{"this is **very** important", "this is **very** important"},
		// English bold with Chinese context — should be preserved
		{"中文 **English** 中文", "中文 **English** 中文"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := filterMarkdown(tt.input)
			if got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestFilterMarkdown_BoldUnderscorePreserved(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"This is __bold__ text", "This is __bold__ text"},
		{"__bold__", "__bold__"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := filterMarkdown(tt.input)
			if got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestFilterMarkdown_StrikethroughPreserved(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"~~deleted~~ text", "~~deleted~~ text"},
		{"keep ~~this~~ too", "keep ~~this~~ too"},
		{"~~中文~~ text", "~~中文~~ text"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := filterMarkdown(tt.input)
			if got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestFilterMarkdown_BlockquotePreserved(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"> quoted text", "> quoted text"},
		{"> line1\n> line2", "> line1\n> line2"},
		{"> **bold** in quote", "> **bold** in quote"},
		{">> deeply nested", ">> deeply nested"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := filterMarkdown(tt.input)
			if got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestFilterMarkdown_Headings(t *testing.T) {
	// H1-H4 preserved, H5-H6 stripped.
	tests := []struct {
		input    string
		expected string
	}{
		{"# Title", "# Title"},
		{"## Subtitle", "## Subtitle"},
		{"### Section", "### Section"},
		{"#### Subsection", "#### Subsection"},
		{"##### Small Heading", "Small Heading"},
		{"###### Tiny Heading", "Tiny Heading"},
		// Heading followed by body text
		{"## Title\nbody text", "## Title\nbody text"},
		{"##### Title\nbody text", "Title\nbody text"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := filterMarkdown(tt.input)
			if got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestFilterMarkdown_HorizontalRulePreserved(t *testing.T) {
	tests := []string{
		"before\n---\nafter",
		"before\n***\nafter",
		"before\n___\nafter",
		"before\n- - -\nafter",
		"text\n---",
		"text\n--\nnext", // two dashes — not a horizontal rule
	}
	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			got := filterMarkdown(input)
			if got != input {
				t.Errorf("got %q, want %q", got, input)
			}
		})
	}
}

func TestFilterMarkdown_TablePreserved(t *testing.T) {
	tests := []string{
		"| Header1 | Header2 |\n|---------|----------|\n| Cell1   | Cell2   |",
		"| A | B |\n|---|---|\n| 1 | 2 |",
		"|:---|---:|",
		"结果如下：\n| A | B |\n|---|---|\n| 1 | 2 |\n完毕。",
	}
	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			got := filterMarkdown(input)
			if got != input {
				t.Errorf("got %q, want %q", got, input)
			}
		})
	}
}

func TestFilterMarkdown_ListsPreserved(t *testing.T) {
	tests := []string{
		"- item 1\n- item 2",
		"* item 1\n* item 2",
		"  - nested item",
		"      - deep item",
		"  * nested",
		"- top\n  - nested\n- top2",
	}
	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			got := filterMarkdown(input)
			if got != input {
				t.Errorf("got %q, want %q", got, input)
			}
		})
	}
}

// =============================================================================
// CJK-aware italic / bold-italic
// =============================================================================

func TestFilterMarkdown_ItalicCJKAware(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		// Non-CJK italic: preserved.
		{"*italic*", "*italic*"},
		{"this is *emphasized* text", "this is *emphasized* text"},
		// CJK italic: markers stripped.
		{"*中文斜体*", "中文斜体"},
		{"*hello 你好*", "hello 你好"},
		{"英文 *中文* 英文", "英文 中文 英文"},
		{"中文 *English* 中文", "中文 *English* 中文"},
		// * followed by space — not italic.
		{"3 * 4 = 12", "3 * 4 = 12"},
		// Mixed non-CJK italic preserved.
		{"*123!*", "*123!*"},
		// CJK with numbers.
		{"*第1章*", "第1章"},
		// Japanese.
		{"*こんにちは*", "こんにちは"},
		// Korean.
		{"*안녕하세요*", "안녕하세요"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := filterMarkdown(tt.input)
			if got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestFilterMarkdown_UnderscoreItalicCJKAware(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"_italic_", "_italic_"},
		{"_中文_", "中文"},
		{"_hello 你好_", "hello 你好"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := filterMarkdown(tt.input)
			if got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestFilterMarkdown_BoldItalicTripleCJKAware(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		// Non-CJK: preserved.
		{"***bold italic***", "***bold italic***"},
		{"this is ***very strong*** text", "this is ***very strong*** text"},
		// CJK: markers stripped.
		{"***粗斜体文字***", "粗斜体文字"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := filterMarkdown(tt.input)
			if got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestFilterMarkdown_UnderscoreTripleCJKAware(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"___bold italic___", "___bold italic___"},
		{"___粗斜体___", "粗斜体"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := filterMarkdown(tt.input)
			if got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestFilterMarkdown_ItalicOpenWithoutClose(t *testing.T) {
	// * followed by newline — just literal asterisk + newline.
	input := "*unclosed\ntext"
	got := filterMarkdown(input)
	// * is not italic because no closing * — passes through as literal.
	if got != "*unclosed\ntext" {
		t.Errorf("got %q, want %q", got, "*unclosed\ntext")
	}
}

// =============================================================================
// Image removal
// =============================================================================

func TestFilterMarkdown_Image(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"![alt](url)", ""},
		{"before ![alt](url) after", "before  after"},
		{"see ![a](u1) and ![b](u2) end", "see  and  end"},
		{"![alt](http://example.com/img.png)", ""},
		// Incomplete image syntax preserved.
		{"![alt](url", "![alt](url"},
		{"![not an image] text", "![not an image] text"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := filterMarkdown(tt.input)
			if got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}

// =============================================================================
// Combined patterns
// =============================================================================

func TestFilterMarkdown_Combined(t *testing.T) {
	input := "## **Title**\n\nUse `code` here."
	expected := "## **Title**\n\nUse `code` here."
	got := filterMarkdown(input)
	if got != expected {
		t.Errorf("got %q, want %q", got, expected)
	}
}

func TestFilterMarkdown_Mixed(t *testing.T) {
	input := "### Title\n\n**Bold** and *italic* with `code`\n\n> a quote\n\nPlain text."
	expected := "### Title\n\n**Bold** and *italic* with `code`\n\n> a quote\n\nPlain text."
	got := filterMarkdown(input)
	if got != expected {
		t.Errorf("got %q, want %q", got, expected)
	}
}

func TestFilterMarkdown_ComplexDocument(t *testing.T) {
	input := "## Summary\n\n> This is a quote.\n\nHere is **important** and *emphasized* text.\n\n```python\nprint('hello')\n```\n\n- item 1\n  - nested\n- item 2\n\n---\n\nEnd."
	got := filterMarkdown(input)
	if got != input {
		t.Errorf("got %q, want %q", got, input)
	}
}

func TestFilterMarkdown_AdjacentBoldItalic(t *testing.T) {
	// **b***i* — bold + italic adjacent.
	input := "**b***i*"
	got := filterMarkdown(input)
	// Both are non-CJK, so both should be preserved.
	if got != input {
		t.Errorf("got %q, want %q", got, input)
	}
}

func TestFilterMarkdown_BlockquoteItalicStrikethrough(t *testing.T) {
	input := "> *italic* and ~~strike~~"
	expected := "> *italic* and ~~strike~~"
	got := filterMarkdown(input)
	if got != expected {
		t.Errorf("got %q, want %q", got, expected)
	}
}

func TestFilterMarkdown_CodeFenceInlineCodeImage(t *testing.T) {
	input := "```\nfenced\n```\n`inline` ![img](url)"
	expected := "```\nfenced\n```\n`inline` "
	got := filterMarkdown(input)
	if got != expected {
		t.Errorf("got %q, want %q", got, expected)
	}
}

// =============================================================================
// Edge cases
// =============================================================================

func TestFilterMarkdown_OnlyWhitespace(t *testing.T) {
	// Leading blank lines may be trimmed.
	got := filterMarkdown("   \n  \n")
	if got != "" {
		t.Errorf("got %q, want %q", got, "")
	}
}

func TestFilterMarkdown_OnlyNewlines(t *testing.T) {
	got := filterMarkdown("\n\n\n")
	if got != "" {
		t.Errorf("got %q, want %q", got, "")
	}
}

func TestFilterMarkdown_VeryLongInput(t *testing.T) {
	var longText strings.Builder
	longText.WriteString("word ")
	for range 1000 {
		longText.WriteString("word ")
	}
	got := filterMarkdown(longText.String())
	if got != longText.String() {
		t.Errorf("long input should pass through unchanged")
	}
}

func TestFilterMarkdown_AlternatingItalicBold(t *testing.T) {
	input := "*a* **b** *c* **d**"
	expected := "*a* **b** *c* **d**"
	got := filterMarkdown(input)
	if got != expected {
		t.Errorf("got %q, want %q", got, expected)
	}
}

// =============================================================================
// containsCJK
// =============================================================================

func TestContainsCJK(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"hello", false},
		{"world", false},
		{"numbers123!", false},
		{"你好", true},
		{"hello 你好", true},
		{"日本語", true},
		{"한국어", true},
		{"混合中文和English", true},
		{"★ special", false},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := containsCJK(tt.input)
			if got != tt.want {
				t.Errorf("containsCJK(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestIsCJK(t *testing.T) {
	tests := []struct {
		r    rune
		want bool
	}{
		{'中', true},
		{'文', true},
		{'あ', true},
		{'한', true},
		{'a', false},
		{'1', false},
		{' ', false},
	}
	for _, tt := range tests {
		t.Run(string(tt.r), func(t *testing.T) {
			got := isCJK(tt.r)
			if got != tt.want {
				t.Errorf("isCJK(%q) = %v, want %v", tt.r, got, tt.want)
			}
		})
	}
}
