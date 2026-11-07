//go:build !headless

package main

import (
	"strings"
	"unicode"

	"github.com/mozillazg/go-pinyin"
)

// Matching a typed query against one application.
//
// The tiers are ORDERED, and that order is the whole design: what a person types first is the
// beginning of a name ("chr" for Chrome), and a row that matches at the start must never sit below
// one that merely contains those letters somewhere. Usage only breaks ties INSIDE a tier — a
// frequently used app must not jump above an exact name match, or the launcher stops being
// predictable the moment it has been used a few times.
const (
	// tierExact is the name itself (or an alias) — 「微信」 typed exactly.
	tierExact = 6
	// tierPrefix is a name that starts with what was typed.
	tierPrefix = 5
	// tierWord is the beginning of a word inside a multi-word name ("chr" → Google Chrome,
	// "gc" → Google Chrome), the start of the pinyin (wx → 微信), or the start of the initials.
	// This is where a pinyin-first user lands, and where an app whose bundle name carries a
	// vendor prefix ("Google Chrome") stays reachable by the word the reader means.
	tierWord = 4
	// tierContains is a name with the query somewhere inside it.
	tierContains = 3
	// tierPinyinContains is a match inside the pinyin/initials rather than the name.
	tierPinyinContains = 2
	// tierSubsequence is the letters in order but not together ("gchrm" → Google Chrome). The
	// loosest tier, and the one that lets a typo still land.
	tierSubsequence = 1
)

// minSubsequence is the shortest query the loose tier will answer: one letter matches nearly every
// app, and a list where everything matches is a list that says nothing.
const minSubsequence = 2

// matchTier reports how well an app answers a query, and whether it answers at all. q must already
// be normalized (lower-cased, trimmed — see normalizeQuery).
func (a *launcherApp) matchTier(q string) (int, bool) {
	if q == "" {
		return tierExact, true
	}
	if a.nameLower == q {
		return tierExact, true
	}
	if strings.HasPrefix(a.nameLower, q) {
		return tierPrefix, true
	}
	for _, alias := range a.aliasesLower {
		if alias == q {
			return tierExact, true
		}
		if strings.HasPrefix(alias, q) {
			return tierPrefix, true
		}
	}
	if strings.HasPrefix(a.pinyinInitials, q) || strings.HasPrefix(a.wordInitials, q) {
		return tierWord, true
	}
	for _, word := range a.words {
		if strings.HasPrefix(word, q) {
			return tierWord, true
		}
	}
	for _, variant := range a.pinyinVariants {
		if strings.HasPrefix(variant, q) {
			return tierWord, true
		}
	}
	if strings.Contains(a.nameLower, q) {
		return tierContains, true
	}
	for _, alias := range a.aliasesLower {
		if strings.Contains(alias, q) {
			return tierContains, true
		}
	}
	if strings.Contains(a.pinyinInitials, q) || strings.Contains(a.wordInitials, q) {
		return tierPinyinContains, true
	}
	for _, variant := range a.pinyinVariants {
		if strings.Contains(variant, q) {
			return tierPinyinContains, true
		}
	}
	if len([]rune(q)) >= minSubsequence && (isSubsequence(q, a.nameLower) || isSubsequence(q, a.pinyinInitials)) {
		return tierSubsequence, true
	}
	return 0, false
}

// isSubsequence reports whether every rune of q appears in s, in order. Both must be lower-case.
func isSubsequence(q, s string) bool {
	qr := []rune(q)
	i := 0
	for _, r := range s {
		if i == len(qr) {
			return true
		}
		if r == qr[i] {
			i++
		}
	}
	return i == len(qr)
}

// pinyinArgs is the style names are converted with: no tones (a reader types "weixin", never
// "wēixìn"), and EVERY reading of a heteronym, because the one the dictionary lists first is not
// always the one the app's name uses — 音乐 is 「yinyue」, not 「yinle」.
var pinyinArgs = func() pinyin.Args {
	args := pinyin.NewArgs()
	args.Style = pinyin.Normal
	args.Heteronym = true
	return args
}()

// maxPinyinVariants bounds the readings a name is expanded into. Most names have one reading per
// character; a name where the alternatives would multiply past this is matched on its first
// readings only, which costs a rare alternate spelling and keeps the catalog small.
const maxPinyinVariants = 24

// pinyinForms is what a pinyin-typing reader might have typed for one name.
type pinyinForms struct {
	// initials is the first letter of each syllable: 微信 → "wx".
	initials string
	// variants are the whole syllable strings, one per combination of readings: 网易云音乐 →
	// "wangyiyunyinyue", "wangyiyunyinle", … The first entry is all first readings.
	variants []string
}

// pinyinFormsOf converts a name into those forms. Latin letters and digits are kept in both, so a
// name that is already English ("IINA") behaves the same through either path.
func pinyinFormsOf(name string) pinyinForms {
	// readings[i] is every pronunciation of the i-th character; a Latin character has exactly one,
	// itself.
	readings := make([][]string, 0, len(name))
	var initials strings.Builder
	for _, r := range name {
		if unicode.Is(unicode.Han, r) {
			if pronunciations := pinyin.SinglePinyin(r, pinyinArgs); len(pronunciations) > 0 {
				lowered := make([]string, 0, len(pronunciations))
				for _, pronunciation := range pronunciations {
					if pronunciation != "" {
						lowered = append(lowered, strings.ToLower(pronunciation))
					}
				}
				if len(lowered) > 0 {
					readings = append(readings, lowered)
					initials.WriteString(lowered[0][:1])
					continue
				}
			}
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			lower := string(unicode.ToLower(r))
			readings = append(readings, []string{lower})
			initials.WriteString(lower)
		}
		// Spaces and punctuation take part in neither: 「Visual Studio Code」 is one key to type
		// through, and no one types the space in the middle of a launcher query.
	}

	if combinations(readings) > maxPinyinVariants {
		for i, choices := range readings {
			readings[i] = choices[:1]
		}
	}
	variants := []string{""}
	for _, choices := range readings {
		next := make([]string, 0, len(variants)*len(choices))
		for _, prefix := range variants {
			for _, choice := range choices {
				next = append(next, prefix+choice)
			}
		}
		variants = next
	}
	return pinyinForms{initials: initials.String(), variants: dedupe(variants)}
}

// combinations is how many strings the readings would expand into (capped so it cannot overflow on
// a pathological name).
func combinations(readings [][]string) int {
	total := 1
	for _, choices := range readings {
		total *= len(choices)
		if total > maxPinyinVariants {
			return total
		}
	}
	return total
}

// words are a name's words, lower-cased — 「Google Chrome」 is matched by "chrome" through these,
// which is the word a reader means when the bundle name carries a vendor prefix.
func nameWords(name string) []string {
	words := make([]string, 0, 4)
	var current strings.Builder
	flush := func() {
		if current.Len() > 0 {
			words = append(words, current.String())
			current.Reset()
		}
	}
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			current.WriteRune(unicode.ToLower(r))
			continue
		}
		flush()
	}
	flush()
	return words
}

// wordInitials is the first letter of each word — "Google Chrome" → "gc".
func wordInitials(name string) string {
	var b strings.Builder
	for _, word := range nameWords(name) {
		b.WriteString(word[:1])
	}
	return b.String()
}

func dedupe(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := values[:0]
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

// normalizeQuery is what every comparison is made against: what a person typed, trimmed and
// lower-cased. Chinese is left as it is (case does not apply), which is what makes 「微信」 work
// without a special path.
func normalizeQuery(q string) string {
	return strings.ToLower(strings.TrimSpace(q))
}
