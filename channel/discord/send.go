package discord

import (
	"bytes"
	"context"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/pkg/channel"
)

// osReadFile is a package-level variable for testability.
// Tests replace it to simulate file read failures.
var osReadFile = os.ReadFile

const (
	// discordMessageLimit is the maximum length of a single Discord message.
	// discordgo v0.29.0 uses API v9, which enforces a 2000-character limit.
	discordMessageLimit = 2000
)

// splitMessage splits a long message into chunks that fit within Discord's
// message length limit (2000 characters, counted as Unicode code points).
// It prefers clean break points:
//  1. Paragraph boundaries ("\n\n" or "\n\r\n")
//  2. Newline boundaries
//  3. Word boundaries (space)
//  4. Hard split at the character limit — rune-safe
//
// Returns at least one chunk (the original or first N chars if content is
// shorter than the limit).
func splitMessage(content string) []string {
	if content == "" {
		return nil
	}
	if utf8.RuneCountInString(content) <= discordMessageLimit {
		return []string{content}
	}

	var chunks []string
	remaining := content

	for len(remaining) > 0 {
		if utf8.RuneCountInString(remaining) <= discordMessageLimit {
			chunks = append(chunks, remaining)
			break
		}

		// Find byte offset of the discordMessageLimit-th rune.
		byteLimit := runeCountToByteOffset(remaining, discordMessageLimit)
		cut := findSplitPoint(remaining, byteLimit)
		chunks = append(chunks, remaining[:cut])
		remaining = remaining[cut:]
	}

	return chunks
}

// runeCountToByteOffset returns the byte offset of the nth rune in s.
// If n exceeds the rune count, returns len(s).
func runeCountToByteOffset(s string, n int) int {
	pos := 0
	for i := 0; i < n && pos < len(s); i++ {
		_, size := utf8.DecodeRuneInString(s[pos:])
		pos += size
	}
	return pos
}

// findSplitPoint finds the best position to split text within [0, byteLimit).
// Priority: paragraph break > newline > space > rune-safe hard cut.
func findSplitPoint(s string, byteLimit int) int {
	limit := min(len(s), byteLimit)

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
		limit = byteLimit // safety fallback, shouldn't happen
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
// it into a Discord MessageEmbed with optional fields. Format:
//
//	EMBED:title|description|color
//	field:Name|Value|inline(可选)
//	field:Name2|Value2
//
// Only title is required on the EMBED line. field: lines after it define
// embed fields (inline=true/false, default false).
// Returns the remaining text (non-field lines) and the embed.
func parseEmbedContent(content string) (string, *discordgo.MessageEmbed, bool) {
	if !strings.HasPrefix(content, "EMBED:") {
		return content, nil, false
	}

	// Split the first line from the rest.
	var embedLine string
	rest := ""
	if before, after, ok := strings.Cut(content, "\n"); ok {
		embedLine = before
		rest = after
	} else {
		embedLine = content
	}

	// Parse EMBED:title|description|color
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
		if desc := strings.TrimSpace(parts[1]); desc != "" {
			embed.Description = desc
		}
	}

	if len(parts) >= 3 {
		if colorStr := strings.TrimSpace(parts[2]); colorStr != "" {
			embed.Color = parseEmbedColor(colorStr)
		}
	}

	// Parse field: lines from the rest.
	var textLines []string
	for line := range strings.SplitSeq(rest, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "field:") {
			fieldContent := trimmed[6:]
			fieldParts := strings.SplitN(fieldContent, "|", 3)
			if len(fieldParts) >= 2 {
				name := strings.TrimSpace(fieldParts[0])
				value := strings.TrimSpace(fieldParts[1])
				if name != "" {
					inline := false
					if len(fieldParts) >= 3 {
						inline = strings.TrimSpace(fieldParts[2]) == "true"
					}
					embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
						Name:   name,
						Value:  value,
						Inline: inline,
					})
				}
			}
		} else {
			textLines = append(textLines, line)
		}
	}

	return strings.TrimLeft(strings.Join(textLines, "\n"), "\n"), embed, true
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
		data, err := channel.ResolveAttachmentData(att)
		if err != nil {
			ch.logger.Error(context.Background(), "discord: send media resolve failed", err, "file", att.FileName)
			continue
		}
		if _, err := ch.session.ChannelFileSend(channelID, att.FileName, bytes.NewReader(data)); err != nil {
			ch.logger.Error(context.Background(), "discord: send media error", err, "file", att.FileName)
			continue
		}
		sent++
	}

	return sent, nil
}

