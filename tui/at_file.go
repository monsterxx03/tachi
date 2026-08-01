package tui

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/container"
	"github.com/monsterxx03/tachi/pkg/fileutil"
)

// --- Cached trie ---

var (
	atFileTrie    *container.PathTrie
	atFileTrieTTL time.Time
	atFileTrieMu  sync.Mutex
)

const (
	atFileCacheDuration = 30 * time.Second
	atFileMaxResults    = 20
)

// getCachedTrie returns the path trie for the current working directory,
// cached for 30 seconds.
func (i *InputArea) getCachedTrie() (*container.PathTrie, error) {
	atFileTrieMu.Lock()
	defer atFileTrieMu.Unlock()

	if atFileTrie != nil && time.Since(atFileTrieTTL) < atFileCacheDuration {
		return atFileTrie, nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Step 1: 正常文件列表（尊重 .gitignore）
	cmd := exec.CommandContext(ctx, "rg", "--files", "--hidden", "--glob", "!.git")
	cmd.Dir = cwd
	output, err := cmd.Output()
	if err != nil {
		atFileTrie = nil
		atFileTrieTTL = time.Now()
		return nil, err
	}

	var paths []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(output)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			paths = append(paths, filepath.ToSlash(line))
		}
	}

	// Step 2: 强制包含 .tachi 目录下的所有文件（即使被 .gitignore）
	if fileutil.IsDir(filepath.Join(cwd, ".tachi")) {
		tachiCmd := exec.CommandContext(ctx, "rg", "--files", "--hidden", "--no-ignore-vcs", "--glob", "!.git", ".tachi")
		tachiCmd.Dir = cwd
		if tachiOutput, err := tachiCmd.Output(); err == nil {
			seen := container.NewSet(paths...)
			for line := range strings.SplitSeq(strings.TrimSpace(string(tachiOutput)), "\n") {
				line = strings.TrimSpace(line)
				if line != "" && !seen.Has(line) {
					paths = append(paths, filepath.ToSlash(line))
					seen.Add(line)
				}
			}
		}
	}

	t := container.NewPathTrie(paths)
	atFileTrie = t
	atFileTrieTTL = time.Now()
	i.logger.Info(context.Background(), "at_file: built trie", "files", t.FileCount())
	return t, nil
}

// --- atFileMatch type (used by both trie and immediate listing) ---

type atFileMatch struct {
	Path  string
	Score int
	IsDir bool
}

// searchAtFiles searches files matching the query using the cached path trie.
// When query is empty (just typed "@"), lists immediate files and directories
// in the current working directory.
func (i *InputArea) searchAtFiles(query string) ([]atFileMatch, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	if query == "" {
		return listCwdImmediate(cwd)
	}

	t, err := i.getCachedTrie()
	if err != nil {
		return nil, err
	}

	results := t.Search(query, atFileMaxResults)
	matches := make([]atFileMatch, len(results))
	for i, r := range results {
		matches[i] = atFileMatch{Path: r.Path, Score: r.Score, IsDir: r.IsDir}
	}
	return matches, nil
}

// listCwdImmediate returns the immediate files and directories in the given
// directory, skipping .git. Directories are listed first, then files, both
// sorted by name (case-insensitive).
func listCwdImmediate(dir string) ([]atFileMatch, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var matches []atFileMatch
	for _, entry := range entries {
		name := entry.Name()
		if name == ".git" {
			continue
		}
		matches = append(matches, atFileMatch{
			Path:  name,
			IsDir: entry.IsDir(),
		})
	}

	slices.SortFunc(matches, func(a, b atFileMatch) int {
		if a.IsDir != b.IsDir {
			if a.IsDir {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.ToLower(a.Path), strings.ToLower(b.Path))
	})

	if len(matches) > atFileMaxResults {
		matches = matches[:atFileMaxResults]
	}

	return matches, nil
}

// --- Reference resolution and message expansion ---

const maxAtInlineSize = 256 * 1024 // 256KB — only inline text files under this

// maxAtImageSize is the maximum image file size to embed as base64 (20MB).
const maxAtImageSize = 20 * 1024 * 1024

// imageExtensions maps file extensions to MIME types for supported image formats.
var imageExtensions = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".gif":  "image/gif",
	".webp": "image/webp",
}

// ExpandResult holds the result of expanding @-file references in a message.
type ExpandResult struct {
	Text   string            // The expanded text message
	Images []llm.ContentPart // Image parts extracted from @-file references
}

