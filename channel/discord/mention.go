package discord

import (
	"strings"
)

// isMentioned checks whether the bot's user ID is mentioned in the message content.
// Handles both `<@USER_ID>` (normal mention) and `<@!USER_ID>` (nickname mention).
// The `<@!` prefix is deprecated in API v10 but still sent by some clients.
func isMentioned(content string, botUserID string) bool {
	if botUserID == "" || content == "" {
		return false
	}
	// Check both mention formats.
	if strings.Contains(content, "<@"+botUserID+">") {
		return true
	}
	if strings.Contains(content, "<@!"+botUserID+">") {
		return true
	}
	return false
}

// extractMentionedUserIDs extracts all mentioned user IDs from the message content.
// Returns an empty slice if no mentions are found.
func extractMentionedUserIDs(content string) []string {
	var ids []string
	remaining := content
	for {
		// Find the start of a mention.
		start := strings.Index(remaining, "<@")
		if start < 0 {
			break
		}
		remaining = remaining[start+2:]

		// Determine the closing bracket position.
		end := strings.Index(remaining, ">")
		if end < 0 {
			break
		}

		mention := remaining[:end]
		remaining = remaining[end+1:]

		// Strip optional "!" prefix for nickname mentions.
		id := strings.TrimPrefix(mention, "!")
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// stripMentions removes Discord mention syntax from content, replacing
// `<@USER_ID>` and `<@!USER_ID>` with just the user ID for cleaner LLM input.
func stripMentions(content string) string {
	result := content
	// Replace `<@!USER_ID>` first (more specific), then `<@USER_ID>`.
	result = replaceBetween(result, "<@!", ">", "")
	result = replaceBetween(result, "<@", ">", "")
	return strings.TrimSpace(result)
}

// replaceBetween replaces all occurrences of text between start and end
// markers (inclusive) with the replacement string.
func replaceBetween(s, start, end, replacement string) string {
	var sb strings.Builder
	for {
		begin := strings.Index(s, start)
		if begin < 0 {
			sb.WriteString(s)
			break
		}
		sb.WriteString(s[:begin])
		s = s[begin+len(start):]

		stop := strings.Index(s, end)
		if stop < 0 {
			sb.WriteString(replacement)
			break
		}
		// Write the user ID as the replacement (so LLM can still see who was @mentioned).
		sb.WriteString(replacement + s[:stop])
		s = s[stop+len(end):]
	}
	return sb.String()
}
