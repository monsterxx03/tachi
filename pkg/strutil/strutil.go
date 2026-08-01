// Package strutil provides small, shared string utilities used across tachi
// (logging, previews, persistence). Functions in this package operate on
// runes rather than bytes so multi-byte characters (e.g. Chinese) are never
// split mid-sequence.
package strutil

import "strings"

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
