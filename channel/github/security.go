package github

import (
	"regexp"
	"strings"
)

// WrapAsUntrusted wraps the given content in UNTRUSTED markers, making it clear
// to the LLM that the wrapped content is user input that should not be trusted.
func WrapAsUntrusted(content string) string {
	return "--- BEGIN UNTRUSTED ISSUE CONTENT ---\n" + content + "\n--- END UNTRUSTED ISSUE CONTENT ---"
}

// WrapCommentAsUntrusted wraps a single comment with author attribution.
func WrapCommentAsUntrusted(author, body string) string {
	return "--- BEGIN UNTRUSTED COMMENT (" + author + ") ---\n" + body + "\n--- END UNTRUSTED COMMENT ---"
}

var (
	controlMarkerRe = regexp.MustCompile(`\s*\[(READY_FOR_PR|NO_REPLY|IMPLEMENT)\]\s*$`)
)

// StripControlMarkers removes protocol control markers ([NO_REPLY], [READY_FOR_PR])
// from agent output before posting to GitHub.
func StripControlMarkers(text string) string {
	return controlMarkerRe.ReplaceAllString(text, "")
}

// HasControlMarker checks if the agent output contains a specific control marker.
// The marker is matched at the end of the text after trimming whitespace.
func HasControlMarker(text, marker string) bool {
	trimmed := strings.TrimSpace(text)
	re := regexp.MustCompile(`\s*\[` + regexp.QuoteMeta(marker) + `\]\s*$`)
	return re.MatchString(trimmed)
}

// IsNoReply checks if the agent chose to stay silent.
func IsNoReply(text string) bool {
	return HasControlMarker(text, "NO_REPLY")
}
