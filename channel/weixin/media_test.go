package weixin

import (
	"testing"
)

func TestIsTextExtension(t *testing.T) {
	tests := []struct {
		filename string
		want     bool
	}{
		{"main.go", true},
		{"index.ts", true},
		{"style.css", true},
		{"data.json", true},
		{"README.md", true},
		{"script.py", true},
		{"image.jpg", false},
		{"photo.png", false},
		{"archive.zip", false},
		{"doc.pdf", false},
		{"movie.mp4", false},
		{"song.mp3", false},
		{"noext", false},
		{"Dockerfile", false},
		{"Makefile", false},
	}

	for _, tt := range tests {
		got := isTextExtension(tt.filename)
		if got != tt.want {
			t.Errorf("isTextExtension(%q) = %v, want %v", tt.filename, got, tt.want)
		}
	}
}

func TestGuessMimeType(t *testing.T) {
	tests := []struct {
		filename string
		want     string
	}{
		{"doc.pdf", "application/pdf"},
		{"archive.zip", "application/zip"},
		{"image.jpg", "image/jpeg"},
		{"photo.png", "image/png"},
		{"data.json", "application/json"},
		{"unknown.xyz", "application/octet-stream"},
		{"noext", "application/octet-stream"},
	}

	for _, tt := range tests {
		got := guessMimeType(tt.filename)
		if got != tt.want {
			t.Errorf("guessMimeType(%q) = %q, want %q", tt.filename, got, tt.want)
		}
	}
}

func TestHumanSize(t *testing.T) {
	tests := []struct {
		n    int
		want string
	}{
		{0, "0B"},
		{100, "100B"},
		{1023, "1023B"},
		{1024, "1.0KB"},
		{1536, "1.5KB"},
		{1048576, "1.0MB"},
		{1572864, "1.5MB"},
	}

	for _, tt := range tests {
		got := humanSize(tt.n)
		if got != tt.want {
			t.Errorf("humanSize(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestAttachmentTypeFromMedia(t *testing.T) {
	tests := []struct {
		mediaType int
		want      string
	}{
		{MessageItemTypeImage, "image"},
		{MessageItemTypeFile, "file"},
		{MessageItemTypeVideo, "file"},
		{MessageItemTypeText, "file"},
	}

	for _, tt := range tests {
		got := attachmentTypeFromMedia(tt.mediaType)
		if string(got) != tt.want {
			t.Errorf("attachmentTypeFromMedia(%d) = %q, want %q", tt.mediaType, got, tt.want)
		}
	}
}
