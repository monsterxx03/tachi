package discord

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/bwmarrin/discordgo"
	"github.com/monsterxx03/tachi/channel/manager"
	"github.com/monsterxx03/tachi/pkg/channel"
)

// handleMessageCreate processes a single MESSAGE_CREATE event through
// the full pipeline: dedup, filter, construct, delegate, respond.
func (ch *DiscordChannel) handleMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate, handler channel.MessageHandler) {
	// 0. Wait until bot is ready (botUserID known).
	ch.mu.RLock()
	botUserID := ch.botUserID
	ch.mu.RUnlock()
	if botUserID == "" {
		return
	}

	// 1. Ignore messages from the bot itself.
	if m.Author == nil || m.Author.ID == botUserID || m.Author.Bot {
		return
	}

	// 2. Message deduplication.
	if ch.deduper.seen(m.ID) {
		ch.logger.Log("discord: duplicate message %s skipped", m.ID)
		return
	}

	// 3. Channel-level filtering.
	if !ch.isAllowedChannel(m.ChannelID) {
		return
	}

	// 4. Determine if this is a DM.
	dm := isDM(m.GuildID)

	// 5. Determine directed status.
	directed := dm || isMentioned(m.Content, botUserID)

	// Free-response channels: all messages are treated as directed.
	if !dm && ch.isFreeResponseChannel(m.ChannelID) {
		directed = true
	}

	// When require_mention is false, every message that passes the
	// filter below is intended to get a direct reply — treat it
	// as directed rather than ambient/whisper.
	if !dm && !ch.cfg.RequireMention {
		directed = true
	}

	// 6. Check mention strategy in guild channels.
	if !dm {
		if ch.cfg.RequireMention && !directed && !ch.isFreeResponseChannel(m.ChannelID) {
			// Non-directed message in a guild that requires @mention and is
			// not a free-response channel — ignore (whisper/ambient handled
			// by the manager layer via GroupChat flag).
			return
		}

		// If IgnoreOtherMentions is true and bot isn't mentioned but others are, ignore.
		if ch.cfg.IgnoreOtherMentions && !directed && containsMention(m.Content, botUserID) {
			// Someone else was @mentioned but not bot → ignore.
			return
		}
	}

	// 7. Access control.
	roles := ch.resolveMemberRoles(m.GuildID, m.Author.ID)
	if !ch.isAuthorized(m.Author.ID, roles, dm) {
		ch.logger.Log("discord: unauthorized user %s (%s) in channel %s",
			m.Author.ID, m.Author.Username, m.ChannelID)
		return
	}

	// 8. Build the ThreadID.
	var threadID string
	if dm {
		threadID = threadIDForDM(m.Author.ID)
	} else {
		threadID = threadIDForGuild(m.GuildID, m.ChannelID)
	}

	// 9. Construct the IncomingMessage.
	incoming := ch.buildIncomingMessage(m, threadID, dm, directed)

	// 10. Start typing indicator and status embed only for directed
	// messages (ambient/whisper messages skip visible feedback).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if directed {
		stopTyping := ch.startTypingLoop(ctx, m.ChannelID)
		defer stopTyping()

		// 11. Set up streaming callback for real-time tool call progress.
		// The status embed is only sent when the first tool call is detected,
		// skipping the embed entirely for simple text-only replies.
		var (
			embedMsgID string
			toolBuf    strings.Builder
			mu         sync.Mutex
		)
		ctx = manager.WithStreamingCallback(ctx, func(textDelta string) error {
			if !strings.HasPrefix(textDelta, "\n\n> <font color=\"comment\">🔧 ") {
				return nil
			}
			// Format: "\n\n> <font color=\"comment\">🔧 ToolName args</font>\n\n"
			cleaned := strings.TrimPrefix(textDelta, "\n\n> <font color=\"comment\">🔧 ")
			if idx := strings.Index(cleaned, "</font>"); idx >= 0 {
				cleaned = cleaned[:idx]
			}

			mu.Lock()
			toolBuf.WriteString("🔧 " + cleaned + "\n")
			desc := "```\n" + toolBuf.String() + "```"
			mu.Unlock()

			sess := ch.session
			if sess == nil {
				return nil
			}

			if embedMsgID == "" {
				// First tool call — send the status embed now.
				sent, err := sess.ChannelMessageSendEmbed(m.ChannelID, &discordgo.MessageEmbed{
					Title:       "🤖 Tachi",
					Description: desc,
					Color:       0x3498DB,
				})
				if err == nil {
					embedMsgID = sent.ID
				}
			} else {
				// Subsequent tool call — update the existing embed.
				_, _ = sess.ChannelMessageEditEmbed(m.ChannelID, embedMsgID, &discordgo.MessageEmbed{
					Title:       "🤖 Tachi",
					Description: desc,
					Color:       0x3498DB,
				})
			}
			return nil
		})
	}

	// 11.5 Delegate to the manager handler.
	result := handler(ctx, incoming)

	// 12. Process the result.
	ch.processHandlerResult(m, result, threadID)
}

