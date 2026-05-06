package weixin

import (
	"testing"
)

func TestFilterMarkdown_Bold(t *testing.T) {
	input := "This is **bold** text"
	expected := "This is bold text"
	got := filterMarkdown(input)
	if got != expected {
		t.Errorf("got %q, want %q", got, expected)
	}
}

func TestFilterMarkdown_Italic(t *testing.T) {
	input := "This is *italic* text"
	expected := "This is italic text"
	got := filterMarkdown(input)
	if got != expected {
		t.Errorf("got %q, want %q", got, expected)
	}
}

func TestFilterMarkdown_Strikethrough(t *testing.T) {
	input := "~~deleted~~ text"
	expected := "deleted text"
	got := filterMarkdown(input)
	if got != expected {
		t.Errorf("got %q, want %q", got, expected)
	}
}

func TestFilterMarkdown_InlineCode(t *testing.T) {
	input := "Use `ls -la` to list files"
	expected := "Use ls -la to list files"
	got := filterMarkdown(input)
	if got != expected {
		t.Errorf("got %q, want %q", got, expected)
	}
}

func TestFilterMarkdown_CodeBlock(t *testing.T) {
	input := "```\ncode block\nline 2\n```"
	expected := "code block\nline 2"
	got := filterMarkdown(input)
	if got != expected {
		t.Errorf("got %q, want %q", got, expected)
	}
}

func TestFilterMarkdown_Blockquote(t *testing.T) {
	input := "> quoted text"
	expected := "quoted text"
	got := filterMarkdown(input)
	if got != expected {
		t.Errorf("got %q, want %q", got, expected)
	}
}

func TestFilterMarkdown_Heading(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"### Heading 3", "Heading 3"},
		{"#### Heading 4", "Heading 4"},
		{"###### Heading 6", "Heading 6"},
	}

	for _, tt := range tests {
		got := filterMarkdown(tt.input)
		if got != tt.expected {
			t.Errorf("filterMarkdown(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestFilterMarkdown_HorizontalRule(t *testing.T) {
	tests := []string{"---", "***", "___", "------", "****"}
	for _, hr := range tests {
		got := filterMarkdown(hr)
		if got != "" {
			t.Errorf("filterMarkdown(%q) = %q, want empty", hr, got)
		}
	}
}

func TestFilterMarkdown_Image(t *testing.T) {
	input := "Here is an image: ![alt](https://example.com/img.png) and more text"
	expected := "Here is an image:  and more text"
	got := filterMarkdown(input)
	if got != expected {
		t.Errorf("got %q, want %q", got, expected)
	}
}

func TestFilterMarkdown_PlainText(t *testing.T) {
	input := "Plain text without any markdown"
	expected := "Plain text without any markdown"
	got := filterMarkdown(input)
	if got != expected {
		t.Errorf("got %q, want %q", got, expected)
	}
}

func TestFilterMarkdown_Mixed(t *testing.T) {
	input := "### Title\n\n**Bold** and *italic* with `code`\n\n> a quote\n\nPlain text."
	expected := "Title\n\nBold and italic with code\n\na quote\n\nPlain text."
	got := filterMarkdown(input)
	if got != expected {
		t.Errorf("got %q, want %q", got, expected)
	}
}

func TestFilterMarkdown_TripleAsterisk(t *testing.T) {
	input := "***bold italic*** text"
	expected := "bold italic text"
	got := filterMarkdown(input)
	if got != expected {
		t.Errorf("got %q, want %q", got, expected)
	}
}

func TestIsHorizontalRule(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"---", true},
		{"------", true},
		{"***", true},
		{"___", true},
		{"--", false},
		{"hello", false},
		{"- -", false},
		{"   ---   ", true},
	}

	for _, tt := range tests {
		got := isHorizontalRule(tt.input)
		if got != tt.want {
			t.Errorf("isHorizontalRule(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}
