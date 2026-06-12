package tools

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/llm"
)

func TestReadTool(t *testing.T) {
	tool := NewReadTool()

	// Create a test file
	content := "Hello, World!"
	err := os.WriteFile("/tmp/test_read.txt", []byte(content), 0644)
	if err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}
	defer os.Remove("/tmp/test_read.txt")

	// Test Read
	result, err := tool.ExecuteContext(context.TODO(), `{"path": "/tmp/test_read.txt"}`)
	if err != nil {
		t.Fatalf("ReadTool.Execute failed: %v", err)
	}
	if result != content {
		t.Errorf("Expected %q, got %q", content, result)
	}
}

func TestReadToolNotFound(t *testing.T) {
	tool := NewReadTool()
	_, err := tool.ExecuteContext(context.TODO(), `{"path": "/nonexistent/file.txt"}`)
	if err == nil {
		t.Error("Expected error for nonexistent file")
	}
}

func TestReadToolWithOffsetAndLimit(t *testing.T) {
	tool := NewReadTool()

	// Create a test file with 10 lines
	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, fmt.Sprintf("line%d", i))
	}
	content := strings.Join(lines, "\n")
	err := os.WriteFile("/tmp/test_read_offset.txt", []byte(content), 0644)
	if err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}
	defer os.Remove("/tmp/test_read_offset.txt")

	tests := []struct {
		name   string
		offset int
		limit  int
		want   string
	}{
		{"default (no offset/limit)", 0, 0, content},
		{"offset only", 3, 0, "line3\nline4\nline5\nline6\nline7\nline8\nline9\nline10"},
		{"limit only", 0, 5, "line1\nline2\nline3\nline4\nline5"},
		{"offset and limit", 3, 4, "line3\nline4\nline5\nline6"},
		{"offset 1 (1-indexed)", 1, 3, "line1\nline2\nline3"},
		{"offset beyond end", 20, 0, ""},
		{"limit exceeds remaining", 8, 10, "line8\nline9\nline10"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var args string
			if tt.offset > 0 && tt.limit > 0 {
				args = fmt.Sprintf(`{"path": "/tmp/test_read_offset.txt", "offset": %d, "limit": %d}`, tt.offset, tt.limit)
			} else if tt.offset > 0 {
				args = fmt.Sprintf(`{"path": "/tmp/test_read_offset.txt", "offset": %d}`, tt.offset)
			} else if tt.limit > 0 {
				args = fmt.Sprintf(`{"path": "/tmp/test_read_offset.txt", "limit": %d}`, tt.limit)
			} else {
				args = `{"path": "/tmp/test_read_offset.txt"}`
			}

			result, err := tool.ExecuteContext(context.TODO(), args)
			if err != nil {
				t.Fatalf("ReadTool.Execute failed: %v", err)
			}
			if result != tt.want {
				t.Errorf("Expected %q, got %q", tt.want, result)
			}
		})
	}
}

func TestReadToolBinary(t *testing.T) {
	tool := NewReadTool()

	// Create a binary file with null bytes
	binaryContent := []byte{0x00, 0x01, 0x02, 'h', 'e', 'l', 'l', 'o'}
	err := os.WriteFile("/tmp/test_binary.bin", binaryContent, 0644)
	if err != nil {
		t.Fatalf("Failed to create binary test file: %v", err)
	}
	defer os.Remove("/tmp/test_binary.bin")

	_, err = tool.ExecuteContext(context.TODO(), `{"path": "/tmp/test_binary.bin"}`)
	if err == nil {
		t.Error("Expected error for binary file")
	}
	if !strings.Contains(err.Error(), "binary file") {
		t.Errorf("Expected binary file error, got: %v", err)
	}
}

