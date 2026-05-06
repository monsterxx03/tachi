package weixin

import (
	"testing"
)

func TestExtractMessageText_Plain(t *testing.T) {
	items := []MessageItem{
		{
			Type:     MessageItemTypeText,
			TextItem: &TextItem{Text: "Hello, world!"},
		},
	}

	text, hasMedia := extractMessageText(items)
	if text != "Hello, world!" {
		t.Errorf("got %q, want %q", text, "Hello, world!")
	}
	if hasMedia {
		t.Error("expected no media")
	}
}

func TestExtractMessageText_Image(t *testing.T) {
	items := []MessageItem{
		{
			Type:      MessageItemTypeImage,
			ImageItem: &MediaItem{MidSize: 100},
		},
	}

	text, hasMedia := extractMessageText(items)
	if text != "" {
		t.Errorf("expected empty text, got %q", text)
	}
	if !hasMedia {
		t.Error("expected hasMedia=true for image")
	}
}

func TestExtractMessageText_VoiceWithText(t *testing.T) {
	items := []MessageItem{
		{
			Type: MessageItemTypeVoice,
			VoiceItem: &VoiceItem{
				Text: "这是语音转文字的结果",
			},
		},
	}

	text, hasMedia := extractMessageText(items)
	if text != "这是语音转文字的结果" {
		t.Errorf("got %q", text)
	}
	if hasMedia {
		t.Error("voice with text should not set hasMedia")
	}
}

func TestExtractMessageText_VoiceWithoutText(t *testing.T) {
	items := []MessageItem{
		{
			Type:      MessageItemTypeVoice,
			VoiceItem: &VoiceItem{},
		},
	}

	text, hasMedia := extractMessageText(items)
	if text != "" {
		t.Errorf("expected empty text, got %q", text)
	}
	if !hasMedia {
		t.Error("expected hasMedia=true for voice without text")
	}
}

func TestExtractMessageText_Quoted(t *testing.T) {
	items := []MessageItem{
		{
			Type:     MessageItemTypeText,
			TextItem: &TextItem{Text: "回复内容"},
			RefMsg: &RefMessage{
				Title: "被引用的消息",
				MessageItem: MessageItem{
					Type:     MessageItemTypeText,
					TextItem: &TextItem{Text: "原始消息"},
				},
			},
		},
	}

	text, _ := extractMessageText(items)
	expected := "[引用: 被引用的消息 | 原始消息]\n回复内容"
	if text != expected {
		t.Errorf("got %q, want %q", text, expected)
	}
}

func TestExtractMessageText_MultipleItems(t *testing.T) {
	items := []MessageItem{
		{
			Type:     MessageItemTypeText,
			TextItem: &TextItem{Text: "第一条消息"},
		},
		{
			Type:      MessageItemTypeImage,
			ImageItem: &MediaItem{MidSize: 200},
		},
	}

	text, hasMedia := extractMessageText(items)
	if text != "第一条消息" {
		t.Errorf("got %q, want %q", text, "第一条消息")
	}
	if !hasMedia {
		t.Error("expected hasMedia=true when image present alongside text")
	}
}

func TestGenerateClientID(t *testing.T) {
	id1 := generateClientID()
	id2 := generateClientID()

	if id1 == id2 {
		t.Error("two generated client IDs should be different")
	}

	if len(id1) == 0 {
		t.Error("client ID should not be empty")
	}
}

func TestRandomBytes(t *testing.T) {
	b := randomBytes(16)
	if len(b) != 16 {
		t.Errorf("expected 16 bytes, got %d", len(b))
	}

	// Should not be all zeros (probabilistic but safe for 16 bytes).
	allZero := true
	for _, v := range b {
		if v != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Error("random bytes should not be all zeros")
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		input string
		n     int
		want  string
	}{
		{"hello", 10, "hello"},
		{"hello world", 8, "hello wo..."},
		{"hello", 5, "hello"},
	}

	for _, tt := range tests {
		got := truncate(tt.input, tt.n)
		if got != tt.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.n, got, tt.want)
		}
	}
}

func TestNormalizeID(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"a1b2c3d4@im.bot", "a1b2c3d4-im-bot"},
		{"wx_user@im.wechat", "wx_user-im-wechat"},
		{"simple", "simple"},
	}

	for _, tt := range tests {
		got := normalizeID(tt.input)
		if got != tt.expected {
			t.Errorf("normalizeID(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}
