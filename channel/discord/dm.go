package discord

import (
	"github.com/bwmarrin/discordgo"
	"strings"
)

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

// resolveThreadParent detects if channelID refers to a thread and returns
// its parent (the actual text/news/forum channel). Returns empty string and
// false when the channel is not a thread.
//
// It first checks the in-memory State cache for speed, falling back to a
// REST API call when State doesn't have the channel.
func resolveThreadParent(s *discordgo.Session, channelID string) (parentID string, isThread bool) {
	if s == nil {
		return "", false
	}

	var ch *discordgo.Channel
	var err error

	// Try State cache first (fast, no HTTP call).
	if s.State != nil {
		ch, err = s.State.Channel(channelID)
	}
	// Fall back to REST API if State doesn't have it.
	if err != nil || ch == nil {
		ch, err = s.Channel(channelID)
	}
	if err != nil || ch == nil {
		return "", false
	}

	if ch.IsThread() {
		return ch.ParentID, true
	}
	return "", false
}

// getThreadContext fetches and caches the initial conversation context of a
// thread — the starter message (the message the thread was created from) plus
// any messages that were sent in the first few exchanges. This covers the case
// where a user types a message in the thread creation dialog: Discord does not
// send MESSAGE_CREATE for that message, so we must fetch it via REST API.
//
// excludeMsgID is the ID of the message currently being processed; it will be
// filtered out of the context to avoid the current message appearing twice.
//
// Once fetched, the context is cached in memory for the lifetime of the bot
// session (thread starter messages and initial messages don't change).
//
// Returns a formatted string with the thread's early message history, or empty
// string if the thread doesn't have meaningful initial context.
func (ch *DiscordChannel) getThreadContext(threadID, excludeMsgID string) string {
	// Check cache first.
	ch.threadStarterCacheMu.Lock()
	if cached, ok := ch.threadStarterCache[threadID]; ok {
		ch.threadStarterCacheMu.Unlock()
		return cached
	}
	ch.threadStarterCacheMu.Unlock()

	sess := ch.session
	if sess == nil {
		return ""
	}

	// Fetch the first few messages in the thread (oldest first).
	// afterID="0" returns messages from the beginning.
	msgs, err := sess.ChannelMessages(threadID, 5, "", "0", "")
	if err != nil || len(msgs) == 0 {
		return ""
	}

	// Build a compact context string from the thread's initial messages.
	var parts []string
	for _, msg := range msgs {
		// Skip the message currently being processed (avoid duplicate).
		if msg.ID == excludeMsgID {
			continue
		}
		switch {
		case msg.Type == discordgo.MessageTypeThreadStarterMessage && msg.ReferencedMessage != nil:
			// Starter message — this is the original message the thread was
			// created from. Its content is in ReferencedMessage.
			author := resolveSenderName(msg.ReferencedMessage.Author)
			text := cleanContentForLLM(msg.ReferencedMessage.Content)
			if author != "" && author != "unknown" {
				parts = append(parts, "["+author+"]: "+text)
			} else {
				parts = append(parts, text)
			}

		case msg.Type == discordgo.MessageTypeDefault && msg.Author != nil:
			// A real user message in the thread.
			author := resolveSenderName(msg.Author)
			text := cleanContentForLLM(msg.Content)
			if text == "" {
				continue
			}
			if author != "" && author != "unknown" {
				parts = append(parts, "["+author+"]: "+text)
			} else {
				parts = append(parts, text)
			}
		}
	}

	if len(parts) == 0 {
		return ""
	}

	text := strings.Join(parts, "\n")

	// Store in cache.
	ch.threadStarterCacheMu.Lock()
	ch.threadStarterCache[threadID] = text
	ch.threadStarterCacheMu.Unlock()

	return text
}