func TestReadToolTooLarge(t *testing.T) {
	tool := NewReadTool()

	// Create a file larger than 256KB
	largeContent := make([]byte, 257*1024) // 257KB
	for i := range largeContent {
		largeContent[i] = 'a'
	}
	err := os.WriteFile("/tmp/test_large.txt", largeContent, 0644)
	if err != nil {
		t.Fatalf("Failed to create large test file: %v", err)
	}
	defer os.Remove("/tmp/test_large.txt")

	_, err = tool.ExecuteContext(context.TODO(), `{"path": "/tmp/test_large.txt"}`)
	if err == nil {
		t.Error("Expected error for large file")
	}
	if !strings.Contains(err.Error(), "file too large") {
		t.Errorf("Expected 'file too large' error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "263168") {
		t.Errorf("Expected actual size in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "262144") {
		t.Errorf("Expected limit size in error, got: %v", err)
	}
}

func TestReadToolAtLimit(t *testing.T) {
	tool := NewReadTool()

	// Create a file exactly at 256KB
	limitContent := make([]byte, 256*1024) // exactly 256KB
	for i := range limitContent {
		limitContent[i] = 'a'
	}
	err := os.WriteFile("/tmp/test_at_limit.txt", limitContent, 0644)
	if err != nil {
		t.Fatalf("Failed to create limit test file: %v", err)
	}
	defer os.Remove("/tmp/test_at_limit.txt")

	// Should not error - exactly at limit
	_, err = tool.ExecuteContext(context.TODO(), `{"path": "/tmp/test_at_limit.txt"}`)
	if err != nil {
		t.Errorf("Should not error at exactly 256KB, got: %v", err)
	}
}

func TestReadToolCacheHit(t *testing.T) {
	tool := NewReadTool()

	content := "Hello, World!"
	err := os.WriteFile("/tmp/test_read_cache.txt", []byte(content), 0644)
	if err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}
	defer os.Remove("/tmp/test_read_cache.txt")

	// First read — should return full content
	result1, err := tool.ExecuteContext(context.TODO(), `{"path": "/tmp/test_read_cache.txt"}`)
	if err != nil {
		t.Fatalf("First read failed: %v", err)
	}
	if result1 != content {
		t.Errorf("Expected %q, got %q", content, result1)
	}

	// Second read — file unchanged, should return cache hint
	result2, err := tool.ExecuteContext(context.TODO(), `{"path": "/tmp/test_read_cache.txt"}`)
	if err != nil {
		t.Fatalf("Second read failed: %v", err)
	}
	if !strings.Contains(result2, "File unchanged since last read") {
		t.Errorf("Expected cache hit message, got: %q", result2)
	}
	if strings.Contains(result2, content) {
		t.Error("Cache hit should not contain full file content")
	}
}

func TestReadToolCacheMissAfterModify(t *testing.T) {
	tool := NewReadTool()

	content1 := "Hello"
	err := os.WriteFile("/tmp/test_read_modify.txt", []byte(content1), 0644)
	if err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}
	defer os.Remove("/tmp/test_read_modify.txt")

	// First read
	_, err = tool.ExecuteContext(context.TODO(), `{"path": "/tmp/test_read_modify.txt"}`)
	if err != nil {
		t.Fatalf("First read failed: %v", err)
	}

	// Modify file — sleep to ensure mtime changes
	time.Sleep(50 * time.Millisecond)
	content2 := "Hello, World!"
	err = os.WriteFile("/tmp/test_read_modify.txt", []byte(content2), 0644)
	if err != nil {
		t.Fatalf("Failed to modify test file: %v", err)
	}

	// Second read — should return new content (cache miss)
	result, err := tool.ExecuteContext(context.TODO(), `{"path": "/tmp/test_read_modify.txt"}`)
	if err != nil {
		t.Fatalf("Second read failed: %v", err)
	}
	if result != content2 {
		t.Errorf("Expected %q, got %q", content2, result)
	}
}

func TestReadToolDifferentOffsetNotCached(t *testing.T) {
	tool := NewReadTool()

	// Create a 10-line file
	lines := make([]string, 10)
	for i := range lines {
		lines[i] = fmt.Sprintf("line%d", i+1)
	}
	content := strings.Join(lines, "\n")
	err := os.WriteFile("/tmp/test_read_offset_cache.txt", []byte(content), 0644)
	if err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}
	defer os.Remove("/tmp/test_read_offset_cache.txt")

	// Read full file
	_, err = tool.ExecuteContext(context.TODO(), `{"path": "/tmp/test_read_offset_cache.txt"}`)
	if err != nil {
		t.Fatalf("First read failed: %v", err)
	}

	// Read with offset=5 — different cache key, should NOT hit cache
	result, err := tool.ExecuteContext(context.TODO(), `{"path": "/tmp/test_read_offset_cache.txt", "offset": 5}`)
	if err != nil {
		t.Fatalf("Offset read failed: %v", err)
	}
	expected := "line5\nline6\nline7\nline8\nline9\nline10"
	if result != expected {
		t.Errorf("Expected %q, got %q", expected, result)
	}
}

func TestReadToolConcurrentCache(t *testing.T) {
	tool := NewReadTool()

	content := "concurrent test"
	err := os.WriteFile("/tmp/test_read_concurrent.txt", []byte(content), 0644)
	if err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}
	defer os.Remove("/tmp/test_read_concurrent.txt")

	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			result, err := tool.ExecuteContext(context.TODO(), `{"path": "/tmp/test_read_concurrent.txt"}`)
			if err != nil {
				t.Errorf("Concurrent read failed: %v", err)
				return
			}
			if result == "" {
				t.Error("Got empty result")
			}
		})
	}
	wg.Wait()
}

