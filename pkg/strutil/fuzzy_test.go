package strutil

import "testing"

func TestFuzzyScoreMatchesSubsequence(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  bool
	}{
		{"empty query matches anything", "", true},
		{"prefix", "read", true},
		{"scattered subsequence", "rme", true},
		{"case insensitive", "README", true},
		{"chars out of order", "daer", false},
		{"missing char", "readz", false},
		{"query longer than target", "readme!!!", false},
		{"chinese", "引用", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := FuzzyScore(tc.query, "跨会话引用 Readme.md"); ok != tc.want {
				t.Errorf("FuzzyScore(%q, ...) ok = %v, want %v", tc.query, ok, tc.want)
			}
		})
	}
}

// TestFuzzyScoreRanks is the property the picker depends on: the three tiers
// stay ordered (exact > prefix > scattered), a word start beats a match buried
// inside a word, and a run of adjacent characters beats the same characters
// found one at a time.
func TestFuzzyScoreRanks(t *testing.T) {
	score := func(query, target string) int {
		s, ok := FuzzyScore(query, target)
		if !ok {
			t.Fatalf("FuzzyScore(%q, %q) did not match", query, target)
		}
		return s
	}

	if score("read", "read") <= score("read", "readme") {
		t.Error("an exact match must outrank a prefix match")
	}
	if score("read", "readme") <= score("read", "a-read-me") {
		t.Error("a prefix match must outrank a scattered one")
	}
	if score("ref", "auth-refactor") <= score("ref", "prefer") {
		t.Error("a match at a word start must outrank one inside a word")
	}
	if score("auth", "auth") <= score("auth", "a_u_t_h") {
		t.Error("consecutive characters must outrank the same chars scattered")
	}
}
