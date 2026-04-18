package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// ErrFileTooLarge creates an error when a file exceeds the size limit
func ErrFileTooLarge(actualSize, limitSize int64) error {
	return fmt.Errorf("file too large: %d bytes (limit: %d bytes)", actualSize, limitSize)
}

const maxFileSize = 256 * 1024 // 256KB

// ReadFile is the Read tool implementation
func ReadFile(args string) (string, error) {
	var argsMap struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(args), &argsMap); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if isBlockedDevicePath(argsMap.Path) {
		return "", fmt.Errorf("cannot read from blocked device path: %s", argsMap.Path)
	}

	// Check file size before reading
	info, err := os.Stat(argsMap.Path)
	if err != nil {
		return "", fmt.Errorf("failed to stat file: %w", err)
	}
	if info.Size() > maxFileSize {
		return "", ErrFileTooLarge(info.Size(), maxFileSize)
	}

	content, err := os.ReadFile(argsMap.Path)
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

	return strings.Join(lines[start:end], "\n"), nil
}

var blockedDevicePaths = map[string]bool{
	"/dev/zero":    true,
	"/dev/random":  true,
	"/dev/urandom":  true,
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
		// Check endsWith for /fd/0, /fd/1, /fd/2
		if len(filePath) >= 10 && (filePath[len(filePath)-5:] == "/fd/0" || filePath[len(filePath)-5:] == "/fd/1" || filePath[len(filePath)-5:] == "/fd/2") {
			return true
		}
	}
	return false
}

// isBinaryFile checks if the content appears to be binary
// by looking for null bytes (\x00)
func isBinaryFile(content []byte) bool {
	// Check first 8000 bytes for null bytes (common heuristic)
	checkLen := len(content)
	if checkLen > 8000 {
		checkLen = 8000
	}
	for i := 0; i < checkLen; i++ {
		if content[i] == 0 {
			return true
		}
	}
	return false
}
