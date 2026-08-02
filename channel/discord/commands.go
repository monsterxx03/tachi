package discord

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/channel/manager"
	"github.com/monsterxx03/tachi/pkg/channel"
)

var commandOptions = map[string][]*discordgo.ApplicationCommandOption{
	"model": {
		{
			Type:         discordgo.ApplicationCommandOptionString,
			Name:         "provider",
			Description:  "Provider name to switch to (leave empty to list available models)",
			Required:     false,
			Autocomplete: true,
		},
	},
	"thinking": {
		{
			Type:         discordgo.ApplicationCommandOptionString,
			Name:         "level",
			Description:  "Thinking level: none | low | medium | high | xhigh | max | default",
			Required:     false,
			Autocomplete: true,
		},
	},
	"mcp": {
		{
			Type:        discordgo.ApplicationCommandOptionString,
			Name:        "action",
			Description: "list, toggle, reconnect, or auth <name>",
			Required:    false,
		},
	},
	"skill": {
		{
			Type:        discordgo.ApplicationCommandOptionString,
			Name:        "name",
			Description: "list, reload, or skill name to activate",
			Required:    false,
		},
	},
	"cd": {
		{
			Type:        discordgo.ApplicationCommandOptionString,
			Name:        "directory",
			Description: "Directory path to switch to",
			Required:    true,
		},
	},
	"review": {
		{
			Type:        discordgo.ApplicationCommandOptionInteger,
			Name:        "rounds",
			Description: "Adversarial review rounds (2-10); omit for single-round review",
			Required:    false,
			MinValue:    new(2.0),
			MaxValue:    10,
		},
	},
}

// registerSlashCommands registers Discord Application Commands from the
// commands registry. Uses DevGuildID for instant registration when set.
func (ch *DiscordChannel) registerSlashCommands(sess *discordgo.Session) error {
	cmds := commands.ForMode(commands.ModeChannel)
	if len(cmds) == 0 {
		return nil
	}

	appID := ch.cfg.ApplicationID
	if appID == "" {
		ch.mu.RLock()
		if ch.botUserID != "" {
			appID = ch.botUserID
		}
		ch.mu.RUnlock()
	}
	if appID == "" {
		return fmt.Errorf("discord: cannot register slash commands: ApplicationID unknown")
	}

	discordCmds := make([]*discordgo.ApplicationCommand, 0, len(cmds))
	for _, cmd := range cmds {
		ac := &discordgo.ApplicationCommand{
			Name:        cmd.Name,
			Description: cmd.Description,
		}
		// Add options for commands that accept arguments.
		if opts, ok := commandOptions[cmd.Name]; ok {
			ac.Options = opts
		}
		discordCmds = append(discordCmds, ac)
	}

	// Guild-level commands (instant) or global commands (cached up to 1h).
	guildID := ch.cfg.DevGuildID
	registered, err := sess.ApplicationCommandBulkOverwrite(appID, guildID, discordCmds)
	if err != nil {
		return fmt.Errorf("discord: register slash commands: %w", err)
	}

	loc := "global"
	if guildID != "" {
		loc = "guild:" + guildID
	}
	ch.logger.Info(context.Background(), "discord: registered slash commands", "count", len(registered), "scope", loc)
	return nil
}

// Run implements channel.Channel. It connects to the Discord Gateway and
// enters the message processing loop.
func (ch *DiscordChannel) handleSlashCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()
	if data.Name == "" {
		return
	}

	ch.logger.Info(context.Background(), "discord: slash command", "cmd", data.Name, "options", len(data.Options))

	cmdHandler := ch.cmdHandler
	if cmdHandler == nil {
		// No deferred ack sent yet — respond directly.
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ Slash command handler not initialized",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	// Build args from command options.
	args := buildSlashArgs(data.Options)
	ch.logger.Info(context.Background(), "discord: slash command args", "cmd", data.Name, "args", args)

	// Determine thread ID from the interaction context.
	threadID := ""
	if i.GuildID != "" {
		threadID = threadIDForGuild(i.GuildID, i.ChannelID)
	} else {
		threadID = threadIDForDM(i.User.ID)
	}

	// Acknowledge the interaction immediately so long-running commands
	// don't time out. Failures here are non-fatal — the followup below will
	// fail loudly if Discord rejects it.
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	}); err != nil {
		ch.logger.Error(context.Background(), "discord: defer slash command response", err)
	}

	// Wire the streaming callback so long-running commands (/commit,
	// /review) show live tool-call progress in a status embed — same UX as
	// the text-message path. The embed collapses to a completion marker when
	// the command returns, so it never duplicates the final reply.
	se := ch.newStatusEmbed(i.ChannelID)
	cmdCtx := manager.WithStreamingCallback(context.Background(), se.cb)

	// Execute the command via the Manager's CommandHandler.
	reply, workDir, model, err := cmdHandler(cmdCtx, channel.SlashCommand{
		Name:     data.Name,
		ThreadID: threadID,
		Args:     args,
	})
	se.finish(err == nil)
	if err != nil {
		// reply.Content holds the handler's status output (single-round
		// review text, or multi-round status + report dir — per-round text
		// was already pushed to the thread as it completed). Keep it and
		// append the error exactly once.
		content := "❌ " + err.Error()
		if errors.Is(err, context.Canceled) {
			// User-initiated /stop — the stop reply already acknowledged it.
			// Keep the status summary (report dir etc.) if any.
			content = "⏹️ 已取消。"
			if reply.Content != "" {
				content = reply.Content + "\n\n" + content
			}
		} else if reply.Content != "" {
			content = reply.Content + "\n\n❌ " + err.Error()
		}
		ch.respondInteraction(s, i, content)
		return
	}

	// Send text reply as ephemeral interaction followup.
	textContent := reply.Content
	if textContent == "" {
		textContent = "✅ Done"
	}
	ch.respondInteraction(s, i, textContent)

	// Send file attachments (e.g., /transcript HTML) to the channel.
	// Interaction responses cannot include files, so we send them as
	// a followup message after the initial acknowledgement.
	for _, att := range reply.Attachments {
		data, attErr := channel.ResolveAttachmentData(att)
		if attErr != nil {
			ch.logger.Error(context.Background(), "discord: slash command resolve attachment", attErr, "file", att.FileName)
			continue
		}
		if _, sendErr := s.ChannelFileSend(i.ChannelID, att.FileName, bytes.NewReader(data)); sendErr != nil {
			ch.logger.Error(context.Background(), "discord: slash command send attachment", sendErr, "file", att.FileName)
		}
	}

	// Update channel topic with the current working directory and model.
	// Skip for threads (they don't have a topic field) and DMs.
	if workDir != "" && !isDM(i.GuildID) {
		_, isThread := resolveThreadParent(s, i.ChannelID)
		if !isThread {
			ch.updateChannelTopic(i.ChannelID, workDir, model)
		}
	}
}

