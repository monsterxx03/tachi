package weixin

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
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

// --- saveFile / filesDir (uses t.TempDir via config.SetBaseDir) ---

func TestSaveFileAndFilesDir(t *testing.T) {
	// Redirect base dir to a temp directory that auto-cleans on test end.
	origBase := config.BaseDir()
	config.SetBaseDir(t.TempDir())
	t.Cleanup(func() { config.SetBaseDir(origBase) })

	ch := &Channel{
		accountID: "test-bot@im.bot",
		cli:       newClient(),
		logger:    logger.Default().With("source", "channel:weixin-test"),
	}

	// Verify filesDir path structure: <tmp>/weixin/files/test-bot-im-bot
	dir := ch.filesDir()
	expectedSuffix := "/weixin/files/test-bot-im-bot"
	if !strings.HasSuffix(dir, expectedSuffix) {
		t.Errorf("filesDir %q should end with %q", dir, expectedSuffix)
	}

	// Save a test file and verify content.
	userID := "wx_user_123"
	data := []byte("hello, this is a test file")
	path, err := ch.saveFile(userID, "test.txt", data)
	if err != nil {
		t.Fatalf("saveFile: %v", err)
	}

	readback, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if !bytes.Equal(readback, data) {
		t.Errorf("saved file content mismatch: got %q, want %q", readback, data)
	}

	// Verify path contains user ID.
	if !strings.Contains(filepath.ToSlash(path), normalizeID(userID)) {
		t.Errorf("saved path %q should contain user dir %q", path, normalizeID(userID))
	}

	// Verify path contains original filename as prefix.
	_, file := filepath.Split(path)
	if !strings.HasPrefix(file, "test.txt-") {
		t.Errorf("saved filename %q should start with 'test.txt-'", file)
	}

	// Save with empty userID — saves to account-level dir, no error.
	path2, err := ch.saveFile("", "empty.txt", []byte("data"))
	if err != nil {
		t.Fatalf("saveFile with empty userID should not error: %v", err)
	}
	os.Remove(path2)
}

func TestFilesDirUsesWeixinStateDir(t *testing.T) {
	tmpDir := t.TempDir()
	origBase := config.BaseDir()
	config.SetBaseDir(tmpDir)
	t.Cleanup(func() { config.SetBaseDir(origBase) })

	ch := &Channel{
		accountID: "my-bot",
		cli:       newClient(),
		logger:    logger.Default().With("source", "channel:weixin-test"),
	}

	dir := ch.filesDir()
	// Should be under config.WeixinStateDir()/files/...
	expectedPrefix := config.WeixinStateDir()
	if !strings.HasPrefix(dir, expectedPrefix) {
		t.Errorf("filesDir %q should start with WeixinStateDir %q", dir, expectedPrefix)
	}
	// Verify the file actually gets created.
	path, err := ch.saveFile("u1", "x.txt", []byte("hello"))
	if err != nil {
		t.Fatalf("saveFile: %v", err)
	}
	if !strings.HasPrefix(path, tmpDir) {
		t.Errorf("saved file %q should be under tmpDir %q", path, tmpDir)
	}
	os.Remove(path)
}
