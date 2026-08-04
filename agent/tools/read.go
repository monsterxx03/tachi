package tools

import (
	"bufio"
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

	"github.com/coder/acp-go-sdk"
	"github.com/monsterxx03/tachi/agent/acpctx"
	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/llm"
)

const (
	maxFileSize  = 256 * 1024 // 256KB
	maxLineChars = 2000       // per-line truncation for context safety
)

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

func (t *ReadTool) Name() string { return ToolNameRead }
func (t *ReadTool) Description() string {
	return "Read the contents of a text file. For image files (png, jpg, gif, webp), returns a " +
		"placeholder and makes the image available to vision-capable models. " +
		"Use offset (1-based; negative reads from the end, e.g. -100 = last 100 lines) and limit " +
		"to page through large files — truncated reads include a hint on how to continue. " +
		"Directories are not readable here; use the glob tool or bash ls instead. Do not use bash cat."
}
func (t *ReadTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"path":   {Type: "string", Description: "The path to the file to read"},
		"offset": {Type: "integer", Description: "Line number to start reading from (1-indexed, default: 1; negative reads from the end, e.g. -100 = last 100 lines)"},
		"limit":  {Type: "integer", Description: "Number of lines to read (default: all lines from offset)", Minimum: new(1.0)},
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
	"image/webp": nil,                      // webp detected by RIFF+WEBP; handled separately
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

	// In ACP mode, route through ACP client so Zed shows which file is being read.
	if conn := acpctx.Conn(ctx); conn != nil {
		resp, err := conn.ReadTextFile(ctx, acp.ReadTextFileRequest{
			SessionId: acpctx.SessionID(ctx),
			Path:      filePath,
		})
		if err != nil {
			return "", fmt.Errorf("ACP readTextFile failed: %w", err)
		}

		// Check for image files (by extension + magic bytes)
		content := []byte(resp.Content)
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

		// Reject other binary files.
		if isBinaryFile(content) {
			return "", fmt.Errorf("this tool cannot read binary files; the file appears to be a binary file, please use appropriate tools for binary file analysis")
		}

		return formatReadOutput(strings.Split(resp.Content, "\n"), argsMap.Offset, argsMap.Limit), nil
	}

	// Check file size before reading.
	// When limit is specified, the output is bounded, so we skip the size check.
	// Without limit (full file or offset-to-EOF), enforce maxFileSize to prevent
	// blowing up the context window.
	info, err := os.Stat(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to stat file: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("cannot read directory: %s. Use glob to find files or bash ls to list contents", argsMap.Path)
	}
	if argsMap.Limit <= 0 && info.Size() > maxFileSize {
		err := ErrFileTooLarge(info.Size(), maxFileSize)
		if lines := countLines(filePath); lines > 0 {
			return "", fmt.Errorf("%w. File has ~%d lines — use offset/limit to read it in parts", err, lines)
		}
		return "", fmt.Errorf("%w. Use offset/limit to read it in parts", err)
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

	result := formatReadOutput(strings.Split(string(content), "\n"), argsMap.Offset, argsMap.Limit)

	// Update cache
	t.mu.Lock()
	t.cache[key] = cachedEntry{
		mtime: info.ModTime(),
		size:  info.Size(),
	}
	t.mu.Unlock()

	return result, nil
}

// formatReadOutput computes the line window (with negative-offset tail reads),
// truncates over-long lines, and appends actionable hints when the read was
// truncated or offset was out of range.
func formatReadOutput(lines []string, offset, limit int) string {
	total := len(lines)

	// strings.Split on content ending with "\n" yields a phantom trailing
	// empty element; drop it so line counts and tail offsets match what the
	// user sees (e.g. offset=-1 must return the real last line, not "").
	if total > 1 && lines[total-1] == "" {
		lines = lines[:total-1]
		total--
	}

	start := 0
	switch {
	case offset < 0:
		start = total + offset
		if start < 0 {
			start = 0
		}
	case offset > 0:
		start = offset - 1
	}
	if start >= total {
		return fmt.Sprintf("[Offset %d is beyond the end of the file (%d lines total).]", offset, total)
	}

	end := total
	if limit > 0 {
		end = start + limit
		if end > total {
			end = total
		}
	}

	// Truncate over-long lines (minified files etc.) so a single line can't
	// blow up the context window. Byte-length pre-filter avoids the []rune
	// allocation for the common short-line case (rune count ≤ byte count).
	truncatedLines := 0
	out := make([]string, 0, end-start)
	for _, line := range lines[start:end] {
		if len(line) > maxLineChars {
			if runes := []rune(line); len(runes) > maxLineChars {
				line = string(runes[:maxLineChars]) + "..."
				truncatedLines++
			}
		}
		out = append(out, line)
	}

	result := strings.Join(out, "\n")
	var hints []string
	if end < total {
		hints = append(hints, fmt.Sprintf("Showing lines %d-%d of %d. Use offset=%d to continue.", start+1, end, total, end+1))
	}
	if truncatedLines > 0 {
		hints = append(hints, fmt.Sprintf("%d long line(s) truncated to %d chars", truncatedLines, maxLineChars))
	}
	if len(hints) > 0 {
		result += "\n\n[" + strings.Join(hints, "; ") + "]"
	}
	return result
}

// countLines streams a file counting newlines with a fixed-size buffer, so
// memory stays bounded regardless of line length. Returns 0 when the count
// could not be determined.
const maxCountedLines = 1_000_000

func countLines(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64*1024)
	buf := make([]byte, 64*1024)
	n := 0
	for n < maxCountedLines {
		read, err := r.Read(buf)
		if read > 0 {
			n += bytes.Count(buf[:read], []byte{'\n'})
		}
		if err != nil {
			break
		}
	}
	return n
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