// minimalPNG returns the bytes of a valid 1x1 pixel grayscale PNG.
// This is a hand-crafted minimal PNG to avoid importing image/png.
func minimalPNG() []byte {
	// Hex dump of a 1x1 grayscale PNG (68 bytes):
	png := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, // signature
		0x00, 0x00, 0x00, 0x0D, // IHDR length = 13
		0x49, 0x48, 0x44, 0x52, // "IHDR"
		0x00, 0x00, 0x00, 0x01, // width = 1
		0x00, 0x00, 0x00, 0x01, // height = 1
		0x08,                         // bit depth = 8
		0x00,                         // color type = grayscale
		0x00,                         // compression
		0x00,                         // filter
		0x00,                         // interlace
		0x6C, 0xE0, 0xDE, 0x2D, // IHDR CRC
		0x00, 0x00, 0x00, 0x0B, // IDAT length = 11
		0x49, 0x44, 0x41, 0x54, // "IDAT"
		0x78, 0x9C, 0x63, 0x60, 0x00, 0x00, 0x00, 0x02, 0x00, 0x01, // zlib data
		0x73, 0x75, 0x01, 0x8A, // IDAT CRC
		0x00, 0x00, 0x00, 0x00, // IEND length = 0
		0x49, 0x45, 0x4E, 0x44, // "IEND"
		0xAE, 0x42, 0x60, 0x82, // IEND CRC
	}
	return png
}

func TestReadToolImage(t *testing.T) {
	tool := NewReadTool()

	pngData := minimalPNG()
	err := os.WriteFile("/tmp/test_read_image.png", pngData, 0644)
	if err != nil {
		t.Fatalf("Failed to create test image: %v", err)
	}
	defer os.Remove("/tmp/test_read_image.png")

	// Use context with image carrier — similar to what Registry.Invoke does.
	ctx := WithImagePartsCarrier(context.TODO())
	result, err := tool.ExecuteContext(ctx, `{"path": "/tmp/test_read_image.png"}`)
	if err != nil {
		t.Fatalf("ReadTool.Execute on image failed: %v", err)
	}

	// Should return a description, not the raw content
	if !strings.HasPrefix(result, "[Image:") {
		t.Errorf("Expected image description prefix, got: %q", result)
	}

	// Image parts should be in context
	parts := ImagePartsFromCtx(ctx)
	if len(parts) != 1 {
		t.Fatalf("Expected 1 image part, got %d", len(parts))
	}
	if parts[0].Type != llm.ContentPartImage {
		t.Errorf("Expected ContentPartImage, got %v", parts[0].Type)
	}
	if parts[0].MediaType != "image/png" {
		t.Errorf("Expected image/png, got %s", parts[0].MediaType)
	}
	if parts[0].Data == "" {
		t.Error("Expected non-empty base64 data")
	}
}

func TestDetectImageMime(t *testing.T) {
	pngData := minimalPNG()

	tests := []struct {
		name     string
		filePath string
		data     []byte
		want     string
	}{
		{"png", "test.png", pngData, "image/png"},
		{"PNG uppercase", "test.PNG", pngData, "image/png"},
		{"jpeg", "photo.jpg", []byte{0xFF, 0xD8, 0xFF, 0x00}, "image/jpeg"},
		{"jpeg ext", "photo.jpeg", []byte{0xFF, 0xD8, 0xFF, 0x00}, "image/jpeg"},
		{"gif", "anim.gif", []byte{0x47, 0x49, 0x46, 0x38, 0x39, 0x61}, "image/gif"},
		{"webp", "img.webp", []byte("RIFF\x00\x00\x00\x00WEBP"), "image/webp"},
		{"bad magic png", "fake.png", []byte{0x00, 0x00, 0x00, 0x00}, ""},
		{"unknown ext", "doc.pdf", pngData, ""},
		{"bad webp", "bad.webp", []byte("RIFF\x00\x00\x00\x00XXXX"), ""},
		{"too short", "x.png", []byte{0x89}, ""},
		{"too short webp", "x.webp", []byte("RIFF"), ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectImageMime(tt.filePath, tt.data)
			if got != tt.want {
				t.Errorf("detectImageMime(%q) = %q, want %q", tt.filePath, got, tt.want)
			}
		})
	}
}

func TestReadToolImageUnknownExtBinary(t *testing.T) {
	tool := NewReadTool()

	// A binary file with an unrecognized extension should still error
	binaryContent := []byte{0x00, 0x01, 0x02, 'h', 'e', 'l', 'l', 'o'}
	err := os.WriteFile("/tmp/test_not_image.bin", binaryContent, 0644)
	if err != nil {
		t.Fatalf("Failed to create binary test file: %v", err)
	}
	defer os.Remove("/tmp/test_not_image.bin")

	_, err = tool.ExecuteContext(context.TODO(), `{"path": "/tmp/test_not_image.bin"}`)
	if err == nil {
		t.Error("Expected error for non-image binary file")
	}
	if !strings.Contains(err.Error(), "binary file") {
		t.Errorf("Expected binary file error, got: %v", err)
	}
}
