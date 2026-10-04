package strutil

import "strings"

// Fuzzy-match scoring. The shape is the one every picker needs: an exact hit
// must beat a prefix, and a prefix must beat the same characters found one by
// one — otherwise a short query ranks a scattered match above the obvious one.
const (
	fuzzyExactBonus       = 1000
	fuzzyPrefixBonus      = 300
	fuzzyWordStartBonus   = 20
	fuzzyConsecutiveBonus = 15
)

// FuzzyScore reports whether query matches target as a case-insensitive
// subsequence — every rune of query appearing in target, in order, not
// necessarily adjacent — and scores how good the match is: higher is better,
// and a false match scores 0. An empty query matches anything with a score of
// 0, which is what lets a caller use it as "no filter, keep the caller's
// order".
//
// The match is the greedy left-to-right one, so the score is a ranking device,
// not a proof of the best alignment: it is meant to sort a list a reader is
// already narrowing by typing, where "the obvious one is on top" is the whole
// requirement.
func FuzzyScore(query, target string) (int, bool) {
	if query == "" {
		return 0, true
	}
	lq := strings.ToLower(query)
	lt := strings.ToLower(target)
	q := []rune(lq)
	// The original runes are kept for word-boundary detection: separators are
	// unaffected by case, and this way the lowercasing above cannot change
	// which positions count as a word start.
	t := []rune(target)
	if len(q) > len(t) {
		return 0, false
	}
	lower := []rune(lt)

	score := 0
	switch {
	case lq == lt:
		score += fuzzyExactBonus
	case strings.HasPrefix(lt, lq):
		score += fuzzyPrefixBonus
	}

	qi, prev := 0, -2
	for ti := 0; ti < len(lower) && qi < len(q); ti++ {
		if lower[ti] != q[qi] {
			continue
		}
		switch {
		case ti == prev+1:
			score += fuzzyConsecutiveBonus
		case isWordStart(t, ti):
			score += fuzzyWordStartBonus
		}
		prev = ti
		qi++
	}
	if qi < len(q) {
		return 0, false
	}
	return score, true
}

// isWordStart reports whether the rune at i begins a word: the first rune, or
// one following a separator. It is what makes "auth-refactor" rank above
// "authorization" for the query "ref".
func isWordStart(target []rune, i int) bool {
	if i == 0 {
		return true
	}
	switch target[i-1] {
	case ' ', '-', '_', '/', '.', ':', '\t':
		return true
	}
	return false
}
