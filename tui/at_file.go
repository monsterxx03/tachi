package tui

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

	slices.SortFunc(matches, func(a, b atFileMatch) int {
		if a.IsDir != b.IsDir {
			if a.IsDir {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.ToLower(a.Path), strings.ToLower(b.Path))
	})

	return matches, nil
}

// --- Reference resolution and message expansion ---

// ExpandAtReferences takes a user message and converts all @path references
// into annotated markers that tell the LLM where the file is, without
// inlining its content. The LLM can then use ReadFile, Bash or Glob tools
// to read the file when it needs to, saving context window space.
//
// For files:   @main.go          →  [@file: main.go]
// For dirs:    @src/              →  [@dir: src/]
// On error:    @nonexistent.go    →  @nonexistent.go (left as-is)
func ExpandAtReferences(message string) string {
	var result strings.Builder
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
				info, err := os.Stat(filepath.Join(cwd, path))
				if err == nil {
					if info.IsDir() {
						result.WriteString("[@dir: ")
						result.WriteString(path)
						result.WriteString("]")
					} else {
						result.WriteString("[@file: ")
						result.WriteString(path)
						result.WriteString("]")
					}
					expanded = true
				} else {
					// File not found — keep original @path, the LLM will
					// figure it out or the user can correct it.
					result.WriteString("@")
					result.WriteString(path)
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
		debuglog.DefaultLogger.Log("at_file: resolved @ references in message")
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