// buildStreamingDesc builds the embed description for the streaming status card.
// Combines accumulated LLM text with tool call lines, truncating at Discord's
// embed description limit (4096 runes) with preference to keep tool calls.
func buildStreamingDesc(text, tools string, toolCount int, maxRunes int) string {
	// Fast path: no text, just tool calls in a code block.
	if text == "" {
		desc := "```\n" + tools + "```"
		if utf8.RuneCountInString(desc) <= maxRunes {
			return desc
		}
		// Truncate tool calls only.
		return truncateStreamingTools(tools, toolCount, maxRunes)
	}

	// Build combined description: text + tool calls.
	combined := text
	if tools != "" {
		combined += "\n```\n" + tools + "```"
	}

	if utf8.RuneCountInString(combined) <= maxRunes {
		return combined
	}

	// Over the limit — truncate text first, then tools if still over.
	// Calculate how much room is left after reserving space for tool calls.
	reserved := 7 // "```\n" and "```"
	if tools != "" {
		reserved += utf8.RuneCountInString(tools) + 7
	}
	textBudget := maxRunes - reserved

	if textBudget < 10 {
		// Too many tool calls, very little room for text — truncate tools too.
		// Show at most 3 tool calls inline without code block.
		shortTools := truncateStreamingToolsShort(tools)
		combined = shortTools
		if text != "" {
			combined = text + "\n" + combined
		}
		if utf8.RuneCountInString(combined) > maxRunes {
			combined = truncateRunes(text, maxRunes-20) + "\n…"
		}
		return combined
	}

	// Truncate text to fit within textBudget.
	truncated := truncateRunes(text, textBudget)
	if truncated != text {
		truncated += "…"
	}

	combined = truncated
	if tools != "" {
		combined += "\n```\n" + tools + "```"
	}

	// Final check — if still over, hard truncate at maxRunes.
	if utf8.RuneCountInString(combined) > maxRunes {
		combined = truncateRunes(combined, maxRunes-5) + "\n…"
	}

	return combined
}

// truncateStreamingTools truncates the tool calls block at maxRunes runes
// and appends a "… 还有 N 个调用" counter for the remaining calls.
func truncateStreamingTools(tools string, toolCount int, maxRunes int) string {
	visible := strings.Count(tools, "\n")
	pos := 0
	for i := 0; i < maxRunes-20 && pos < len(tools); i++ {
		_, size := utf8.DecodeRuneInString(tools[pos:])
		pos += size
	}
	return tools[:pos] + "\n… 还有 " + strconv.Itoa(toolCount-visible) + " 个调用"
}

// truncateStreamingToolsShort returns a compact inline summary of tool calls.
func truncateStreamingToolsShort(tools string) string {
	lines := strings.Split(strings.TrimRight(tools, "\n"), "\n")
	if len(lines) > 3 {
		lines = lines[:3]
	}
	return strings.Join(lines, "\n") + "\n…"
}

// truncateRunes truncates s to at most n runes, respecting UTF-8 boundaries.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	pos := 0
	for i := 0; i < n && pos < len(s); i++ {
		_, size := utf8.DecodeRuneInString(s[pos:])
		pos += size
	}
	return s[:pos]
}

// formatToolArgsForEmbed formats tool call arguments for display in a Discord
// embed status line. Delegates to tools.ToolArgsSummary and adds a " — "
// prefix when the summary differs from the raw JSON.
func formatToolArgsForEmbed(toolName, argsJSON string) string {
	if argsJSON == "" {
		return ""
	}
	summary := tools.ToolArgsSummary(toolName, argsJSON)
	if summary == "" || summary == argsJSON {
		return ""
	}
	return " — " + summary
}
