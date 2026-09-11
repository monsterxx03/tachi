package linediff

import "testing"

// TestFragments covers the shapes a tool call actually produces.
func TestFragments(t *testing.T) {
	tests := []struct {
		name     string
		old      string
		new      string
		wantKind []Kind
		wantText []string
	}{
		{
			name:     "identical fragments have no diff at all",
			old:      "a\nb\n",
			new:      "a\nb\n",
			wantKind: nil,
		},
		{
			name:     "one line replaced keeps the common sides as context",
			old:      "a\nb\nc\n",
			new:      "a\nB\nc\n",
			wantKind: []Kind{KindContext, KindDel, KindAdd, KindContext},
			wantText: []string{"a", "b", "B", "c"},
		},
		{
			name:     "pure addition appends",
			old:      "a\n",
			new:      "a\nb\n",
			wantKind: []Kind{KindContext, KindAdd},
			wantText: []string{"a", "b"},
		},
		{
			name:     "pure deletion removes",
			old:      "a\nb\n",
			new:      "a\n",
			wantKind: []Kind{KindContext, KindDel},
			wantText: []string{"a", "b"},
		},
		{
			// A create: the old side has no lines, so nothing is deleted — and the
			// trailing newline is a terminator, not a blank line being added.
			name:     "empty old text is a create, without a phantom blank line",
			old:      "",
			new:      "x\ny\n",
			wantKind: []Kind{KindAdd, KindAdd},
			wantText: []string{"x", "y"},
		},
		{
			// The trailing newline is a real difference and must not vanish.
			name:     "a dropped trailing newline shows up",
			old:      "x\n",
			new:      "x",
			wantKind: []Kind{KindContext, KindDel},
			wantText: []string{"x", ""},
		},
		{
			name:     "complete rewrite is all del then all add",
			old:      "a\nb\n",
			new:      "c\nd\n",
			wantKind: []Kind{KindDel, KindDel, KindAdd, KindAdd},
			wantText: []string{"a", "b", "c", "d"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Fragments(tt.old, tt.new)
			if len(got) != len(tt.wantKind) {
				t.Fatalf("Fragments() returned %d hunks, want %d: %+v", len(got), len(tt.wantKind), got)
			}
			for i, h := range got {
				if h.Kind != tt.wantKind[i] {
					t.Errorf("hunk %d kind = %q, want %q", i, h.Kind, tt.wantKind[i])
				}
				if h.Text != tt.wantText[i] {
					t.Errorf("hunk %d text = %q, want %q", i, h.Text, tt.wantText[i])
				}
			}
		})
	}
}

// TestFragmentsLineNumbers pins the coordinate rule: 1-based within the fragment,
// 0 on the side where the line does not exist.
func TestFragmentsLineNumbers(t *testing.T) {
	hunks := Fragments("keep\nold1\nold2\ntail\n", "keep\nnew1\ntail\n")
	want := []Hunk{
		{Kind: KindContext, OldLine: 1, NewLine: 1, Text: "keep"},
		{Kind: KindDel, OldLine: 2, NewLine: 0, Text: "old1"},
		{Kind: KindDel, OldLine: 3, NewLine: 0, Text: "old2"},
		{Kind: KindAdd, OldLine: 0, NewLine: 2, Text: "new1"},
		{Kind: KindContext, OldLine: 4, NewLine: 3, Text: "tail"},
	}
	if len(hunks) != len(want) {
		t.Fatalf("got %d hunks, want %d: %+v", len(hunks), len(want), hunks)
	}
	for i := range want {
		if hunks[i] != want[i] {
			t.Errorf("hunk %d = %+v, want %+v", i, hunks[i], want[i])
		}
	}
}

// TestFragmentsSuffixIsTrimmedOnce: the suffix must not overlap the prefix, or a
// fragment of repeated lines would report phantom changes.
func TestFragmentsSuffixIsTrimmedOnce(t *testing.T) {
	hunks := Fragments("a\na\na\n", "a\na\n")
	added, removed := Counts(hunks)
	if added != 0 || removed != 1 {
		t.Errorf("added/removed = %d/%d, want 0/1: %+v", added, removed, hunks)
	}
}

