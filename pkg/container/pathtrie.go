package container

import (
	"container/heap"
	"strings"
)

// trieNode is a node in the path trie. Children are keyed by the original
// (un-lowered) byte value from the file path.
type trieNode struct {
	children     map[byte]*trieNode
	isFile       bool // this node ends a file path
	isDir        bool // this node is an intermediate directory
	maxRemaining int  // max chars from this node to any descendant leaf
	depth        int  // depth from root (in bytes)
}

// PathTrie stores file paths in a trie for efficient fuzzy search with
// prefix-scoped pruning.
type PathTrie struct {
	root      *trieNode
	fileCount int
}

// New builds a PathTrie from a list of slash-separated relative paths.
func NewPathTrie(paths []string) *PathTrie {
	t := &PathTrie{
		root: &trieNode{
			children: make(map[byte]*trieNode),
		},
	}
	for _, p := range paths {
		t.insert(p)
	}
	t.computeMaxRemaining(t.root)
	return t
}

// FileCount returns the number of file paths stored in the trie.
func (t *PathTrie) FileCount() int { return t.fileCount }

func (t *PathTrie) insert(path string) {
	if path == "" {
		return
	}
	cur := t.root
	// Byte-wise on purpose: the trie is keyed by raw path bytes. `for i := range path`
	// would walk RUNE boundaries and `path[i]` would hand back only the first byte of
	// every multi-byte character, silently truncating any non-ASCII file name
	// ("实时保存的计划.json" collapsed to its first seven bytes).
	for i := 0; i < len(path); i++ {
		b := path[i]
		child, ok := cur.children[b]
		if !ok {
			child = &trieNode{
				children: make(map[byte]*trieNode),
				depth:    cur.depth + 1,
			}
			cur.children[b] = child
		}
		if b == '/' {
			child.isDir = true
		}
		cur = child
	}
	if !cur.isFile {
		cur.isFile = true
		t.fileCount++
	}
}

// computeMaxRemaining post-order computes maxRemaining for each node.
func (t *PathTrie) computeMaxRemaining(n *trieNode) int {
	maxR := 0
	if n.isFile {
		maxR = 1
	}
	for _, child := range n.children {
		r := t.computeMaxRemaining(child) + 1
		if r > maxR {
			maxR = r
		}
	}
	n.maxRemaining = maxR
	return maxR
}

// matchByte returns true if trie byte b case-insensitively matches
// lowercased query byte qb.
func matchByte(b, qb byte) bool {
	if b == qb {
		return true
	}
	if b >= 'A' && b <= 'Z' && b+32 == qb {
		return true
	}
	return false
}

// Match is a single fuzzy-search result.
type Match struct {
	Path  string // the matched path
	Score int    // fuzzy score (higher is better)
	IsDir bool   // true if this match is a directory
}

// --- Top-N heap ---

type matchHeap struct {
	items []Match
}

func (h matchHeap) Len() int           { return len(h.items) }
func (h matchHeap) Less(i, j int) bool { return h.items[i].Score < h.items[j].Score } // min-heap
func (h matchHeap) Swap(i, j int)      { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *matchHeap) Push(x any)        { h.items = append(h.items, x.(Match)) }
func (h *matchHeap) Pop() any {
	old := h.items
	n := len(old)
	x := old[n-1]
	h.items = old[:n-1]
	return x
}

// searchDFS recursively walks the trie looking for fuzzy matches of query.
func (t *PathTrie) searchDFS(
	n *trieNode,
	query string,
	qi int,
	consecutive int,
	score int,
	path []byte,
	h *matchHeap,
	topN int,
) {
	remainingQuery := len(query) - qi
	if n.maxRemaining < remainingQuery {
		return
	}

	if qi == len(query) {
		finalScore := score - n.depth
		// Directory match: the current node represents a path that ends
		// with "/" (isDir was set during insert).
		if n.isDir {
			heap.Push(h, Match{Path: string(path), Score: finalScore, IsDir: true})
			if h.Len() > topN {
				heap.Pop(h)
			}
		}
		if n.isFile {
			heap.Push(h, Match{Path: string(path), Score: finalScore, IsDir: false})
			if h.Len() > topN {
				heap.Pop(h)
			}
		}
		for b, child := range n.children {
			np := make([]byte, len(path), len(path)+1)
			copy(np, path)
			np = append(np, b)
			t.searchDFS(child, query, qi, 0, score, np, h, topN)
		}
		return
	}

	qb := query[qi]
	for b, child := range n.children {
		np := make([]byte, len(path), len(path)+1)
		copy(np, path)
		np = append(np, b)
		if matchByte(b, qb) {
			newConsecutive := consecutive + 1
			newScore := score + newConsecutive*2
			if b == '/' || n.depth == 0 {
				newScore += 10
			}
			t.searchDFS(child, query, qi+1, newConsecutive, newScore, np, h, topN)
		} else {
			t.searchDFS(child, query, qi, 0, score, np, h, topN)
		}
	}
}

// Search performs fuzzy search and returns up to topN matches sorted by
// score descending. A "/" in the query scopes the walk to the directory the
// query names; when it names none, the query degrades to a whole-tree fuzzy
// match rather than coming back empty (see scope).
func (t *PathTrie) Search(query string, topN int) []Match {
	if query == "" {
		return nil
	}
	if topN <= 0 {
		return nil
	}

	query = strings.ToLower(query)
	h := &matchHeap{}
	heap.Init(h)

	startNode, fuzzyQuery, prefixPath := t.scope(query)
	t.searchDFS(startNode, fuzzyQuery, 0, 0, 0, prefixPath, h, topN)

	results := make([]Match, h.Len())
	for i := len(results) - 1; i >= 0; i-- {
		results[i] = heap.Pop(h).(Match)
	}
	return results
}

// scope decides where searchDFS starts and which part of the query it matches
// fuzzily.
//
// A query containing "/" is read as a path: everything up to and including the
// last "/" must name a real directory, and only the tail is fuzzy-matched. That
// scoping is what makes "docs/agents/deskt" a narrowing of the tree instead of
// a subsequence hunt through every path carrying those letters.
//
// When the prefix names NO directory the query still has to be answerable: it
// degrades to a subsequence match over the whole tree, where the "/" is just
// another character the match may skip. One wrong character in a directory name
// ("doc/2026" for docs/2026-09-01.md) then shows the file the reader is aiming
// at instead of emptying the picker. Addressing an existing prefix always wins,
// so the fallback never dilutes a real drill-down.
func (t *PathTrie) scope(query string) (start *trieNode, fuzzy string, prefix []byte) {
	idx := strings.LastIndex(query, "/")
	if idx < 0 {
		return t.root, query, nil
	}
	dir := query[:idx+1]
	if node := t.walkPrefix(dir); node != nil {
		return node, query[idx+1:], []byte(dir)
	}
	return t.root, query, nil
}

// walkPrefix follows an exact (case-insensitive) path through the trie.
func (t *PathTrie) walkPrefix(prefix string) *trieNode {
	cur := t.root
	// Byte-wise, for the same reason as insert: rune indices would skip the continuation
	// bytes of a non-ASCII prefix and walk a path that was never inserted.
	for i := 0; i < len(prefix); i++ {
		b := prefix[i]
		found := false
		for cb, child := range cur.children {
			if matchByte(cb, b) {
				cur = child
				found = true
				break
			}
		}
		if !found {
			return nil
		}
	}
	return cur
}
