package weixin

import (
	"testing"

	"github.com/monsterxx03/tachi/pkg/channel"
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

func TestExtractMediaItems_File(t *testing.T) {
	items := []MessageItem{
		{
			Type: MessageItemTypeFile,
			FileItem: &FileItem{
				FileName: "test.go",
				Len:      "1024",
				Media:    MediaData{EncryptQueryParam: "abc", AESKey: "a2V5MTIzNDU2Nzg5MDEyMzQ1Ng=="},
			},
		},
	}

	refs := extractMediaItems(items)
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref, got %d", len(refs))
	}
	if refs[0].Type != MessageItemTypeFile {
		t.Errorf("expected file type, got %d", refs[0].Type)
	}
	if refs[0].FileName != "test.go" {
		t.Errorf("expected test.go, got %s", refs[0].FileName)
	}
	if refs[0].RawSize != 1024 {
		t.Errorf("expected raw size 1024, got %d", refs[0].RawSize)
	}
}

func TestExtractMediaItems_Image(t *testing.T) {
	items := []MessageItem{
		{
			Type:      MessageItemTypeImage,
			ImageItem: &MediaItem{AESKey: "abcdef1234567890abcdef1234567890"},
		},
	}

	refs := extractMediaItems(items)
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref, got %d", len(refs))
	}
	if refs[0].Type != MessageItemTypeImage {
		t.Errorf("expected image type, got %d", refs[0].Type)
	}
	if refs[0].FileName != "image" {
		t.Errorf("expected 'image', got %s", refs[0].FileName)
	}
	if refs[0].AESKey != "abcdef1234567890abcdef1234567890" {
		t.Errorf("aes key mismatch")
	}
}

func TestExtractMediaItems_Mixed(t *testing.T) {
	items := []MessageItem{
		{
			Type:     MessageItemTypeText,
			TextItem: &TextItem{Text: "hello"},
		},
		{
			Type: MessageItemTypeFile,
			FileItem: &FileItem{
				FileName: "data.csv",
				Media:    MediaData{AESKey: "a2V5MTIzNDU2Nzg5MDEyMzQ1Ng=="},
			},
		},
		{
			Type:      MessageItemTypeImage,
			ImageItem: &MediaItem{MidSize: 500},
		},
	}

	refs := extractMediaItems(items)
	if len(refs) != 2 {
		t.Fatalf("expected 2 refs, got %d", len(refs))
	}
	if refs[0].Type != MessageItemTypeFile {
		t.Errorf("first ref should be file, got %d", refs[0].Type)
	}
	if refs[1].Type != MessageItemTypeImage {
		t.Errorf("second ref should be image, got %d", refs[1].Type)
	}
}

func TestExtractMediaItems_NoMedia(t *testing.T) {
	items := []MessageItem{
		{
			Type:     MessageItemTypeText,
			TextItem: &TextItem{Text: "just text"},
		},
		{
			Type:      MessageItemTypeVoice,
			VoiceItem: &VoiceItem{Text: "voice note"},
		},
	}

	refs := extractMediaItems(items)
	if len(refs) != 0 {
		t.Errorf("expected 0 refs, got %d", len(refs))
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

func TestChannelAttachmentToILinkMediaType(t *testing.T) {
	tests := []struct {
		at       channel.AttachmentType
		expected int
	}{
		{channel.AttachmentTypeImage, MediaTypeImage},
		{channel.AttachmentTypeFile, MediaTypeFile},
		{channel.AttachmentTypeText, MediaTypeFile}, // text files go as MediaTypeFile
	}

	for _, tt := range tests {
		got := channelAttachmentToILinkMediaType(tt.at)
		if got != tt.expected {
			t.Errorf("channelAttachmentToILinkMediaType(%q) = %d, want %d", tt.at, got, tt.expected)
		}
	}
}