func TestCounts(t *testing.T) {
	if a, r := Counts(nil); a != 0 || r != 0 {
		t.Errorf("Counts(nil) = %d/%d, want 0/0", a, r)
	}
	hunks := Fragments("a\nb\n", "a\nc\nd\n")
	added, removed := Counts(hunks)
	if added != 2 || removed != 1 {
		t.Errorf("Counts = %d added / %d removed, want 2/1: %+v", added, removed, hunks)
	}
}

// TestFragmentsMultiOccurrence pins the documented shape of a replace_all edit: the
// trimming algorithm produces one delete run against one add run, not paired
// changes. The UI labels it ("多处替换，整段对照") — the algorithm is not expected to
// pair them up.
func TestFragmentsMultiOccurrence(t *testing.T) {
	old := "call(x)\nkeep1\ncall(x)\nkeep2\ncall(x)\n"
	new := "call(y)\nkeep1\ncall(y)\nkeep2\ncall(y)\n"
	hunks := Fragments(old, new)
	added, removed := Counts(hunks)
	// 5/5, not 3/3: with prefix/suffix trimming the lines BETWEEN the replacements
	// (keep1, keep2) belong to neither the common prefix nor the common suffix, so
	// they appear in both runs. That is the documented shape of this algorithm — the
	// counts are fragment line counts, not a minimal edit script.
	if added != 5 || removed != 5 {
		t.Fatalf("counts = %d/%d, want 5/5", added, removed)
	}
	// All deletions come before all additions — the documented limitation.
	firstAdd := -1
	lastDel := -1
	for i, h := range hunks {
		if h.Kind == KindAdd && firstAdd < 0 {
			firstAdd = i
		}
		if h.Kind == KindDel {
			lastDel = i
		}
	}
	if firstAdd < lastDel {
		t.Errorf("expected the delete run before the add run, got %+v", hunks)
	}
}

