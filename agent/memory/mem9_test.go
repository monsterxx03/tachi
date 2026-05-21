package memory

import (
	"testing"
)

func TestIsTrivialUserMessage_Empty(t *testing.T) {
	tests := []string{"", "  ", "\n\t"}
	for _, input := range tests {
		if !isTrivialUserMessage(input) {
			t.Errorf("isTrivialUserMessage(%q) = false, want true", input)
		}
	}
}

func TestIsTrivialUserMessage_Greetings(t *testing.T) {
	tests := []string{"hello", "Hello", "HELLO", "hi", "hey", "heyy", "heyyy", "yo", "sup", "hola"}
	for _, input := range tests {
		if !isTrivialUserMessage(input) {
			t.Errorf("isTrivialUserMessage(%q) = false, want true", input)
		}
	}
}

func TestIsTrivialUserMessage_Chinese(t *testing.T) {
	tests := []string{"你好", "哈喽", "在吗", "在？", "在?", "测试", "test", "ceshi", "试用", "试试"}
	for _, input := range tests {
		if !isTrivialUserMessage(input) {
			t.Errorf("isTrivialUserMessage(%q) = false, want true", input)
		}
	}
}

func TestIsTrivialUserMessage_Meaningful(t *testing.T) {
	tests := []string{
		"How do I fix this bug?",
		"请帮我重构这段代码",
		"What is the capital of France?",
		"explain goroutines",
	}
	for _, input := range tests {
		if isTrivialUserMessage(input) {
			t.Errorf("isTrivialUserMessage(%q) = true, want false", input)
		}
	}
}

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

func TestStripMemoriesTag(t *testing.T) {
	input := "Before <relevant-memories>\nsome memory content\n</relevant-memories> After"
	result := stripMemoriesTag(input)
	// After removing the tag block, there's a double space (space before + space after tag)
	expected := "Before  After"
	if result != expected {
		t.Errorf("stripMemoriesTag = %q, want %q", result, expected)
	}
}

func TestStripMemoriesTag_NoTag(t *testing.T) {
	input := "Plain content"
	result := stripMemoriesTag(input)
	if result != input {
		t.Errorf("stripMemoriesTag = %q, want %q", result, input)
	}
}

func TestStripMemoriesTag_UnmatchedOpening(t *testing.T) {
	input := "Text <relevant-memories>unclosed"
	result := stripMemoriesTag(input)
	expected := "Text"
	if result != expected {
		t.Errorf("stripMemoriesTag = %q, want %q", result, expected)
	}
}

func TestTruncateStr_Short(t *testing.T) {
	result := truncateStr("hello", 10)
	if result != "hello" {
		t.Errorf("truncateStr = %q, want %q", result, "hello")
	}
}

func TestTruncateStr_Long(t *testing.T) {
	result := truncateStr("hello world this is a long string", 10)
	expected := "hello worl..."
	if result != expected {
		t.Errorf("truncateStr = %q, want %q", result, expected)
	}
}

func TestTruncateStr_Chinese(t *testing.T) {
	result := truncateStr("你好世界这是一个很长的字符串用于测试", 5)
	expected := "你好世界这..."
	if result != expected {
		t.Errorf("truncateStr = %q, want %q", result, expected)
	}
}

func TestTruncateStr_Exact(t *testing.T) {
	result := truncateStr("12345", 5)
	if result != "12345" {
		t.Errorf("truncateStr = %q, want %q", result, "12345")
	}
}

func TestFilterMessages_TrivialUser(t *testing.T) {
	b, _ := NewMem9Backend(Config{Mem9: Mem9Config{APIKey: "k"}})
	msgs := []Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "Hi! How can I help?"},
	}
	result := b.filterMessages(msgs, ContentFilter{MaxMessages: 10, MaxBytes: 100000})
	if len(result) != 0 {
		t.Errorf("filterMessages with trivial user: got %d messages, want 0", len(result))
	}
}

