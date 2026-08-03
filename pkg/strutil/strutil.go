// Package strutil provides small, shared string utilities used across tachi
// (logging, previews, persistence). Functions in this package operate on
// runes rather than bytes so multi-byte characters (e.g. Chinese) are never
// split mid-sequence.
package strutil

import (
	"fmt"
	"math"
	"strings"

	"github.com/google/uuid"
)

// truncate returns the first max runes of s, never splitting multi-byte
// characters (e.g. Chinese) mid-sequence. truncated reports whether s was
// actually longer than max. max <= 0 yields ("", false).
func truncate(s string, max int) (out string, truncated bool) {
	if max <= 0 {
		return "", false
	}
	// Fast path: byte length within limit implies rune count within limit.
	if len(s) <= max {
		return s, false
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s, false
	}
	return string(runes[:max]), true
}

// Truncate caps s at max runes. If s is longer, only the first max runes are
// kept and a "..." suffix is appended (result length max+3 runes). Multi-byte
// characters (e.g. Chinese) are never split mid-sequence.
//
// max <= 0 yields "".
func Truncate(s string, max int) string {
	out, truncated := truncate(s, max)
	if truncated {
		return out + "..."
	}
	return out
}

// TruncatePlain is like Truncate but without the ellipsis: it returns at
// most the first max runes of s. Multi-byte characters are never split.
//
// max <= 0 yields "".
func TruncatePlain(s string, max int) string {
	out, _ := truncate(s, max)
	return out
}

// TruncateFitted caps s at exactly max runes. If s is longer, the last kept
// rune position is replaced with a single "…" so the result is exactly max
// runes (ellipsis included in the budget). Multi-byte characters are never
// split mid-sequence.
//
// max <= 0 yields "".
func TruncateFitted(s string, max int) string {
	if max <= 0 {
		return ""
	}
	// Fast path: byte length within limit implies rune count within limit.
	if len(s) <= max {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}

// FirstLine returns the first line of s — everything up to (and excluding)
// the first newline. If s has no newline, s is returned unchanged.
func FirstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx]
	}
	return s
}

// FirstLineOrTruncate returns the first line of s (via FirstLine), truncated
// via Truncate to max runes with an ellipsis if needed.
func FirstLineOrTruncate(s string, max int) string {
	return Truncate(FirstLine(s), max)
}

// IsCJK reports whether r is a CJK character (Chinese/Japanese/Korean).
// Covers the union of blocks used across tachi: CJK Radicals Supplement
// through Unified Ideographs (incl. Extension A, Hiragana, Katakana),
// Compatibility Ideographs, Extension B, and Hangul Syllables.
func IsCJK(r rune) bool {
	return (r >= 0x2E80 && r <= 0x9FFF) || // Radicals Supplement .. Unified Ideographs
		(r >= 0xF900 && r <= 0xFAFF) || // CJK Compatibility Ideographs
		(r >= 0x20000 && r <= 0x2A6DF) || // CJK Unified Ideographs Extension B
		(r >= 0xAC00 && r <= 0xD7AF) // Hangul Syllables
}

// ShortUUID returns the first n characters of a random UUID v4 string with
// hyphens removed. Used for collision-resistant short IDs (session IDs,
// worktree names, etc).
func ShortUUID(n int) string {
	return strings.ReplaceAll(uuid.New().String(), "-", "")[:n]
}

// HumanBytes formats a byte count as a human-readable string with units
// (B, KB, MB, GB).
func HumanBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	case n < 1024*1024*1024:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	default:
		return fmt.Sprintf("%.1f GB", float64(n)/(1024*1024*1024))
	}
}

// FormatMagnitude renders v/div with one decimal place, dropping a redundant
// ".0" so 12_000 → "12k" while 7_300 → "7.3k". Used for humanizing token
// rates and other magnitudes (see FormatTPS).
func FormatMagnitude(v, div int64, unit string) string {
	r := float64(v) / float64(div)
	if rounded := math.Round(r*10) / 10; rounded == math.Trunc(rounded) {
		return fmt.Sprintf("%.0f%s", rounded, unit)
	}
	return fmt.Sprintf("%.1f%s", r, unit)
}

// FormatTPS renders a tokens-per-second value in human-friendly form with a
// "/s" suffix: ≥1k → "7.3k/s" (or "12k/s" when the decimal is redundant),
// ≥1M → "1.2m/s", below 1k → "200/s".
func FormatTPS(tps int64) string {
	switch {
	case tps >= 1_000_000:
		return FormatMagnitude(tps, 1_000_000, "m") + "/s"
	case tps >= 1_000:
		return FormatMagnitude(tps, 1_000, "k") + "/s"
	default:
		return fmt.Sprintf("%d/s", tps)
	}
}

// SplitBy splits s by sep into non-empty items, trimming whitespace from
// each item. Empty and whitespace-only items are dropped, so
// SplitBy("a, b, ,c", ",") → ["a", "b", "c"] and
// SplitBy(" a\n\n b ", "\n") → ["a", "b"]. An empty or whitespace-only
// string yields a nil slice. Used for --allowed-tools style CLI flags, MCP
// select lists, and multi-line command output parsing.
func SplitBy(s string, sep string) []string {
	var out []string
	for item := range strings.SplitSeq(s, sep) {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// SanitizeFilename replaces characters that are problematic in filenames
// (slash, colon, quotes, angle brackets, pipe, spaces, …) with "_", then
// truncates to max runes (max <= 0 means no truncation). Multi-byte
// characters are never split.
func SanitizeFilename(s string, max int) string {
	if s == "" {
		return ""
	}
	result := strings.NewReplacer(
		"/", "_",
		"\\", "_",
		":", "_",
		"*", "_",
		"?", "_",
		"\"", "_",
		"<", "_",
		">", "_",
		"|", "_",
		" ", "_",
	).Replace(s)
	if max > 0 {
		return TruncatePlain(result, max)
	}
	return result
}