// TestParseUnified covers the shapes `git diff` actually prints. The fixtures are
// literal git output (paths, index lines and all) so the parser is tested against the
// real format rather than a tidied-up idea of it.
func TestParseUnified(t *testing.T) {
	tests := []struct {
		name  string
		diff  string
		check func(t *testing.T, files []FileDiff)
	}{
		{
			name: "modified file keeps real line numbers",
			diff: "diff --git a/src/main.go b/src/main.go\n" +
				"index 1111111..2222222 100644\n" +
				"--- a/src/main.go\n" +
				"+++ b/src/main.go\n" +
				"@@ -10,3 +10,4 @@ func main() {\n" +
				" \tctx := context.Background()\n" +
				"-\told()\n" +
				"+\tnewWith(30 * time.Second)\n" +
				"+\tdefer cancel()\n" +
				" \treturn\n",
			check: func(t *testing.T, files []FileDiff) {
				if len(files) != 1 {
					t.Fatalf("got %d files, want 1", len(files))
				}
				f := files[0]
				if f.Path != "src/main.go" || f.OldPath != "src/main.go" {
					t.Errorf("paths = %q / %q", f.Path, f.OldPath)
				}
				if f.Added != 2 || f.Removed != 1 {
					t.Errorf("counts = %d/%d, want 2/1", f.Added, f.Removed)
				}
				want := []Hunk{
					{Kind: KindContext, OldLine: 10, NewLine: 10, Text: "\tctx := context.Background()"},
					{Kind: KindDel, OldLine: 11, Text: "\told()"},
					{Kind: KindAdd, NewLine: 11, Text: "\tnewWith(30 * time.Second)"},
					{Kind: KindAdd, NewLine: 12, Text: "\tdefer cancel()"},
					{Kind: KindContext, OldLine: 12, NewLine: 13, Text: "\treturn"},
				}
				if len(f.Hunks) != len(want) {
					t.Fatalf("hunks = %+v", f.Hunks)
				}
				for i := range want {
					if f.Hunks[i] != want[i] {
						t.Errorf("hunk %d = %+v, want %+v", i, f.Hunks[i], want[i])
					}
				}
			},
		},
		{
			name: "two files in one diff",
			diff: "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-x\n+y\n" +
				"diff --git a/b.go b/b.go\n--- a/b.go\n+++ b/b.go\n@@ -5 +5 @@\n-p\n+q\n",
			check: func(t *testing.T, files []FileDiff) {
				if len(files) != 2 || files[0].Path != "a.go" || files[1].Path != "b.go" {
					t.Fatalf("files = %+v", files)
				}
			},
		},
		{
			name: "created file",
			diff: "diff --git a/CHANGELOG.md b/CHANGELOG.md\n" +
				"new file mode 100644\nindex 0000000..3333333\n" +
				"--- /dev/null\n+++ b/CHANGELOG.md\n@@ -0,0 +1,2 @@\n+# Changelog\n+\n",
			check: func(t *testing.T, files []FileDiff) {
				f := files[0]
				if !f.Created || f.Deleted {
					t.Errorf("created = %v deleted = %v, want created", f.Created, f.Deleted)
				}
				if f.Path != "CHANGELOG.md" || f.Added != 2 || f.Removed != 0 {
					t.Errorf("file = %+v", f)
				}
				if f.Hunks[0].OldLine != 0 || f.Hunks[0].NewLine != 1 {
					t.Errorf("first hunk = %+v (a created file has no old line)", f.Hunks[0])
				}
			},
		},
		{
			name: "deleted file",
			diff: "diff --git a/gone.go b/gone.go\ndeleted file mode 100644\n--- a/gone.go\n+++ /dev/null\n@@ -1 +0,0 @@\n-bye\n",
			check: func(t *testing.T, files []FileDiff) {
				f := files[0]
				if !f.Deleted || f.Path != "gone.go" {
					t.Errorf("file = %+v, want a deleted gone.go", f)
				}
			},
		},
		{
			name: "binary file has no hunks",
			diff: "diff --git a/logo.png b/logo.png\nindex 1111111..2222222 100644\nBinary files a/logo.png and b/logo.png differ\n",
			check: func(t *testing.T, files []FileDiff) {
				if len(files) != 1 || !files[0].Binary || len(files[0].Hunks) != 0 {
					t.Errorf("files = %+v, want one binary file with no hunks", files)
				}
			},
		},
		{
			name: "rename carries both paths",
			diff: "diff --git a/old.go b/new.go\nsimilarity index 95%\nrename from old.go\nrename to new.go\n" +
				"--- a/old.go\n+++ b/new.go\n@@ -1 +1 @@\n-a\n+b\n",
			check: func(t *testing.T, files []FileDiff) {
				f := files[0]
				if f.Path != "new.go" || f.OldPath != "old.go" {
					t.Errorf("paths = %q / %q, want new.go / old.go", f.Path, f.OldPath)
				}
			},
		},
		{
			name: "quoted paths (spaces and non-ASCII) are unquoted",
			diff: "diff --git \"a/\\346\\212\\245\\345\\221\\212 2026.md\" \"b/\\346\\212\\245\\345\\221\\212 2026.md\"\n" +
				"--- \"a/\\346\\212\\245\\345\\221\\212 2026.md\"\n+++ \"b/\\346\\212\\245\\345\\221\\212 2026.md\"\n@@ -1 +1 @@\n-a\n+b\n",
			check: func(t *testing.T, files []FileDiff) {
				if want := "报告 2026.md"; files[0].Path != want {
					t.Errorf("Path = %q, want %q", files[0].Path, want)
				}
			},
		},
		{
			name: "no newline marker and mode-only change are ignored",
			diff: "diff --git a/mode.sh b/mode.sh\nold mode 100644\nnew mode 100755\n" +
				"diff --git a/tail.txt b/tail.txt\n--- a/tail.txt\n+++ b/tail.txt\n@@ -1 +1 @@\n-a\n\\ No newline at end of file\n+b\n",
			check: func(t *testing.T, files []FileDiff) {
				if len(files) != 2 {
					t.Fatalf("files = %+v, want two (one with no hunks)", files)
				}
				if len(files[0].Hunks) != 0 {
					t.Errorf("mode-only change has hunks: %+v", files[0].Hunks)
				}
				if files[1].Added != 1 || files[1].Removed != 1 {
					t.Errorf("tail.txt counts = %d/%d", files[1].Added, files[1].Removed)
				}
			},
		},
		{
			name: "empty input",
			diff: "",
			check: func(t *testing.T, files []FileDiff) {
				if len(files) != 0 {
					t.Errorf("files = %+v, want none", files)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.check(t, ParseUnified(tt.diff)) })
	}
}
