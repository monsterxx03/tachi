package discord

// isDM returns true if the message was sent via direct message (not in a guild channel).
// In Discord, DM messages have an empty GuildID.
func isDM(guildID string) bool {
	return guildID == ""
}

// threadIDForDM returns the ThreadID for a DM conversation.
// Format: "dm:<userID>"
func threadIDForDM(userID string) string {
	return "dm:" + userID
}

// threadIDForGuild returns the ThreadID for a guild channel conversation.
// Format: "guild:<guildID>:channel:<channelID>"
func threadIDForGuild(guildID, channelID string) string {
	return "guild:" + guildID + ":channel:" + channelID
}

// channelIDFromThreadID extracts the Discord channel ID from a ThreadID.
// Returns empty string if the ThreadID format is unrecognized.
func channelIDFromThreadID(threadID string) string {
	// DM format: "dm:<channelID>"
	if len(threadID) > 3 && threadID[:3] == "dm:" {
		return threadID[3:]
	}
	// Guild format: "guild:<guildID>:channel:<channelID>"
	// Parse from the end: find last ":channel:" prefix
	const channelPrefix = ":channel:"
	if idx := lastIndexStr(threadID, channelPrefix); idx >= 0 {
		return threadID[idx+len(channelPrefix):]
	}
	return ""
}

// lastIndexStr finds the last occurrence of substr in s.
func lastIndexStr(s, substr string) int {
	for i := len(s) - len(substr); i >= 0; i-- {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
