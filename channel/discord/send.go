package discord

import (
	"bytes"
	"strings"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
	"github.com/monsterxx03/tachi/pkg/channel"
)

const (
	// discordMessageLimit is the maximum length of a single Discord message.
	// discordgo v0.29.0 uses API v9, which enforces a 2000-character limit.
	discordMessageLimit = 2000
)

// splitMessage splits a long message into chunks that fit within Discord's
// message length limit. It prefers clean break points:
//  1. Paragraph boundaries ("\n\n" or "\n\r\n")
//  2. Newline boundaries
//  3. Word boundaries (space)
//  4. Hard split at the character limit (last resort)
//
// Returns at least one chunk (the original or first N chars if content is
// shorter than the limit).
func splitMessage(content string) []string {
	if content == "" {
		return nil
	}
	if len(content) <= discordMessageLimit {
		return []string{content}
	}

	var chunks []string
	remaining := content

	for len(remaining) > 0 {
		if len(remaining) <= discordMessageLimit {
			chunks = append(chunks, remaining)
			break
		}

		// Find the best split point within the limit.
		cut := findSplitPoint(remaining[:discordMessageLimit+1])
		chunks = append(chunks, remaining[:cut])
		remaining = remaining[cut:]
	}

	return chunks
}

// findSplitPoint finds the best position to split text within [0, limit).
// Priority: paragraph break > newline > space > hard cut.
func findSplitPoint(s string) int {
	limit := len(s)
	if limit > discordMessageLimit {
		limit = discordMessageLimit
	}

	// 1. Try paragraph break (double newline).
	if idx := lastIndexAny(s[:limit], "\n\n", "\n\r\n"); idx >= 0 {
		return idx + 1 // keep the first newline in the current chunk
	}

	// 2. Try single newline.
	if idx := strings.LastIndex(s[:limit], "\n"); idx >= 0 {
		return idx + 1
	}

	// 3. Try space boundary (avoid splitting words).
	if idx := strings.LastIndex(s[:limit], " "); idx >= 0 {
		return idx + 1
	}

	// 4. Hard split at the limit — ensure we don't split mid-rune.
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	if limit == 0 {
		limit = discordMessageLimit // safety fallback, shouldn't happen
	}
	return limit
}

// lastIndexAny finds the last index of any of the substrings in s[:limit].
func lastIndexAny(s string, substrs ...string) int {
	idx := -1
	for _, substr := range substrs {
		if i := strings.LastIndex(s, substr); i > idx {
			idx = i
		}
	}
	return idx
}

// sendText sends a text message to a Discord channel.
// Long messages are automatically split into multiple messages.
func (ch *DiscordChannel) sendText(channelID, content string) error {
	sess := ch.session
	if sess == nil {
		return nil // silently ignore if not connected
	}

	chunks := splitMessage(content)
	for _, chunk := range chunks {
		if _, err := sess.ChannelMessageSend(channelID, chunk); err != nil {
			return err
		}
	}
	return nil
}

// sendTextReply sends a text reply to a Discord channel at the given
// thread/channel. The replyTo parameter is the original message ID
// for thread reference (not used in basic send, but available for
// channel reply threading).
func (ch *DiscordChannel) sendTextReply(channelID, replyTo, content string) error {
	// For Phase 1, we send as a regular message.
	// Phase 2 can add threaded replies via ChannelMessageSendReply.
	return ch.sendText(channelID, content)
}

// buildThreadID constructs a ThreadID from guild and channel context.
func buildThreadID(guildID, channelID string) string {
	if guildID == "" {
		return "dm:" + channelID
	}
	return "guild:" + guildID + ":channel:" + channelID
}

// emojiToReaction formats an emoji string for use with Discord's reaction API.
// Standard emojis (like 👀) are used as-is.
// Custom emojis would need name:id format, but Phase 1 only uses standard emojis.
func emojiToReaction(emoji string) string {
	// Standard unicode emojis are used directly.
	if !strings.HasPrefix(emoji, ":") {
		return emoji
	}
	// Custom emoji handling (Phase 2+).
	return emoji
}

// cleanContentForLLM removes Discord-specific formatting from message content
// before sending it to the LLM. This includes:
//   - Stripping @mention syntax (keeping user IDs for context)
//   - Normalizing whitespace
func cleanContentForLLM(content string) string {
	// Strip mention syntax but keep user IDs.
	result := replaceBetween(content, "<@!", ">", "")
	result = replaceBetween(result, "<@", ">", "")
	return strings.TrimSpace(result)
}

// --- MEDIA tag ---