// buildIncomingMessage constructs a channel.IncomingMessage from a Discord
// MessageCreate event.
func (ch *DiscordChannel) buildIncomingMessage(m *discordgo.MessageCreate, threadID string, dm, directed bool) channel.IncomingMessage {
	content := cleanContentForLLM(m.Content)

	// TODO: Channel prompts should be injected into the system prompt via
	// SystemPromptSuffixer, not prepended to user messages. However, the
	// current SystemPromptSuffix() interface has no way to know which
	// Discord channel the current message belongs to. Once the interface
	// is extended to carry context (e.g., a context.Context parameter),
	// wire ch.cfg.ChannelPrompts into SystemPromptSuffix() using
	// channelIDFromThreadID(threadID) to look up the prompt.

	senderName := resolveSenderName(m.Author)

	// In shared guild sessions, prepend sender name so LLM knows who said what.
	if !dm && senderName != "" && senderName != "unknown" {
		content = "[" + senderName + "]: " + content
	}

	msg := channel.IncomingMessage{
		ThreadID:  threadID,
		MessageID: m.ID,
		Content:   content,
		ChannelID: m.ChannelID,
		Sender:    senderName,
		Directed:  directed,
		GroupChat: !dm,
	}

	// Handle attachments.
	if len(m.Attachments) > 0 {
		for _, att := range m.Attachments {
			downloaded, err := ch.downloadAttachment(att.URL)
			if err != nil {
				ch.logger.Log("discord: download attachment %s: %v", att.Filename, err)
				msg.Attachments = append(msg.Attachments, channel.Attachment{
					FileName: att.Filename,
					Error:    err.Error(),
				})
				continue
			}

			// Save to cache.
			savedPath := ""
			if path, err := ch.saveAttachment(downloaded); err == nil {
				savedPath = path
			}

			channelAtt := channel.Attachment{
				Type:      resolveAttachmentType(downloaded.MimeType),
				FileName:  downloaded.FileName,
				MimeType:  downloaded.MimeType,
				Content:   downloaded.Data,
				Size:      downloaded.Size,
				SavedPath: savedPath,
			}

			// For text files, extract text content if within size limits.
			if isTextContent(downloaded.MimeType) && isWithinTextInjectionLimit(downloaded.Size) {
				channelAtt.TextContent = string(downloaded.Data)
			}

			msg.Attachments = append(msg.Attachments, channelAtt)
		}
	}

	return msg
}

