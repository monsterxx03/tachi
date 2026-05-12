package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/agent/wdctx"
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
func (t *ReadTool) Description() string { return "Read the contents of a file" }
func (t *ReadTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"path":   {Type: "string", Description: "The path to the file to read"},
		"offset": {Type: "number", Description: "Line number to start reading from (1-indexed, default: 1)"},
		"limit":  {Type: "number", Description: "Number of lines to read (default: all lines from offset)"},
	}
}
func (t *ReadTool) Required() []string { return []string{"path"} }
func (t *ReadTool) Parallel() bool     { return true }

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

	// Check if file is binary by looking for null bytes
	if isBinaryFile(content) {
		return "", fmt.Errorf("This tool cannot read binary files. The file appears to be a binary file. Please use appropriate tools for binary file analysis.")
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
