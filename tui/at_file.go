package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/pkg/debuglog"
	"github.com/monsterxx03/tachi/pkg/pathtrie"
)

// --- Cached trie ---

var (
	atFileTrie    *pathtrie.PathTrie
	atFileTrieTTL time.Time
	atFileTrieMu  sync.Mutex
)

const (
	atFileCacheDuration = 30 * time.Second
	atFileMaxResults    = 20
)

// getCachedTrie returns the path trie for the current working directory,
// cached for 30 seconds.
func getCachedTrie() (*pathtrie.PathTrie, error) {
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

	cmd := exec.CommandContext(ctx, "rg", "--files", "--hidden", "--glob", "!.git")
	cmd.Dir = cwd
	output, err := cmd.Output()
	if err != nil {
		atFileTrie = nil
		atFileTrieTTL = time.Now()
		return nil, err
	}

	var paths []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			paths = append(paths, filepath.ToSlash(line))
		}
	}

	t := pathtrie.New(paths)
	atFileTrie = t
	atFileTrieTTL = time.Now()
	debuglog.DefaultLogger.Log("at_file: built trie with %d files", t.FileCount())
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
func searchAtFiles(query string) ([]atFileMatch, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	if query == "" {
		return listCwdImmediate(cwd)
	}

	t, err := getCachedTrie()
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

	sort.Slice(matches, func(i, j int) bool {
		if matches[i].IsDir != matches[j].IsDir {
			return matches[i].IsDir
		}
		return strings.ToLower(matches[i].Path) < strings.ToLower(matches[j].Path)
	})

	return matches, nil
}

// --- Reference resolution and message expansion ---

const maxAtFileSize = 256 * 1024 // 256KB

func resolveAtReference(path string) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getcwd: %w", err)
	}

	fullPath := filepath.Join(cwd, path)
	info, err := os.Stat(fullPath)
	if err != nil {
		return "", fmt.Errorf("stat: %w", err)
	}

	if info.IsDir() {
		return listDirContents(fullPath, path)
	}

	return readFileForAt(fullPath)
}

func listDirContents(dir, displayPath string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "rg", "--files", "--hidden", "--glob", "!.git")
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("list dir: %w", err)
	}

	files := strings.Split(strings.TrimSpace(string(output)), "\n")
	var nonEmpty []string
	for _, f := range files {
		if strings.TrimSpace(f) != "" {
			nonEmpty = append(nonEmpty, filepath.ToSlash(f))
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Directory %s contains %d files:\n", displayPath, len(nonEmpty))
	for _, f := range nonEmpty {
		fmt.Fprintf(&b, "  %s\n", f)
	}
	return b.String(), nil
}

func readFileForAt(fullPath string) (string, error) {
	info, err := os.Stat(fullPath)
	if err != nil {
		return "", fmt.Errorf("stat: %w", err)
	}
	if info.Size() > maxAtFileSize {
		return "", fmt.Errorf("file too large: %d bytes (limit: %d)", info.Size(), maxAtFileSize)
	}

	content, err := os.ReadFile(fullPath)
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}

	checkLen := min(len(content), 8000)
	for i := 0; i < checkLen; i++ {
		if content[i] == 0 {
			return "", fmt.Errorf("cannot include binary file")
		}
	}

	return string(content), nil
}

// ExpandAtReferences takes a user message and expands all @path references
// by injecting file/directory contents. The expanded message is what gets
// sent to the LLM. The original @path markers are preserved so the LLM
// knows which files were referenced.
func ExpandAtReferences(message string) string {
	var result strings.Builder
	expanded := false

	i := 0
	for i < len(message) {
		if message[i] == '@' && (i == 0 || message[i-1] == ' ') {
			j := i + 1
			for j < len(message) && message[j] != ' ' && message[j] != '\n' {
				j++
			}
			path := message[i+1 : j]

			result.WriteString("@")
			result.WriteString(path)

			if path != "" {
				content, err := resolveAtReference(path)
				if err != nil {
					debuglog.DefaultLogger.Log("at_file: resolve %q: %v", path, err)
				} else {
					result.WriteString("\n\n--- BEGIN UNTRUSTED FILE CONTENT: ")
					result.WriteString(path)
					result.WriteString(" ---\n")
					result.WriteString(content)
					result.WriteString("\n--- END UNTRUSTED FILE CONTENT: ")
					result.WriteString(path)
					result.WriteString(" ---")
					expanded = true
				}
			}
			i = j
		} else {
			result.WriteByte(message[i])
			i++
		}
	}

	if expanded {
		debuglog.DefaultLogger.Log("at_file: expanded @ references in message")
	}

	return result.String()
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