// processHandlerResult handles the result of a manager handler call.
func (ch *DiscordChannel) processHandlerResult(m *discordgo.MessageCreate, result channel.HandlerResult, threadID string) {
	channelID := m.ChannelID

	if result.Steered {
		return
	}

	if result.Buffered {
		return
	}

	if result.Streamed {
		return
	}

	if result.Err != nil {
		ch.logger.Log("discord: handler error: %v", result.Err)
		return
	}

	// Send the reply.
	reply := result.Reply
	if reply.Content == "" {
		return
	}

	// 1. Check for EMBED prefix.
	if cleaned, embed, ok := parseEmbedContent(reply.Content); ok {
		if err := ch.sendEmbed(channelID, embed); err != nil {
			ch.logger.Log("discord: send embed error: %v", err)
		}
		// Send remaining text after embed, with MEDIA parsing.
		if cleaned != "" {
			if _, err := ch.sendTextWithMedia(channelID, cleaned); err != nil {
				ch.logger.Log("discord: send embed text error: %v", err)
			}
		}
	} else {
		// 2. Normal text with MEDIA tag support.
		if _, err := ch.sendTextWithMedia(channelID, reply.Content); err != nil {
			ch.logger.Log("discord: send reply error: %v", err)
		}
	}

	// 3. Send any explicit attachments from the reply (besides MEDIA ones).
	for _, att := range reply.Attachments {
		var err error
		if att.Data != nil {
			_, err = ch.session.ChannelFileSend(channelID, att.FileName, bytes.NewReader(att.Data))
		} else if att.LocalPath != "" {
			data, readErr := osReadFile(att.LocalPath)
			if readErr != nil {
				ch.logger.Log("discord: send attachment read %s: %v", att.FileName, readErr)
				continue
			}
			_, err = ch.session.ChannelFileSend(channelID, att.FileName, bytes.NewReader(data))
		}
		if err != nil {
			ch.logger.Log("discord: send attachment %s error: %v", att.FileName, err)
		}
	}

	// 4. Update channel topic with working directory and git branch.
	// Only for guild channels (DM channels don't have a meaningful topic).
	if !isDM(m.GuildID) {
		ch.updateChannelTopic(channelID)
	}
}

// resolveSenderName returns the best display name for a user.
// Prefers the global username; falls back to ID if both are empty.
func resolveSenderName(user *discordgo.User) string {
	if user == nil {
		return "unknown"
	}
	if user.GlobalName != "" {
		return user.GlobalName
	}
	if user.Username != "" {
		return user.Username
	}
	return user.ID
}

// resolveAttachmentType maps MIME types to channel.AttachmentType.
func resolveAttachmentType(mimeType string) channel.AttachmentType {
	if strings.HasPrefix(mimeType, "text/") {
		return channel.AttachmentTypeText
	}
	if strings.HasPrefix(mimeType, "image/") {
		return channel.AttachmentTypeImage
	}
	return channel.AttachmentTypeFile
}

// containsMention checks whether the message contains any @mention
// (including mentions of other users), as opposed to isMentioned which
// specifically checks for the bot user.
func containsMention(content, botUserID string) bool {
	return strings.Contains(content, "<@") && !isMentioned(content, botUserID)
}

// osReadFile is a wrapper around os.ReadFile for testability.
var osReadFile = osReadFileFunc

func osReadFileFunc(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// updateChannelTopic retrieves the current working directory and git branch,
// then updates the given channel's topic if anything has changed since the
// last update. Skips if the channel is a DM.
func (ch *DiscordChannel) updateChannelTopic(channelID string) {
	dir, err := os.Getwd()
	if err != nil {
		return
	}

	branch := ""
	if b, err := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output(); err == nil {
		branch = strings.TrimSpace(string(b))
	}

	// Check if anything changed vs cached status.
	ch.topicStatusMu.Lock()
	last, seen := ch.topicStatus[channelID]
	if seen && last.dir == dir && last.branch == branch {
		ch.topicStatusMu.Unlock()
		return
	}
	ch.topicStatus[channelID] = topicEntry{dir: dir, branch: branch}
	ch.topicStatusMu.Unlock()

	// Build the new topic text.
	topic := "`" + dir + "`"
	if branch != "" {
		topic += " · `" + branch + "`"
	}

	sess := ch.session
	if sess == nil {
		return
	}
	if _, err := sess.ChannelEdit(channelID, &discordgo.ChannelEdit{
		Topic: topic,
	}); err != nil {
		ch.logger.Log("discord: update channel topic for %s: %v", channelID, err)
	}
}
