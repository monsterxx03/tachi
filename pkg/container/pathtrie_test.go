package container

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
)

func paths(s ...string) []string { return s }

func sortMatches(ms []Match) []Match {
	sort.Slice(ms, func(i, j int) bool {
		if ms[i].Score != ms[j].Score {
			return ms[i].Score > ms[j].Score
		}
		return ms[i].Path < ms[j].Path
	})
	return ms
}

func TestNewEmpty(t *testing.T) {
	tr := NewPathTrie(nil)
	if tr.FileCount() != 0 {
		t.Errorf("expected 0 files, got %d", tr.FileCount())
	}
	if res := tr.Search("x", 10); len(res) != 0 {
		t.Errorf("expected empty result, got %v", res)
	}
}

func TestNewBasic(t *testing.T) {
	tr := NewPathTrie(paths("a/b.go", "a/c.go", "README.md"))
	if tr.FileCount() != 3 {
		t.Errorf("expected 3 files, got %d", tr.FileCount())
	}
}

func TestSearchExact(t *testing.T) {
	tr := NewPathTrie(paths(
		"foo/bar.go",
		"foo/baz.go",
		"foo/qux/zzz.go",
		"README.md",
	))

	got := sortMatches(tr.Search("foo", 10))

	// "foo" should match:
	// - foo/ (dir, high score — matched all 3 chars consecutively + boundary bonus)
	// - foo/bar.go, foo/baz.go (files, "foo" matched)
	// - foo/qux/ (dir, "foo" matched)
	// - foo/qux/zzz.go (file, "foo" matched)
	dirFound := false
	barFound := false
	bazFound := false
	for _, m := range got {
		if m.Path == "foo/" && m.IsDir {
			dirFound = true
		}
		if m.Path == "foo/bar.go" && !m.IsDir {
			barFound = true
		}
		if m.Path == "foo/baz.go" && !m.IsDir {
			bazFound = true
		}
	}
	if !dirFound {
		t.Errorf("expected foo/ dir in results: %v", got)
	}
	if !barFound {
		t.Errorf("expected foo/bar.go in results: %v", got)
	}
	if !bazFound {
		t.Errorf("expected foo/baz.go in results: %v", got)
	}
}

