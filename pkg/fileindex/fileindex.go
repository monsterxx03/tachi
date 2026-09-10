// Package fileindex builds and caches a fuzzy-searchable index of the files
// under a directory, backing "@" file completion in interactive frontends.
//
// A directory is listed once with ripgrep (gitignore-aware, .git excluded, the
// .tachi directory force-included), stored in a path trie, and then searched
// fuzzily per keystroke. Built indexes are cached per root for DefaultTTL, so
// typing "@" does not re-run ripgrep on every keypress.
package fileindex

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/pkg/container"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/logger"
)

const (
	// DefaultTTL is how long a built index stays valid before it is rebuilt.
	DefaultTTL = 30 * time.Second
	// DefaultLimit caps the number of matches a search returns.
	DefaultLimit = 20
	// listTimeout bounds a single ripgrep listing.
	listTimeout = 10 * time.Second
)

// Match is a single search result.
type Match struct {
	Path  string // path relative to the searched root, slash-separated
	Score int    // fuzzy score (higher is better); 0 for immediate listings
	IsDir bool
}

// Index caches one path trie per root directory. It is safe for concurrent use.
type Index struct {
	ttl    time.Duration
	logger *logger.Logger

	mu      sync.Mutex
	entries map[string]*entry
}

// entry is the cached index of one root. Its own mutex keeps a slow rebuild of
// one root from blocking searches against another.
type entry struct {
	mu      sync.Mutex
	trie    *container.PathTrie
	err     error
	builtAt time.Time
}

// New returns an Index with DefaultTTL, logging index builds to l (l may be nil).
func New(l *logger.Logger) *Index {
	return NewWithTTL(l, DefaultTTL)
}

// NewWithTTL returns an Index whose built indexes expire after ttl. A
// non-positive ttl disables caching, so every search rebuilds.
func NewWithTTL(l *logger.Logger, ttl time.Duration) *Index {
	return &Index{ttl: ttl, logger: l, entries: make(map[string]*entry)}
}

// Search returns matches for query under root. An empty query lists the
// immediate entries of root; otherwise the whole tree is fuzzy-searched.
func (ix *Index) Search(root, query string, limit int) ([]Match, error) {
	if strings.TrimSpace(query) == "" {
		return ix.Immediate(root, limit)
	}

	t, err := ix.trie(root)
	if err != nil {
		return nil, err
	}

	results := t.Search(query, normalizeLimit(limit))
	matches := make([]Match, len(results))
	for i, r := range results {
		matches[i] = Match{Path: r.Path, Score: r.Score, IsDir: r.IsDir}
	}
	return matches, nil
}

// Immediate lists the immediate entries of root, directories first and then
// files, each group sorted by name (case-insensitive). .git is skipped.
func (ix *Index) Immediate(root string, limit int) ([]Match, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}

	matches := make([]Match, 0, len(entries))
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		matches = append(matches, Match{Path: e.Name(), IsDir: e.IsDir()})
	}

	slices.SortFunc(matches, func(a, b Match) int {
		if a.IsDir != b.IsDir {
			if a.IsDir {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.ToLower(a.Path), strings.ToLower(b.Path))
	})

	if limit = normalizeLimit(limit); len(matches) > limit {
		matches = matches[:limit]
	}
	return matches, nil
}

// Invalidate drops the cached index for root, forcing the next search to
// rebuild it.
func (ix *Index) Invalidate(root string) {
	ix.mu.Lock()
	delete(ix.entries, root)
	ix.mu.Unlock()
}

// trie returns the cached trie for root, rebuilding it when missing or stale.
// Failures (ripgrep unavailable, unreadable root) are cached for the same TTL.
func (ix *Index) trie(root string) (*container.PathTrie, error) {
	ix.mu.Lock()
	e, ok := ix.entries[root]
	if !ok {
		e = &entry{}
		ix.entries[root] = e
	}
	ix.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()

	if !e.builtAt.IsZero() && time.Since(e.builtAt) < ix.ttl {
		return e.trie, e.err
	}

	paths, err := listFiles(root)
	e.builtAt = time.Now()
	if err != nil {
		e.trie, e.err = nil, err
		ix.logger.Info(context.Background(), "fileindex: build failed", "root", root, "err", err.Error())
		return nil, err
	}

	e.trie, e.err = container.NewPathTrie(paths), nil
	ix.logger.Info(context.Background(), "fileindex: built index", "root", root, "files", e.trie.FileCount())
	return e.trie, nil
}

// listFiles lists the files under root: gitignore-aware, .git excluded, and
// with .tachi force-included (its contents are usually gitignored, yet
// referenceable).
func listFiles(root string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "rg", "--files", "--hidden", "--glob", "!.git")
	cmd.Dir = root
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	paths := splitLines(string(output))

	if fileutil.IsDir(filepath.Join(root, ".tachi")) {
		tachiCmd := exec.CommandContext(ctx, "rg", "--files", "--hidden", "--no-ignore-vcs", "--glob", "!.git", ".tachi")
		tachiCmd.Dir = root
		if tachiOutput, err := tachiCmd.Output(); err == nil {
			seen := container.NewSet(paths...)
			for _, p := range splitLines(string(tachiOutput)) {
				if !seen.Has(p) {
					paths = append(paths, p)
					seen.Add(p)
				}
			}
		}
	}

	return paths, nil
}

// splitLines splits ripgrep output into non-empty, slash-separated paths.
func splitLines(output string) []string {
	var paths []string
	for line := range strings.SplitSeq(output, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			paths = append(paths, filepath.ToSlash(line))
		}
	}
	return paths
}

// normalizeLimit clamps a search limit to DefaultLimit when unset.
func normalizeLimit(limit int) int {
	if limit <= 0 {
		return DefaultLimit
	}
	return limit
}