// parseMediaTags scans content for MEDIA:/path/to/file patterns, reads the files,
// and returns the cleaned content (with MEDIA tags removed) plus attachment data.
// Files that can't be read are left as-is (with a warning in the text).
func parseMediaTags(content string) (string, []channel.OutgoingAttachment) {
	var attachments []channel.OutgoingAttachment
	var buf strings.Builder
	remaining := content

	for {
		idx := strings.Index(remaining, "MEDIA:")
		if idx < 0 {
			buf.WriteString(remaining)
			break
		}

		// Write text before the tag.
		buf.WriteString(remaining[:idx])
		rest := remaining[idx+6:] // skip "MEDIA:"

		// Extract path (until whitespace or end).
		end := strings.IndexAny(rest, " \t\n\r")
		path := rest
		if end >= 0 {
			path = rest[:end]
			rest = rest[end:]
		} else {
			rest = ""
		}

		// Try to read the file.
		data, err := osReadFile(path)
		if err != nil {
			// Can't read — keep the MEDIA tag as text (with error note).
			buf.WriteString("[MEDIA:" + path + " (读取失败: " + err.Error() + ")]")
			remaining = rest
			continue
		}

		// Determine a display name from the path.
		name := path
		if idx := strings.LastIndexAny(name, "/\\"); idx >= 0 {
			name = name[idx+1:]
		}

		attachments = append(attachments, channel.OutgoingAttachment{
			Type:      channel.AttachmentTypeFile,
			FileName:  name,
			LocalPath: path,
			Data:      data,
		})

		// Replace MEDIA tag with filename reference in text.
		buf.WriteString("[" + name + "]")
		remaining = rest
	}

	return buf.String(), attachments
}

// --- Embed ---

// parseEmbedContent checks if the content starts with "EMBED:" and parses
// the first line into a Discord MessageEmbed. Format (single line):
//
//	EMBED:title|description|color
//
// All fields except title are optional. Returns the cleaned content (with
// the EMBED line removed) and the embed, or false if no embed prefix is found.
func parseEmbedContent(content string) (string, *discordgo.MessageEmbed, bool) {
	if !strings.HasPrefix(content, "EMBED:") {
		return content, nil, false
	}

	// Split the first line from the rest — EMBED only uses the first line.
	var embedLine string
	rest := ""
	if idx := strings.IndexByte(content, '\n'); idx >= 0 {
		embedLine = content[:idx]
		rest = strings.TrimLeft(content[idx+1:], "\n")
	} else {
		embedLine = content
	}

	// Parse EMBED format: EMBED:title|description|color
	raw := embedLine[6:]
	parts := strings.SplitN(raw, "|", 3)

	title := strings.TrimSpace(parts[0])
	if title == "" {
		return content, nil, false
	}

	embed := &discordgo.MessageEmbed{
		Title: title,
	}

	if len(parts) >= 2 {
		desc := strings.TrimSpace(parts[1])
		if desc != "" {
			embed.Description = desc
		}
	}

	if len(parts) >= 3 {
		colorStr := strings.TrimSpace(parts[2])
		if colorStr != "" {
			embed.Color = parseEmbedColor(colorStr)
		}
	}

	// Return remaining text after the EMBED line.
	return rest, embed, true
}

// parseEmbedColor converts a color string to an int.
// Supports hex format (#RRGGBB) and basic named colors.
func parseEmbedColor(s string) int {
	if strings.HasPrefix(s, "#") {
		c := 0
		for _, ch := range strings.TrimPrefix(s, "#") {
			c *= 16
			switch {
			case ch >= '0' && ch <= '9':
				c += int(ch - '0')
			case ch >= 'a' && ch <= 'f':
				c += int(ch-'a') + 10
			case ch >= 'A' && ch <= 'F':
				c += int(ch-'A') + 10
			default:
				return 0
			}
		}
		return c
	}

	switch strings.ToLower(s) {
	case "red":
		return 0xE74C3C
	case "green":
		return 0x2ECC71
	case "blue":
		return 0x3498DB
	case "yellow":
		return 0xF1C40F
	case "orange":
		return 0xE67E22
	case "purple":
		return 0x9B59B6
	case "gray", "grey":
		return 0x95A5A6
	default:
		return 0
	}
}

// sendEmbed sends a Discord embed message to the given channel.
func (ch *DiscordChannel) sendEmbed(channelID string, embed *discordgo.MessageEmbed) error {
	sess := ch.session
	if sess == nil {
		return nil
	}
	_, err := sess.ChannelMessageSendEmbed(channelID, embed)
	return err
}

// sendTextWithMedia sends text content, parsing MEDIA tags and uploading files.
// Returns the number of attachments sent.
func (ch *DiscordChannel) sendTextWithMedia(channelID string, content string) (int, error) {
	cleanContent, attachments := parseMediaTags(content)

	// Send the cleaned text.
	if cleanContent != "" {
		if err := ch.sendText(channelID, cleanContent); err != nil {
			return 0, err
		}
	}

	// Upload each attachment.
	sent := 0
	for _, att := range attachments {
		var err error
		if att.Data != nil {
			_, err = ch.session.ChannelFileSend(channelID, att.FileName, bytes.NewReader(att.Data))
		} else if att.LocalPath != "" {
			data, readErr := osReadFile(att.LocalPath)
			if readErr != nil {
				ch.logger.Log("discord: send media %s: %v", att.FileName, readErr)
				continue
			}
			_, err = ch.session.ChannelFileSend(channelID, att.FileName, bytes.NewReader(data))
		}
		if err != nil {
			ch.logger.Log("discord: send media %s error: %v", att.FileName, err)
			continue
		}
		sent++
	}

	return sent, nil
}