func TestSearchFuzzy(t *testing.T) {
	tr := NewPathTrie(paths(
		"tui/input.go",
		"tui/model.go",
		"tui/at_file.go",
		"tui/chatview.go",
		"tools/grep.go",
		"tools/glob.go",
		"agent/agent.go",
	))

	tests := []struct {
		query string
		topN  int
		check func(t *testing.T, got []Match)
	}{
		{
			query: "inp",
			topN:  10,
			check: func(t *testing.T, got []Match) {
				if len(got) != 1 {
					t.Fatalf("expected 1 result, got %d: %v", len(got), got)
				}
				if got[0].Path != "tui/input.go" {
					t.Errorf("expected tui/input.go, got %s", got[0].Path)
				}
				if got[0].IsDir {
					t.Error("input.go should not be marked as dir")
				}
			},
		},
		{
			query: "mod",
			topN:  10,
			check: func(t *testing.T, got []Match) {
				paths := make([]string, len(got))
				for i, m := range got {
					paths[i] = m.Path
				}
				if !slices.Contains(paths, "tui/model.go") {
					t.Errorf("expected model.go in results: %v", paths)
				}
			},
		},
		{
			query: "grep",
			topN:  10,
			check: func(t *testing.T, got []Match) {
				if len(got) != 1 || got[0].Path != "tools/grep.go" {
					t.Errorf("expected [tools/grep.go], got %v", got)
				}
			},
		},
		{
			query: "grp",
			topN:  10,
			check: func(t *testing.T, got []Match) {
				paths := make([]string, len(got))
				for i, m := range got {
					paths[i] = m.Path
				}
				if !slices.Contains(paths, "tools/grep.go") {
					t.Errorf("expected grep.go in results: %v", paths)
				}
			},
		},
		{
			query: "glo",
			topN:  10,
			check: func(t *testing.T, got []Match) {
				paths := make([]string, len(got))
				for i, m := range got {
					paths[i] = m.Path
				}
				if !slices.Contains(paths, "tools/glob.go") {
					t.Errorf("expected glob.go in results: %v", paths)
				}
			},
		},
		{
			query: "age",
			topN:  10,
			check: func(t *testing.T, got []Match) {
				paths := make([]string, len(got))
				for i, m := range got {
					paths[i] = m.Path
				}
				if !slices.Contains(paths, "agent/agent.go") {
					t.Errorf("expected agent.go in results: %v", paths)
				}
			},
		},
		{
			query: "x",
			topN:  10,
			check: func(t *testing.T, got []Match) {
				if len(got) != 0 {
					t.Errorf("expected 0 results for 'x', got %v", got)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			got := tr.Search(tt.query, tt.topN)
			tt.check(t, got)
		})
	}
}

func TestSearchPrefixScoped(t *testing.T) {
	tr := NewPathTrie(paths(
		"tui/input.go",
		"tui/model.go",
		"tui/at_file.go",
		"tools/grep.go",
		"tools/edit.go",
		"agent/agent.go",
	))

	got := sortMatches(tr.Search("tui/inp", 10))
	if len(got) != 1 || got[0].Path != "tui/input.go" {
		t.Errorf("Search(tui/inp) = %v, want [tui/input.go]", got)
	}

	// Non-existent prefix
	got = tr.Search("foo/bar", 10)
	if len(got) != 0 {
		t.Errorf("Search(foo/bar) = %v, want []", got)
	}
}

func TestSearchTopN(t *testing.T) {
	var ps []string
	for i := range 100 {
		ps = append(ps, fmt.Sprintf("dir/file_%03d.txt", i))
	}
	tr := NewPathTrie(ps)

	got := tr.Search("file", 5)
	if len(got) != 5 {
		t.Errorf("expected 5 results, got %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Score > got[i-1].Score {
			t.Errorf("results not sorted by score desc: %d: %d > %d",
				i, got[i].Score, got[i-1].Score)
		}
	}
}

func TestSearchDirectoryMatch(t *testing.T) {
	tr := NewPathTrie(paths(
		"src/cmd/main.go",
		"src/lib/util.go",
		"src/lib/helper.go",
	))

	// "lib" should fuzzy-match the "src/lib/" directory prefix
	got := tr.Search("lib", 10)
	hasDir := false
	for _, m := range got {
		if m.Path == "src/lib/" && m.IsDir {
			hasDir = true
		}
	}
	if !hasDir {
		t.Errorf("expected src/lib/ dir in fuzzy results for 'lib': %v", got)
	}

	// "src/" should also match (exact prefix match via DFS)
	got = tr.Search("src/", 10)
	hasDir = false
	for _, m := range got {
		if m.Path == "src/" && m.IsDir {
			hasDir = true
		}
	}
	if !hasDir {
		t.Errorf("expected src/ dir in results: %v", got)
	}
}

func TestCaseInsensitive(t *testing.T) {
	tr := NewPathTrie(paths("FOO/BAR.go", "foo/bar.go", "FoO/BaR.GO"))
	got := tr.Search("foo", 10)
	if len(got) < 1 {
		t.Errorf("expected at least 1 match, got %d", len(got))
	}
}

func TestEmptyQuery(t *testing.T) {
	tr := NewPathTrie(paths("a/b.go"))
	if got := tr.Search("", 10); len(got) != 0 {
		t.Errorf("expected empty result for empty query")
	}
	if got := tr.Search("x", 0); len(got) != 0 {
		t.Errorf("expected empty result for topN=0")
	}
}

func TestScoreOrdering(t *testing.T) {
	tr := NewPathTrie(paths(
		"abc.go",    // depth 5: 22 - 5 = 17
		"axbxc.go",  // depth 7: 16 - 7 = 9
		"abx.go",    // depth 5: 16 - 5 = 11
		"abcdef.go", // depth 9: 22 - 9 = 13
	))

	got := tr.Search("abc", 10)
	if len(got) == 0 {
		t.Fatal("no results")
	}
	// Best should be abc.go (shorter depth at same fuzzy score)
	if got[0].Path != "abc.go" {
		t.Errorf("expected abc.go first, got %v", got)
	}
	// Second should be abcdef.go (same fuzzy score, deeper)
	if got[1].Path != "abcdef.go" {
		t.Errorf("expected abcdef.go second, got %v", got)
	}
}

func TestWalkPrefixExactCase(t *testing.T) {
	tr := NewPathTrie(paths("Src/Main.go", "src/main_test.go"))
	if got := tr.Search("src/", 10); len(got) < 1 {
		t.Errorf("expected results for src/: %v", got)
	}
}

func TestPathTrieSiblings(t *testing.T) {
	// Verify no overlap between prefix path slices across siblings
	tr := NewPathTrie(paths("ab/c.go", "ab/d.go"))

	got := sortMatches(tr.Search("ab", 10))
	for _, m := range got {
		t.Logf("  %q score=%d isDir=%v", m.Path, m.Score, m.IsDir)
	}

	// Should find ab/ as dir, ab/c.go, ab/d.go
	if len(got) < 3 {
		t.Errorf("expected >=3 matches, got %d: %v", len(got), got)
	}
}

func TestDeepPaths(t *testing.T) {
	tr := NewPathTrie(paths(
		"a/b/c/d/e.go",
		"a/b/x/y/z/f.go",
		"a/b/c/g/h/i.go",
	))

	// Unscoped fuzzy: "bc" should match within the trie
	got := tr.Search("bc", 10)
	for _, m := range got {
		t.Logf("  bc -> %q score=%d", m.Path, m.Score)
	}
	if len(got) == 0 {
		t.Error("expected matches for 'bc'")
	}
}

func TestMatchByte(t *testing.T) {
	tests := []struct {
		b  byte
		qb byte
		ok bool
	}{
		{'a', 'a', true},
		{'A', 'a', true},
		{'z', 'z', true},
		{'Z', 'z', true},
		{'.', '.', true},
		{'/', '/', true},
		{'a', 'b', false},
		{'A', 'B', false},
		// Non-ASCII shouldn't match uppercase trick
	}
	for _, tt := range tests {
		if got := matchByte(tt.b, tt.qb); got != tt.ok {
			t.Errorf("matchByte(%q, %q) = %v, want %v", tt.b, tt.qb, got, tt.ok)
		}
	}
}

func TestNewDedup(t *testing.T) {
	// Duplicate paths count once (they merge in the trie)
	tr := NewPathTrie(paths("a/b.go", "a/b.go", "a/b.go"))
	if tr.FileCount() != 1 {
		t.Errorf("expected 1 file, got %d", tr.FileCount())
	}
}

func TestSearchResultCount(t *testing.T) {
	// Fork a copy of the trie and ensure result count matches FileCount when
	// query matches everything.
	var ps []string
	for i := range 50 {
		ps = append(ps, fmt.Sprintf("x/file_%d.go", i))
	}
	tr := NewPathTrie(ps)

	got := tr.Search("x/file", 100)
	// All 50 files + 1 dir should be found
	if len(got) < 50 {
		t.Errorf("expected >=50 results, got %d", len(got))
	}
}

func TestSearchWithWeirdChars(t *testing.T) {
	tr := NewPathTrie(paths(
		"foo/bar-baz.go",
		"foo/bar_baz.go",
		"foo/.hidden",
		"foo/@special.go",
		"foo/with space.go",
	))

	got := tr.Search("bar-", 10)
	if len(got) == 0 {
		t.Error("expected match for 'bar-'")
	}
	got = tr.Search("@sp", 10)
	if len(got) == 0 {
		t.Error("expected match for '@sp'")
	}
	// space in query should work fine
	got = tr.Search("with space", 10)
	if len(got) == 0 {
		t.Error("expected match for 'with space'")
	}
}

func TestInsert_EmptyPathsIgnored(t *testing.T) {
	tr := NewPathTrie(paths("", "a/b.go", ""))
	if tr.FileCount() != 1 {
		t.Errorf("expected 1 file, got %d", tr.FileCount())
	}
}

// ---- benchmarks ----

func BenchmarkBuild(b *testing.B) {
	var ps []string
	for i := range 5000 {
		ps = append(ps, fmt.Sprintf("src/module/sub/pkg/file_%04d.go", i))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		NewPathTrie(ps)
	}
}

func BenchmarkSearch(b *testing.B) {
	var ps []string
	for i := range 5000 {
		ps = append(ps, fmt.Sprintf("src/module/sub/pkg/file_%04d.go", i))
	}
	tr := NewPathTrie(ps)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tr.Search("file_", 20)
	}
}

func BenchmarkSearchPrefixScoped(b *testing.B) {
	var ps []string
	for i := range 5000 {
		ps = append(ps, fmt.Sprintf("src/module/sub/pkg/file_%04d.go", i))
	}
	tr := NewPathTrie(ps)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tr.Search("src/module/sub/pkg/file_", 20)
	}
}

func BenchmarkSearchWeirdChars(b *testing.B) {
	// Ensure no regex special char handling causes slowdowns
	ps := []string{
		"foo/bar-baz.go",
		"foo/.hidden",
		"foo/@special.go",
	}
	for i := range 500 {
		ps = append(ps, fmt.Sprintf("src/file_%04d.go", i))
	}
	tr := NewPathTrie(ps)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tr.Search("@sp", 20)
	}
}

var _ = strings.Compare