// handleAutocomplete processes an AUTOCOMPLETE interaction.
// Returns suggestions for the /model provider option or the /thinking
// level option, depending on the command being typed.
func (ch *DiscordChannel) handleAutocomplete(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()
	if len(data.Options) == 0 {
		ch.respondAutocompleteEmpty(s, i)
		return
	}

	// Get user's current input for prefix filtering (empty = show all).
	prefix := strings.ToLower(data.Options[0].StringValue())

	var choices []*discordgo.ApplicationCommandOptionChoice
	switch data.Name {
	case "model":
		for _, name := range ch.providerNames {
			if name == "" {
				continue
			}
			if prefix != "" && !strings.HasPrefix(strings.ToLower(name), prefix) {
				continue
			}
			choices = append(choices, &discordgo.ApplicationCommandOptionChoice{
				Name:  name,
				Value: name,
			})
		}
	case "thinking":
		for _, level := range ch.thinkingLevels {
			if level == "" {
				continue
			}
			if prefix != "" && !strings.HasPrefix(strings.ToLower(level), prefix) {
				continue
			}
			choices = append(choices, &discordgo.ApplicationCommandOptionChoice{
				Name:  level,
				Value: level,
			})
		}
	}
	if choices == nil {
		choices = []*discordgo.ApplicationCommandOptionChoice{}
	}

	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionApplicationCommandAutocompleteResult,
		Data: &discordgo.InteractionResponseData{Choices: choices},
	})
}

// respondAutocompleteEmpty replies to an autocomplete interaction with no
// choices (e.g. the command has no options populated).
func (ch *DiscordChannel) respondAutocompleteEmpty(s *discordgo.Session, i *discordgo.InteractionCreate) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionApplicationCommandAutocompleteResult,
		Data: &discordgo.InteractionResponseData{Choices: []*discordgo.ApplicationCommandOptionChoice{}},
	})
}

// buildSlashArgs converts discordgo ApplicationCommandInteractionDataOption
// into a space-separated argument string. Integer options (e.g. /review
// rounds) are rendered as their decimal value; number and boolean options
// likewise; string options as-is.
func buildSlashArgs(options []*discordgo.ApplicationCommandInteractionDataOption) string {
	var parts []string
	for _, opt := range options {
		switch opt.Type {
		case discordgo.ApplicationCommandOptionInteger:
			parts = append(parts, strconv.FormatInt(opt.IntValue(), 10))
		case discordgo.ApplicationCommandOptionNumber:
			parts = append(parts, strconv.FormatFloat(opt.FloatValue(), 'f', -1, 64))
		case discordgo.ApplicationCommandOptionBoolean:
			parts = append(parts, strconv.FormatBool(opt.BoolValue()))
		default:
			if opt.StringValue() != "" {
				parts = append(parts, opt.StringValue())
			}
		}
	}
	return strings.Join(parts, " ")
}

// respondInteraction delivers the final result of a slash command as an
// ephemeral followup (the initial deferred acknowledgment was already sent
// in handleSlashCommand). Long content is split into ≤2000-char chunks —
// Discord rejects messages over that limit regardless of channel type.
//
// Followup failure degrades to a regular channel message: the interaction
// token expires ~15 minutes after the initial ack, and long commands
// (/review N, /commit) can exceed that. Without the fallback the final
// result would be silently lost (the user only ever sees progress messages).
func (ch *DiscordChannel) respondInteraction(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	content = strings.TrimSpace(content)
	if content == "" {
		return
	}
	chunks := splitMessage(content)
	for idx, chunk := range chunks {
		if _, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
			Content: chunk,
			Flags:   discordgo.MessageFlagsEphemeral,
		}); err != nil {
			ch.logger.Error(context.Background(), "discord: slash command followup failed, degrading to channel message", err, "content_len", len(chunk))
			// Send the remaining chunks (including this one) as a regular
			// channel message — no interaction time limit applies.
			remaining := strings.Join(chunks[idx:], "")
			if err := ch.sendText(i.ChannelID, remaining); err != nil {
				ch.logger.Error(context.Background(), "discord: degraded channel message send failed", err, "content_len", len(remaining))
			}
			return
		}
	}
}

// sendGreeting sends the startup greeting as a DM to the first allowed user.
