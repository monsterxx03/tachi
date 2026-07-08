package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/llm"
)

const maxFileSize = 256 * 1024 // 256KB

// ErrFileTooLarge creates an error when a file exceeds the size limit
func ErrFileTooLarge(actualSize, limitSize int64) error {
	return fmt.Errorf("file too large: %d bytes (limit: %d bytes)", actualSize, limitSize)
}

type cachedEntry struct {
	mtime time.Time
	size  int64
}

// ReadTool reads the contents of a file
type ReadTool struct {
	mu    sync.RWMutex
	cache map[string]cachedEntry
}

// NewReadTool creates a ReadTool with initialized cache state.
func NewReadTool() *ReadTool {
	return &ReadTool{
		cache: make(map[string]cachedEntry),
	}
}

func (t *ReadTool) Name() string        { return ToolNameRead }
func (t *ReadTool) Description() string {
	return "Read the contents of a file. For image files (png, jpg, gif, webp), " +
		"returns a description and makes the image available to vision-capable models."
}
func (t *ReadTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"path":   {Type: "string", Description: "The path to the file to read"},
		"offset": {Type: "number", Description: "Line number to start reading from (1-indexed, default: 1)"},
		"limit":  {Type: "number", Description: "Number of lines to read (default: all lines from offset)"},
	}
}
func (t *ReadTool) Required() []string { return []string{"path"} }
func (t *ReadTool) Parallel() bool     { return true }

// imageMimeByExt maps lowercase file extensions (including the leading dot)
// to MIME types for image formats supported by common LLM providers.
var imageMimeByExt = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

// imageMagicBytes contains magic byte signatures for image format detection.
// Checked after extension match to avoid false positives.
var imageMagicBytes = map[string][]byte{
	"image/png":  {0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A},
	"image/jpeg": {0xFF, 0xD8, 0xFF},
	"image/gif":  {0x47, 0x49, 0x46, 0x38}, // "GIF8"
	"image/webp": nil, // webp detected by RIFF+WEBP; handled separately
}

// detectImageMime determines whether a file is a supported image format.
// Uses file extension first, then validates with magic bytes.
// Returns the MIME type or empty string if not a supported image.
func detectImageMime(filePath string, data []byte) string {
	ext := strings.ToLower(path.Ext(filePath))
	mime, ok := imageMimeByExt[ext]
	if !ok {
		return ""
	}

	// Validate with magic bytes
	if expected, ok := imageMagicBytes[mime]; ok && expected != nil {
		if len(data) < len(expected) || !bytes.Equal(data[:len(expected)], expected) {
			return ""
		}
	}

	// WebP: check RIFF header (first 4 bytes) + WEBP at offset 8
	if mime == "image/webp" {
		if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
			return ""
		}
	}

	return mime
}

func (t *ReadTool) ExecuteContext(ctx context.Context, args string) (string, error) {
	var argsMap struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(args), &argsMap); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	filePath := argsMap.Path
	if !filepath.IsAbs(filePath) {
		filePath = filepath.Join(wdctx.Dir(ctx), filePath)
	}

	if isBlockedDevicePath(filePath) {
		return "", fmt.Errorf("cannot read from blocked device path: %s", argsMap.Path)
	}

	// Check file size before reading
	info, err := os.Stat(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to stat file: %w", err)
	}
	if info.Size() > maxFileSize {
		return "", ErrFileTooLarge(info.Size(), maxFileSize)
	}

	// Check cache: if the file hasn't changed since last read, return a short hint
	key := readCacheKey(filePath, argsMap.Offset, argsMap.Limit)
	t.mu.RLock()
	if cached, ok := t.cache[key]; ok {
		if cached.mtime.Equal(info.ModTime()) && cached.size == info.Size() {
			t.mu.RUnlock()
			return formatCacheHit(info.ModTime()), nil
		}
	}
	t.mu.RUnlock()

	content, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %w", err)
	}

	// Check for image files first (by extension + magic bytes), before the
	// generic binary check. This ensures images are always detected even if
	// they happen to lack null bytes in the first 8KB (some JPEGs, etc.).
	if mime := detectImageMime(filePath, content); mime != "" {
		encoded := base64.StdEncoding.EncodeToString(content)
		AddImageParts(ctx, []llm.ContentPart{
			{
				Type:      llm.ContentPartImage,
				MediaType: mime,
				Data:      encoded,
			},
		})
		return fmt.Sprintf("[Image: %s, %s, %d bytes, %d base64 chars]",
			filepath.Base(filePath), mime, len(content), len(encoded)), nil
	}

	// Not an image — reject other binary files.
	if isBinaryFile(content) {
		return "", fmt.Errorf("this tool cannot read binary files; the file appears to be a binary file, please use appropriate tools for binary file analysis")
	}

	lines := strings.Split(string(content), "\n")

	// Default offset is 1 (1-indexed), convert to 0-indexed
	start := 0
	if argsMap.Offset > 0 {
		start = argsMap.Offset - 1
	}
	if start >= len(lines) {
		return "", nil
	}

	end := len(lines)
	if argsMap.Limit > 0 {
		end = start + argsMap.Limit
	}
	if end > len(lines) {
		end = len(lines)
	}

	result := strings.Join(lines[start:end], "\n")

	// Update cache
	t.mu.Lock()
	t.cache[key] = cachedEntry{
		mtime: info.ModTime(),
		size:  info.Size(),
	}
	t.mu.Unlock()

	return result, nil
}

func readCacheKey(path string, offset, limit int) string {
	return fmt.Sprintf("%s|%d|%d", path, offset, limit)
}

func formatCacheHit(mtime time.Time) string {
	return fmt.Sprintf(
		"[File unchanged since last read (mtime: %s). "+
			"The content is identical to your earlier ReadFile result for this path. "+
			"Please refer to your previous tool call output.]",
		mtime.Format("2006-01-02T15:04:05-07:00"),
	)
}

var blockedDevicePaths = map[string]bool{
	"/dev/zero":    true,
	"/dev/random":  true,
	"/dev/urandom": true,
	"/dev/full":    true,
	"/dev/stdin":   true,
	"/dev/tty":     true,
	"/dev/console": true,
	"/dev/stdout":  true,
	"/dev/stderr":  true,
	"/dev/fd/0":    true,
	"/dev/fd/1":    true,
	"/dev/fd/2":    true,
}

func isBlockedDevicePath(filePath string) bool {
	if blockedDevicePaths[filePath] {
		return true
	}
	// /proc/self/fd/0-2 and /proc/<pid>/fd/0-2 are Linux aliases for stdio
	if len(filePath) >= 11 && filePath[:6] == "/proc/" {
		if len(filePath) >= 10 && (filePath[len(filePath)-5:] == "/fd/0" || filePath[len(filePath)-5:] == "/fd/1" || filePath[len(filePath)-5:] == "/fd/2") {
			return true
		}
	}
	return false
}

// isBinaryFile checks if the content appears to be binary
// by looking for null bytes (\x00)
func isBinaryFile(content []byte) bool {
	checkLen := min(len(content), 8000)
	return bytes.Contains(content[:checkLen], []byte{0})
}
