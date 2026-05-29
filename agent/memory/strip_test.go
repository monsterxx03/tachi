package memory

import (
	"testing"
)

func TestStripNoiseTags_SingleTag(t *testing.T) {
	input := "<system-reminder>\nsome reminder text\n</system-reminder>\nReal content here"
	result := stripNoiseTags(input)
	expected := "Real content here"
	if result != expected {
		t.Errorf("stripNoiseTags = %q, want %q", result, expected)
	}
}

func TestStripNoiseTags_MultipleTags(t *testing.T) {
	input := "<system-reminder>\nA\n</system-reminder>\nKeep this\n<available-deferred-tools>\nB\n</available-deferred-tools>\nMore content"
	result := stripNoiseTags(input)
	expected := "Keep this\nMore content"
	if result != expected {
		t.Errorf("stripNoiseTags = %q, want %q", result, expected)
	}
}

func TestStripNoiseTags_NoTags(t *testing.T) {
	input := "Just normal content without any tags"
	result := stripNoiseTags(input)
	if result != input {
		t.Errorf("stripNoiseTags = %q, want %q", result, input)
	}
}

func TestStripNoiseTags_UnmatchedOpening(t *testing.T) {
	input := "Text before <system-reminder>unclosed tag"
	result := stripNoiseTags(input)
	expected := "Text before"
	if result != expected {
		t.Errorf("stripNoiseTags = %q, want %q", result, expected)
	}
}

func TestStripNoiseTags_AllKnownTags(t *testing.T) {
	allTags := []string{
		"<local-command-caveat>",
		"<local-command-stdout>",
		"<command-name>",
		"<command-message>",
		"<task-notification>",
		"<system-reminder>",
		"<available-skills>",
		"<available-deferred-tools>",
		"<relevant-memories>",
	}
	for _, tag := range allTags {
		endTag := tag[:1] + "/" + tag[1:]
		input := tag + "noise" + endTag + "\nReal"
		result := stripNoiseTags(input)
		if result != "Real" {
			t.Errorf("stripNoiseTags(%q) = %q, want %q", tag, result, "Real")
		}
	}
}

func TestStripNoiseTags_RelevantMemories(t *testing.T) {
	input := "Before <relevant-memories>\nsome memory content\n</relevant-memories> After"
	result := stripNoiseTags(input)
	// After removing the tag block, there's a double space (space before + space after tag)
	expected := "Before  After"
	if result != expected {
		t.Errorf("stripNoiseTags = %q, want %q", result, expected)
	}
}

func TestStripNoiseTags_RelevantMemoriesUnmatched(t *testing.T) {
	input := "Text <relevant-memories>unclosed"
	result := stripNoiseTags(input)
	expected := "Text"
	if result != expected {
		t.Errorf("stripNoiseTags = %q, want %q", result, expected)
	}
}
