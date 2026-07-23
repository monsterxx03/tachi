package github

import (
	"regexp"
	"strings"
)

// injectionPatterns are regex patterns that indicate potential prompt injection attempts.
// Matches are logged for review but content is NOT modified — altering content would
// break legitimate issue text and tip off attackers about filtering rules.
//
// Patterns are split into two confidence levels:
//   - High confidence: specific patterns that strongly indicate an attack.
//     Logged at WARN level.
//   - Low confidence: broader patterns that may match legitimate content.
//     Logged at INFO level to avoid alert fatigue.
var (
	highConfidencePatterns = map[string]*regexp.Regexp{
		"ignore_instructions": regexp.MustCompile(`(?i)ignore (all )?(previous|prior|above) instructions`),
		"role_impersonation":  regexp.MustCompile(`(?i)you are (now |an? )?(system |assistant |GPT |Claude |Tachi)`),
		"forget_instructions": regexp.MustCompile(`(?i)forget (everything|all|your instructions|yourself)`),
		"system_reminder":     regexp.MustCompile(`<system-reminder>`),
		"available_skills":    regexp.MustCompile(`<available-skills>`),
		"untrusted_marker":    regexp.MustCompile(`--- (BEGIN|END) (UNTRUSTED|SYSTEM|SECRET)`),
		"control_marker":      regexp.MustCompile(`\[(READY_FOR_PR|NO_REPLY|IMPLEMENT|SKIP)\]`),
	}

	// Low-confidence patterns may match legitimate content like "installation instructions:".
	// They are logged at INFO level for awareness without triggering alert fatigue.
	lowConfidencePatterns = map[string]*regexp.Regexp{
		"new_instructions": regexp.MustCompile(`(?i)(new |updated )?instructions?[:\n]`),
	}
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
	noReplyRe       = regexp.MustCompile(`\s*\[NO_REPLY\]\s*$`)
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