func TestFilterMessages_NoFilter(t *testing.T) {
	b, _ := NewMem9Backend(Config{Mem9: Mem9Config{APIKey: "k"}})
	msgs := []Message{
		{Role: "user", Content: "What are goroutines?"},
		{Role: "assistant", Content: "Goroutines are lightweight threads..."},
	}
	result := b.filterMessages(msgs, ContentFilter{MaxMessages: 10, MaxBytes: 100000})
	if len(result) != 2 {
		t.Fatalf("got %d messages, want 2", len(result))
	}
	if result[0].Role != "user" {
		t.Errorf("first message role = %q, want %q", result[0].Role, "user")
	}
}

func TestFilterMessages_StripNoise(t *testing.T) {
	b, _ := NewMem9Backend(Config{Mem9: Mem9Config{APIKey: "k"}})
	msgs := []Message{
		{Role: "user", Content: "<system-reminder>\nreminder\n</system-reminder>\nReal question?"},
	}
	result := b.filterMessages(msgs, ContentFilter{MaxMessages: 10, MaxBytes: 100000})
	if len(result) != 1 {
		t.Fatalf("got %d messages, want 1", len(result))
	}
	if result[0].Content != "Real question?" {
		t.Errorf("Content = %q, want %q", result[0].Content, "Real question?")
	}
}

func TestFilterMessages_StripMemoriesBlock(t *testing.T) {
	b, _ := NewMem9Backend(Config{Mem9: Mem9Config{APIKey: "k"}})
	msgs := []Message{
		{Role: "assistant", Content: "I recall<relevant-memories>\nold memory\n</relevant-memories> something useful"},
	}
	result := b.filterMessages(msgs, ContentFilter{MaxMessages: 10, MaxBytes: 100000})
	if len(result) != 1 {
		t.Fatalf("got %d messages, want 1", len(result))
	}
	if result[0].Content != "I recall something useful" {
		t.Errorf("Content = %q, want %q", result[0].Content, "I recall something useful")
	}
}

func TestFilterMessages_AllNoise(t *testing.T) {
	b, _ := NewMem9Backend(Config{Mem9: Mem9Config{APIKey: "k"}})
	msgs := []Message{
		{Role: "assistant", Content: "<system-reminder>\nx\n</system-reminder>"},
	}
	result := b.filterMessages(msgs, ContentFilter{MaxMessages: 10, MaxBytes: 100000})
	if len(result) != 0 {
		t.Errorf("filterMessages all-noise: got %d messages, want 0", len(result))
	}
}

func TestFilterMessages_MaxMessages(t *testing.T) {
	b, _ := NewMem9Backend(Config{Mem9: Mem9Config{APIKey: "k"}})
	msgs := []Message{
		{Role: "user", Content: "msg1"},
		{Role: "assistant", Content: "msg2"},
		{Role: "user", Content: "msg3"},
		{Role: "assistant", Content: "msg4"},
	}
	result := b.filterMessages(msgs, ContentFilter{MaxMessages: 2, MaxBytes: 100000})
	if len(result) != 2 {
		t.Fatalf("got %d messages, want 2", len(result))
	}
	// Should keep the newest 2 messages (from tail)
	if result[0].Content != "msg3" {
		t.Errorf("first = %q, want msg3", result[0].Content)
	}
	if result[1].Content != "msg4" {
		t.Errorf("second = %q, want msg4", result[1].Content)
	}
}

func TestFilterMessages_MaxBytes(t *testing.T) {
	b, _ := NewMem9Backend(Config{Mem9: Mem9Config{APIKey: "k"}})
	msgs := []Message{
		{Role: "user", Content: "short"},
		{Role: "assistant", Content: "this is a much longer message that should exceed budget"},
	}
	result := b.filterMessages(msgs, ContentFilter{MaxMessages: 10, MaxBytes: 30})
	// Should keep at most the last message that fits
	if len(result) == 0 {
		t.Fatal("expected at least 1 message")
	}
	// The newest message "this is a..." is 52 bytes, doesn't fit in 30, but
	// filterMessages adds the first message from tail regardless of size.
	// Actually looking at the code: it adds messages from tail until budget exceeded.
	// First from tail: "this is a much longer message..." = 52 bytes > 30.
	// But it only checks budget after first message when len(result)>0.
	if len(result) > 1 {
		t.Errorf("expected at most 1 message due to byte budget, got %d", len(result))
	}
}
