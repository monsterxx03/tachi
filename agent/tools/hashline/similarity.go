package hashline

// levenshteinDistance computes the edit distance between two strings.
// It uses the Wagner-Fischer algorithm with O(min(m,n)) space.
func levenshteinDistance(a, b string) int {
	if a == b {
		return 0
	}

	// Use rune-based distance to handle Unicode correctly.
	ra := []rune(a)
	rb := []rune(b)

	m, n := len(ra), len(rb)
	if m == 0 {
		return n
	}
	if n == 0 {
		return m
	}

	// Ensure b is the shorter for O(min(m,n)) space
	if m < n {
		ra, rb = rb, ra
		m, n = n, m
	}

	// prev and cur are the two rows we alternate between
	prev := make([]int, n+1)
	cur := make([]int, n+1)

	// Initialize prev row (0..n)
	for j := 0; j <= n; j++ {
		prev[j] = j
	}

	for i := 1; i <= m; i++ {
		cur[0] = i
		for j := 1; j <= n; j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			del := prev[j] + 1
			ins := cur[j-1] + 1
			sub := prev[j-1] + cost
			cur[j] = min(del, min(ins, sub))
		}
		prev, cur = cur, prev
	}

	return prev[n]
}

// levenshteinSimilarity returns a similarity score between 0.0 and 1.0.
// 1.0 means exact match, 0.0 means completely different.
// Uses normalized Levenshtein distance: 1 - (distance / max(len)).
func levenshteinSimilarity(a, b string) float64 {
	ra := []rune(a)
	rb := []rune(b)

	m, n := len(ra), len(rb)

	// Both empty → identical
	if m == 0 && n == 0 {
		return 1.0
	}

	distance := levenshteinDistance(a, b)
	maxLen := max(n, m)

	return 1.0 - float64(distance)/float64(maxLen)
}

// fuzzyLineMatch checks if two lines match within the given similarity threshold.
// It also applies normalization (trimming trailing whitespace) before comparison
// to be slightly more tolerant of minor whitespace differences.
func fuzzyLineMatch(actual, expected string, threshold float64) bool {
	// Quick exact match
	if actual == expected {
		return true
	}

	// Normalize: trim trailing whitespace for tolerance
	normActual := trimTrailingWhitespace(actual)
	normExpected := trimTrailingWhitespace(expected)

	if normActual == normExpected {
		return true
	}

	// Fuzzy match
	similarity := levenshteinSimilarity(normActual, normExpected)
	return similarity >= threshold
}

// trimTrailingWhitespace removes trailing spaces and tabs from a string.
func trimTrailingWhitespace(s string) string {
	end := len(s)
	for end > 0 && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[:end]
}

// SimilarityThresholdDefault is the default fuzzy similarity threshold (0.95).
const SimilarityThresholdDefault = 0.95
