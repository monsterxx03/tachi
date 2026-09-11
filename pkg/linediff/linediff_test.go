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
