package discord

import (
	"bytes"
	"context"
	"os"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
	"github.com/monsterxx03/tachi/channel/manager"
	"github.com/monsterxx03/tachi/pkg/channel"
	"github.com/monsterxx03/tachi/pkg/shutil"
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

	// 1.5 Ignore system messages (e.g. thread created notifications,
	// pinned messages, etc.). Only process regular user messages.
	if m.Type != discordgo.MessageTypeDefault {
		return
	}

	// 2. Message deduplication.
	if ch.deduper.seen(m.ID) {
		ch.logger.Info(context.Background(), "discord: duplicate message skipped", "id", m.ID)
		return
	}

	// 3. Determine if this is a DM.
	dm := isDM(m.GuildID)

	// 4. Detect thread: resolve the parent channel for auth/filtering.
	// Threads have their own ChannelID, but allow/ignore/free-response
	// lists reference the parent channel.
	var isThread bool
	var parentChannelID string
	if !dm {
		parentChannelID, isThread = resolveThreadParent(s, m.ChannelID)
	}

	// 5. Channel-level filtering.
	// For threads, check against the parent channel; for regular
	// channels, use ChannelID directly.
	authChannelID := m.ChannelID
	if isThread && parentChannelID != "" {
		authChannelID = parentChannelID
	}
	if !ch.isAllowedChannel(authChannelID) {
		return
	}

	// 6. Determine directed status.
	directed := dm || isMentioned(m.Content, botUserID)

	// Free-response channels: all messages are treated as directed.
	// For threads, check the parent channel's free-response status.
	if !dm && ch.isFreeResponseChannel(authChannelID) {
		directed = true
	}

	// When require_mention is false, every message that passes the
	// filter below is intended to get a direct reply — treat it
	// as directed rather than ambient/whisper.
	if !dm {
		if isThread {
			// ThreadRequireMention controls mention requirement in threads.
			if !ch.cfg.ThreadRequireMention {
				directed = true
			}
		} else if !ch.cfg.RequireMention {
			directed = true
		}
	}

	// 7. Check mention strategy in guild channels.
	if !dm {
		// Determine which mention mode applies.
		mentionRequired := ch.cfg.RequireMention
		if isThread {
			mentionRequired = ch.cfg.ThreadRequireMention
		}

		if mentionRequired && !directed && !ch.isFreeResponseChannel(authChannelID) {
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

	// 8. Access control.
	roles := ch.resolveMemberRoles(m.GuildID, m.Author.ID)
	if !ch.isAuthorized(m.Author.ID, roles, dm) {
		ch.logger.Info(context.Background(), "discord: unauthorized user", "user", m.Author.ID, "name", m.Author.Username, "channel", m.ChannelID)
		return
	}

	// 9. Build the ThreadID.
	var threadID string
	if dm {
		threadID = threadIDForDM(m.Author.ID)
	} else {
		threadID = threadIDForGuild(m.GuildID, m.ChannelID)
	}

	// 10. Construct the IncomingMessage.
	incoming := ch.buildIncomingMessage(m, threadID, dm, directed, isThread)

	// 11. Start typing indicator and status embed only for directed
	// messages (ambient/whisper messages skip visible feedback).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var se *statusEmbedView
	if directed {
		stopTyping := ch.startTypingLoop(ctx, m.ChannelID)
		defer stopTyping()

		// 12. Set up streaming callback for real-time tool call progress.
		// The status embed is only sent when the first tool call is detected,
		// skipping the embed entirely for simple text-only replies.
		se = ch.newStatusEmbed(m.ChannelID)
		ctx = manager.WithStreamingCallback(ctx, se.cb)
	}

	// 12.5 Delegate to the manager handler.
	result := handler(ctx, incoming)

	// Collapse the status embed to a completion marker once the turn is
	// truly done (steer/buffered returns mean the turn is still running).
	if se != nil && !result.Steered && !result.Buffered && !result.Dropped {
		se.finish(result.Err == nil)
	}

	// 13. Process the result.
	ch.processHandlerResult(m, result, threadID, isThread)
}

// newStatusEmbed returns a status embed view that renders tool-call progress
// during a turn, with a short trailing text preview (NOT the accumulated
// full text — the final reply message is the single text delivery, so the
// embed must not duplicate it).
//
// cb is the StreamingCallback to attach to the turn context; finish collapses
// the embed to a completion marker once the turn ends (or aborts), so the
// embed never lingers showing the same content as the final reply.
//
// Concurrency: the callback and finish are invoked strictly sequentially on
// the same goroutine (the handler stack — finish runs after handler returns),
// so embedMsgID needs no locking between them. The lock inside only guards
// the preview buffers against nothing in practice; it is kept for safety if
// a future path moves the callback to another goroutine.
//
// Shared by the text-message path (handleMessageCreate) and the typed
// interaction path (handleSlashCommand) — the latter previously had no
// streaming at all, so /commit and /review issued via UI buttons showed no
// progress.
func (ch *DiscordChannel) newStatusEmbed(channelID string) *statusEmbedView {
	const maxEmbedDescRunes = 3800 // Discord limit is 4096, leave room
	const textTailCap = 400        // tail window of LLM text shown in the preview

	var (
		embedMsgID string
		toolBuf    strings.Builder
		textTail   []rune // capped tail window of accumulated LLM text
		toolCount  int
		mu         sync.Mutex
	)

	update := func(title, desc string, color int) {
		sess := ch.session
		if sess == nil {
			return
		}
		if embedMsgID == "" {
			sent, err := sess.ChannelMessageSendEmbed(channelID, &discordgo.MessageEmbed{
				Title: title, Description: desc, Color: color,
			})
			if err == nil {
				embedMsgID = sent.ID
			}
		} else {
			_, _ = sess.ChannelMessageEditEmbed(channelID, embedMsgID, &discordgo.MessageEmbed{
				Title: title, Description: desc, Color: color,
			})
		}
	}

	return &statusEmbedView{
		cb: func(event manager.StreamEvent) error {
			switch event.Type {
			case manager.StreamEventToolCall:
				mu.Lock()
				toolCount++
				toolBuf.WriteString("🔧 " + event.ToolName + formatToolArgsForEmbed(event.ToolName, event.ToolArgs) + "\n")
				// Short trailing preview only — full text arrives as the
				// final reply message.
				desc := buildStreamingDesc(string(textTail), toolBuf.String(), toolCount, maxEmbedDescRunes)
				mu.Unlock()
				update("🤖 Tachi", desc, 0x3498DB)
			case manager.StreamEventTextDelta:
				// Accumulate a capped tail window for the preview (full text
				// is delivered by the final reply; keeping the whole stream
				// would be wasted memory on long runs).
				mu.Lock()
				textTail = append(textTail, []rune(event.Text)...)
				if len(textTail) > textTailCap {
					textTail = textTail[len(textTail)-textTailCap:]
				}
				mu.Unlock()
			}
			return nil
		},
		finish: func(success bool) {
			mu.Lock()
			defer mu.Unlock()
			if embedMsgID == "" {
				return
			}
			sess := ch.session
			if sess == nil {
				return
			}
			title := "✅ Tachi — 已完成"
			color := 0x2ECC71
			if !success {
				title = "❌ Tachi — 已中止"
				color = 0xE74C3C
			}
			// Collapse to a completion marker + tool-call record. The text is
			// deliberately dropped — the final reply carries it. The tool
			// block is truncated to the embed limit so a long run's record
			// can never silently exceed Discord's 4096-char description cap
			// (which would fail the edit and leave the embed stuck mid-run).
			desc := ""
			if toolBuf.Len() > 0 {
				tools := toolBuf.String()
				if utf8.RuneCountInString(tools) > maxEmbedDescRunes-7 {
					tools = truncateStreamingTools(tools, toolCount, maxEmbedDescRunes-7)
				}
				desc = "```\n" + tools + "```"
			}
			if _, err := sess.ChannelMessageEditEmbed(channelID, embedMsgID, &discordgo.MessageEmbed{
				Title: title, Description: desc, Color: color,
			}); err != nil {
				ch.logger.Error(context.Background(), "discord: status embed finish edit failed", err)
			}
		},
	}
}

// statusEmbedView bundles the streaming callback and the completion hook for
// a status embed (see newStatusEmbed).
type statusEmbedView struct {
	cb     manager.StreamingCallback
	finish func(success bool)
}

// buildIncomingMessage constructs a channel.IncomingMessage from a Discord
// MessageCreate event.
func (ch *DiscordChannel) buildIncomingMessage(m *discordgo.MessageCreate, threadID string, dm, directed, isThread bool) channel.IncomingMessage {
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

	// Detect Discord reply (message_reference) and inject context.
	var referencedMessageID string
	if m.MessageReference != nil && m.MessageReference.MessageID != "" {
		referencedMessageID = m.MessageReference.MessageID
		if m.ReferencedMessage != nil {
			// Discord provides the referenced message content when available.
			refAuthor := resolveSenderName(m.ReferencedMessage.Author)
			refContent := cleanContentForLLM(m.ReferencedMessage.Content)
			if refAuthor != "" && refAuthor != "unknown" {
				content = "[回复 @" + refAuthor + ": " + refContent + "]\n" + content
			} else {
				content = "[回复消息: " + refContent + "]\n" + content
			}
		} else {
			// ReferencedMessage not provided (uncached or deleted) — just note the fact.
			content = "[回复了一条消息]\n" + content
		}
	}

	// If the message is in a thread, inject the thread context as
	// context so the LLM knows what the thread is about. This includes
	// the starter message (the original message the thread was created
	// from) plus any initial messages sent during thread creation.
	// Only injected on the first message in a thread session.
	if isThread {
		ch.threadStarterInjectedMu.Lock()
		alreadyInjected := ch.threadStarterInjected[m.ChannelID]
		ch.threadStarterInjectedMu.Unlock()

		if !alreadyInjected {
			if ctx := ch.getThreadContext(m.ChannelID, m.ID); ctx != "" {
				content = "[子区上下文]\n" + ctx + "\n[/子区上下文]\n" + content

				ch.threadStarterInjectedMu.Lock()
				ch.threadStarterInjected[m.ChannelID] = true
				ch.threadStarterInjectedMu.Unlock()
			}
		}
	}

	msg := channel.IncomingMessage{
		ThreadID:            threadID,
		MessageID:           m.ID,
		Content:             content,
		ChannelID:           m.ChannelID,
		Sender:              senderName,
		Directed:            directed,
		GroupChat:           !dm,
		ReferencedMessageID: referencedMessageID,
	}

	// Handle attachments.
	if len(m.Attachments) > 0 {
		for _, att := range m.Attachments {
			downloaded, err := ch.downloadAttachment(att.URL)
			if err != nil {
				ch.logger.Error(context.Background(), "discord: download attachment failed", err, "file", att.Filename)
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
func (ch *DiscordChannel) processHandlerResult(m *discordgo.MessageCreate, result channel.HandlerResult, threadID string, isThread bool) {
	channelID := m.ChannelID

	if result.Steered {
		return
	}

	if result.Dropped {
		return
	}

	if result.Buffered {
		return
	}

	if result.Streamed {
		return
	}

	if result.Err != nil {
		ch.logger.Error(context.Background(), "discord: handler error", result.Err)
		return
	}

	// Send the reply.
	reply := result.Reply
	if reply.Content == "" {
		return
	}

	// Build a MessageReference so the response shows as a Discord reply
	// to the user's message (unless the incoming message has no ID or is
	// a DM — DMs don't benefit from reply decoration).
	var ref *discordgo.MessageReference
	if m.ID != "" && !isDM(m.GuildID) {
		ref = &discordgo.MessageReference{
			MessageID: m.ID,
			ChannelID: channelID,
			GuildID:   m.GuildID,
		}
	}

	// 1. Check for EMBED prefix.
	if cleaned, embed, ok := parseEmbedContent(reply.Content); ok {
		if err := ch.sendEmbed(channelID, embed); err != nil {
			ch.logger.Error(context.Background(), "discord: send embed error", err)
		}
		// Send remaining text after embed, with MEDIA parsing.
		if cleaned != "" {
			if _, err := ch.sendTextWithMediaRef(channelID, cleaned, ref); err != nil {
				// If Discord rejects the reply (e.g. system message),
				// retry without MessageReference.
				if isSystemMessageReplyError(err) {
					ch.logger.Info(context.Background(), "discord: retrying embed text without reply reference")
					_, _ = ch.sendTextWithMediaRef(channelID, cleaned, nil)
				} else {
					ch.logger.Error(context.Background(), "discord: send embed text error", err)
				}
			}
		}
	} else {
		// 2. Normal text with MEDIA tag support.
		if _, err := ch.sendTextWithMediaRef(channelID, reply.Content, ref); err != nil {
			// If Discord rejects the reply (e.g. system message),
			// retry without MessageReference.
			if isSystemMessageReplyError(err) {
				ch.logger.Info(context.Background(), "discord: retrying reply without reference")
				_, _ = ch.sendTextWithMediaRef(channelID, reply.Content, nil)
			} else {
				ch.logger.Error(context.Background(), "discord: send reply error", err)
			}
		}
	}

	// 3. Send any explicit attachments from the reply (besides MEDIA ones).
	for _, att := range reply.Attachments {
		data, err := channel.ResolveAttachmentData(att)
		if err != nil {
			ch.logger.Error(context.Background(), "discord: send attachment resolve failed", err, "file", att.FileName)
			continue
		}
		if _, err := ch.session.ChannelFileSend(channelID, att.FileName, bytes.NewReader(data)); err != nil {
			ch.logger.Error(context.Background(), "discord: send attachment error", err, "file", att.FileName)
		}
	}

	// 4. Update channel topic with working directory and git branch.
	// Only for guild channels (DM channels don't have a meaningful topic).
	// Threads don't have a topic field, so skip them too.
	if !isDM(m.GuildID) && !isThread {
		ch.updateChannelTopic(channelID, result.WorkDir, result.Model)
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

// updateChannelTopic retrieves the current working directory, git branch,
// and model name, then updates the given channel's topic if anything has
// changed since the last update. Skips if the channel is a DM.
// workDir is the per-thread working directory passed from the manager.
func (ch *DiscordChannel) updateChannelTopic(channelID, workDir, model string) {
	dir := workDir
	if dir == "" {
		// Fallback: use the process CWD if no per-thread workDir is known.
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return
		}
	}

	// Fast path: if the directory and model haven't changed since the last
	// update, skip entirely — the topic content is effectively the same.
	// The git branch is secondary info; changes via external checkout are
	// rare and this avoids an unnecessary shell out on every reply.
	ch.topicStatusMu.Lock()
	last, seen := ch.topicStatus[channelID]
	ch.topicStatusMu.Unlock()
	if seen && last.dir == dir && last.model == model {
		return
	}

	branch := ""
	if b, err := shutil.Output(context.Background(), dir, "git", "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		branch = b
	}

	// Build the new topic text (Discord channel topic does not support
	// markdown formatting, so plain text only).
	topic := dir
	if branch != "" {
		topic += " [" + branch + "]"
	}
	if model != "" {
		topic += " | " + model
	}

	sess := ch.session
	if sess == nil {
		return
	}

	// Full cache check with branch and model — only call the API when
	// something actually changed (avoids rate limit issues on repeated replies).
	ch.topicStatusMu.Lock()
	last, seen = ch.topicStatus[channelID]
	if seen && last.dir == dir && last.branch == branch && last.model == model {
		ch.topicStatusMu.Unlock()
		return
	}
	ch.topicStatusMu.Unlock()

	if _, err := sess.ChannelEdit(channelID, &discordgo.ChannelEdit{
		Topic: topic,
	}); err != nil {
		ch.logger.Error(context.Background(), "discord: update channel topic failed", err, "channel", channelID)
		return
	}

	// Only cache the new status once the API call succeeded, so a transient
	// failure (rate limit, permission) doesn't permanently block updates.
	ch.topicStatusMu.Lock()
	ch.topicStatus[channelID] = topicEntry{dir: dir, branch: branch, model: model}
	ch.topicStatusMu.Unlock()
}

// isSystemMessageReplyError checks if a Discord API error is the
// "Cannot reply to a system message" error, which means we should
// retry without a MessageReference.
func isSystemMessageReplyError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "REPLIES_CANNOT_REPLY_TO_SYSTEM_MESSAGE")
}
