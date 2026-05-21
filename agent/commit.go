package agent

import (
	"fmt"
	"strings"
)

// FormatTokens formats a token count for display (e.g. "1.2k", "3.5M").
func FormatTokens(n int64) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}

// ModelToCoAuthor converts a model name to a valid Co-authored-by name + email pair.
// The email local part is the model name lowercased with non-alphanumeric chars replaced by hyphens.
func ModelToCoAuthor(modelName string) string {
	if modelName == "" {
		return "AI <ai@tachi>"
	}
	emailLocal := SanitizeModelName(modelName)
	return modelName + " <" + emailLocal + "@tachi>"
}

// SanitizeModelName lowercases and replaces non-alphanumeric sequences with a single hyphen.
func SanitizeModelName(name string) string {
	var sb strings.Builder
	sb.Grow(len(name))
	prevDash := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
			prevDash = false
		} else {
			if !prevDash {
				sb.WriteRune('-')
				prevDash = true
			}
		}
	}
	return sb.String()
}