// ExpandAtReferences takes a user message and expands all @path references.
//
//   - Directories: inline the file listing via rg (original behavior).
//   - Text files (no null bytes, ≤256KB): inline full content with
//     --- BEGIN UNTRUSTED FILE CONTENT --- markers.
//   - Image files (jpg, png, gif, webp ≤20MB): extracted as ContentPart images
//     for multi-modal LLM input.
//   - Other binary files (PDF, Excel, etc.): annotate as [@file: path]
//     and let the LLM use Bash/ReadFile tools to parse them.
//   - Errors / not found: leave @path as-is.
func (m *Model) ExpandAtReferences(message string) ExpandResult {
	var result strings.Builder
	var images []llm.ContentPart
	expanded := false
	cwd, _ := os.Getwd()

	i := 0
	for i < len(message) {
		if message[i] == '@' && (i == 0 || message[i-1] == ' ') {
			j := i + 1
			for j < len(message) && message[j] != ' ' && message[j] != '\n' {
				j++
			}
			path := message[i+1 : j]

			if path != "" {
				fullPath := filepath.Join(cwd, path)
				info, err := os.Stat(fullPath)
				if err != nil {
					// Not found — leave as-is.
					result.WriteString("@")
					result.WriteString(path)
				} else if info.IsDir() {
					// Directory — expand file listing.
					if listing := expandDirListing(fullPath); listing != "" {
						result.WriteString("@")
						result.WriteString(path)
						result.WriteString("\n\n--- BEGIN UNTRUSTED FILE CONTENT: ")
						result.WriteString(path)
						result.WriteString(" ---\n")
						result.WriteString(listing)
						result.WriteString("\n--- END UNTRUSTED FILE CONTENT: ")
						result.WriteString(path)
						result.WriteString(" ---")
						expanded = true
					} else {
						result.WriteString("@")
						result.WriteString(path)
					}
				} else if imgPart, ok := tryReadImageFile(fullPath); ok {
					// Image file — extract as ContentPart for multi-modal input.
					images = append(images, imgPart)
					result.WriteString("[图片: ")
					result.WriteString(path)
					result.WriteString("]")
					expanded = true
				} else if content, ok := tryReadTextFile(fullPath); ok {
					// Text file — inline content.
					result.WriteString("@")
					result.WriteString(path)
					result.WriteString("\n\n--- BEGIN UNTRUSTED FILE CONTENT: ")
					result.WriteString(path)
					result.WriteString(" ---\n")
					result.WriteString(content)
					result.WriteString("\n--- END UNTRUSTED FILE CONTENT: ")
					result.WriteString(path)
					result.WriteString(" ---")
					expanded = true
				} else {
					// Binary file — annotate path, let LLM use tools.
					result.WriteString("[@file: ")
					result.WriteString(path)
					result.WriteString("]")
					expanded = true
				}
			} else {
				result.WriteByte('@')
			}
			i = j
		} else {
			result.WriteByte(message[i])
			i++
		}
	}

	if expanded {
		m.logger.Info(context.Background(), "at_file: expanded @ references in message", "images", len(images))
	}

	return ExpandResult{
		Text:   result.String(),
		Images: images,
	}
}

// tryReadImageFile reads an image file and returns a ContentPart if it's a
// supported image format (JPEG, PNG, GIF, WebP) and within the size limit.
// Returns ok=false for non-image files, oversized files, or read errors.
func tryReadImageFile(fullPath string) (llm.ContentPart, bool) {
	ext := strings.ToLower(filepath.Ext(fullPath))
	mimeType, isImage := imageExtensions[ext]
	if !isImage {
		return llm.ContentPart{}, false
	}

	info, err := os.Stat(fullPath)
	if err != nil || info.Size() > maxAtImageSize {
		return llm.ContentPart{}, false
	}

	data, err := os.ReadFile(fullPath)
	if err != nil {
		return llm.ContentPart{}, false
	}

	return llm.ContentPart{
		Type:      llm.ContentPartImage,
		MediaType: mimeType,
		Data:      base64.StdEncoding.EncodeToString(data),
	}, true
}

// expandDirListing returns a recursive file listing for a directory.
func expandDirListing(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "rg", "--files", "--hidden", "--glob", "!.git")
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		return ""
	}

	var files []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(output)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			files = append(files, filepath.ToSlash(line))
		}
	}

	if len(files) == 0 {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Directory contains %d files:\n", len(files))
	for _, f := range files {
		fmt.Fprintf(&b, "  %s\n", f)
	}
	return b.String()
}

// tryReadTextFile reads a file and returns its content if it looks like a
// text file (no null bytes in the first 8KB) and is within the size limit.
// Returns ok=false for binary files, oversized files, or read errors.
func tryReadTextFile(fullPath string) (content string, ok bool) {
	info, err := os.Stat(fullPath)
	if err != nil {
		return "", false
	}
	if info.Size() > maxAtInlineSize {
		return "", false
	}

	data, err := os.ReadFile(fullPath)
	if err != nil {
		return "", false
	}

	checkLen := min(len(data), 8000)
	if bytes.Contains(data[:checkLen], []byte{0}) {
		return "", false // binary file
	}

	return string(data), true
}

// findLastAt finds the position of the last @ in s that is preceded by
// a space (or at position 0). Returns -1 if not found.
func findLastAt(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '@' && (i == 0 || s[i-1] == ' ') {
			return i
		}
	}
	return -1
}
