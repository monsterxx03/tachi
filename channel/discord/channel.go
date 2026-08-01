package discord

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"
	"github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/channel/manager"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/channel"
	"github.com/monsterxx03/tachi/pkg/logger"
	tachiproxy "github.com/monsterxx03/tachi/pkg/proxy"
	"gopkg.in/yaml.v3"
)

func init() {
	channel.Register("discord", func(rawCfg map[string]any) (channel.Channel, error) {
		b, err := yaml.Marshal(rawCfg)
		if err != nil {
			return nil, fmt.Errorf("discord: marshal config: %w", err)
		}
		// Start from documented defaults, then overlay user config.
		// yaml.Unmarshal only overrides fields present in the YAML,
		// so unset fields keep their default values.
		cfg := newDefaultConfig()
		if err := yaml.Unmarshal(b, &cfg); err != nil {
			return nil, fmt.Errorf("discord: unmarshal config: %w", err)
		}
		// Resolve token from env reference if needed, then override.
		token, err := tokenFromConfig(rawCfg)
		if err != nil {
			return nil, err
		}
		cfg.Token = token
		return NewChannel(cfg)
	})
}

const (
	// dedupTTL is how long we keep a message ID in the dedup set.
	dedupTTL = 5 * time.Minute
	// dedupMaxSize is the maximum number of entries in the dedup set.
	dedupMaxSize = 1000
)

// messageDeduper prevents processing the same message ID twice.
// Discord's Gateway may re-deliver MESSAGE_CREATE events during resume.
type messageDeduper struct {
	mu      sync.Mutex
	entries map[string]time.Time // messageID → addedAt
}

func newMessageDeduper() *messageDeduper {
	return &messageDeduper{entries: make(map[string]time.Time)}
}

// seen returns true if the message ID was already processed.
// If not seen, it records the ID and returns false.
func (d *messageDeduper) seen(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	if _, exists := d.entries[id]; exists {
		return true
	}

	// Enforce max size by evicting all expired entries when at capacity.
	if len(d.entries) >= dedupMaxSize {
		now := time.Now()
		for k, v := range d.entries {
			if now.After(v) {
				delete(d.entries, k)
			}
		}
	}

	d.entries[id] = time.Now().Add(dedupTTL)
	return false
}

// DiscordChannel implements the channel.Channel interface for Discord.
type DiscordChannel struct {
	cfg     DiscordConfig
	session *discordgo.Session
	logger  *logger.Logger

	// httpClient is used for CDN downloads and other HTTP requests.
	// Configured with proxy support when cfg.Proxy is set.
	httpClient *http.Client

	// Runtime state
	mu        sync.RWMutex
	botUserID string // Bot's own UserID (for @mention detection)

	memberCache *memberCache // Guild member info cache
	deduper     *messageDeduper

	// Store the message handler for use by thread creation and other
	// secondary event handlers that need to simulate message processing.
	messageHandler channel.MessageHandler

	// Component callback registry (Phase 2+)
	componentHandlers map[string]componentHandler

	// Slash command handler (injected by Manager via CommandChannel interface)
	cmdHandler channel.CommandHandler

	// Provider names for slash command autocomplete (injected by Manager)
	providerNames []string

	// /thinking level values for slash command autocomplete (injected by Manager)
	thinkingLevels []string

	// Attachment cache directory
	cacheDir string

	// Per-channel topic status tracking — avoids unnecessary API calls
	// when directory and git branch haven't changed.
	topicStatus   map[string]topicEntry // channelID → last topic info
	topicStatusMu sync.Mutex

	// Thread starter message cache — maps thread channel ID to the
	// starter message content. Fetched once per thread, then cached
	// for the lifetime of the bot session.
	threadStarterCache   map[string]string // threadID → starter text
	threadStarterCacheMu sync.Mutex

	// Tracks which threads have already had their starter message injected
	// into the session context, to avoid repeating it on every message.
	threadStarterInjected   map[string]bool // threadID → injected
	threadStarterInjectedMu sync.Mutex
}

// topicEntry holds the last known directory, git branch, and model for a channel topic.
type topicEntry struct {
	dir    string
	branch string
	model  string
}

// componentHandler is a callback for interactive component interactions (Phase 2+).
type componentHandler func(customID string, interaction *discordgo.Interaction) error

// NewChannel creates a Discord channel. It sets defaults, resolves the token,
// and prepares the cache directory.
func NewChannel(cfg DiscordConfig) (*DiscordChannel, error) {
	// Apply defaults for zero-value fields.
	dc := cfg
	if dc.Token == "" {
		return nil, fmt.Errorf("discord: token is required")
	}
	dc.setDefaults()

	cacheDir := filepath.Join(config.BaseDir(), "discord", "cache")

	// Initialize HTTP client with optional proxy.
	httpClient, err := tachiproxy.NewHTTPClient(dc.Proxy, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("discord: create HTTP client: %w", err)
	}

	return &DiscordChannel{
		cfg:                   dc,
		logger:                logger.New("channel.discord"),
		httpClient:            httpClient,
		cacheDir:              cacheDir,
		componentHandlers:     make(map[string]componentHandler),
		memberCache:           newMemberCache(),
		deduper:               newMessageDeduper(),
		topicStatus:           make(map[string]topicEntry),
		threadStarterCache:    make(map[string]string),
		threadStarterInjected: make(map[string]bool),
	}, nil
}

// Name returns the channel type identifier.
func (ch *DiscordChannel) Name() string { return "discord" }

// SetCommandHandler implements channel.CommandChannel.
// Called by Manager before Run() to inject the programmatic slash command handler.
func (ch *DiscordChannel) SetCommandHandler(handler channel.CommandHandler) {
	ch.cmdHandler = handler
}

// SetProviderNames implements channel.Autocompleter.
// Called by Manager before Run() to inject available provider names for autocomplete.
func (ch *DiscordChannel) SetProviderNames(names []string) {
	ch.providerNames = names
}

// SetThinkingLevels implements channel.Autocompleter.
// Called by Manager before Run() to inject the valid /thinking level values
// for autocomplete.
func (ch *DiscordChannel) SetThinkingLevels(levels []string) {
	ch.thinkingLevels = levels
}

// SystemPromptSuffix implements channel.SystemPromptSuffixer.
// Tells the agent it's currently operating as a Discord bot.
func (ch *DiscordChannel) SystemPromptSuffix() string {
	return `## Current Channel: Discord

You are currently operating as a Discord bot. Your responses are delivered
through Discord guild channels, direct messages, and threads.

Platform characteristics:
- Discord markdown is supported (bold, italic, lists, headers, quotes,
  inline code and code blocks), but NOT tables — Discord clients never
  render markdown tables. Any table you output is automatically converted
  to an aligned monospace code block, so keep tables narrow (2-4 columns);
  prefer bullet lists for wide or long data
- Messages are limited to 2000 characters; longer responses are
  automatically split into multiple messages
- Media attachments (images, files) are supported as separate uploads
- Threads are fully supported: the bot can receive and reply inside
  Discord threads without @mention by default (configurable via
  thread_require_mention)`
}

// Send implements channel.MessageSender for proactive message delivery
// (cron notifications, etc.). It sends text content and any attachments
// to the channel specified by ThreadID.
func (ch *DiscordChannel) Send(ctx context.Context, msg channel.OutgoingMessage) error {
	sess := ch.session
	if sess == nil {
		return fmt.Errorf("discord: session not initialized")
	}

	// Extract Discord channel ID from ThreadID (format: "guild:gid:channel:cid" or "dm:cid").
	channelID := channelIDFromThreadID(msg.ThreadID)
	if channelID == "" {
		return fmt.Errorf("discord: Send: invalid ThreadID %q", msg.ThreadID)
	}

	// Send text content if present.
	if msg.Content != "" {
		if err := ch.sendText(channelID, msg.Content); err != nil {
			return fmt.Errorf("discord: Send text: %w", err)
		}
	}

	// Send each attachment.
	for _, att := range msg.Attachments {
		data, err := channel.ResolveAttachmentData(att)
		if err != nil {
			ch.logger.Error(ctx, "discord: Send resolve attachment", err, "file", att.FileName)
			continue
		}
		if _, err := sess.ChannelFileSend(channelID, att.FileName, bytes.NewReader(data)); err != nil {
			ch.logger.Error(ctx, "discord: Send attachment error", err, "file", att.FileName)
		}
	}

	return nil
}

// OnStart implements channel.Channel. It creates the cache directory,
// ensures the state dir exists, and registers slash commands.
func (ch *DiscordChannel) OnStart(ctx context.Context) error {
	// Create attachment cache directory.
	if err := os.MkdirAll(ch.cacheDir, 0755); err != nil {
		return fmt.Errorf("discord: create cache dir: %w", err)
	}

	ch.logger.Info(ctx, "discord: cache dir ready", "path", ch.cacheDir)
	return nil
}

// commandOptions defines Discord slash command options for commands that accept arguments.
// Keyed by command name, mapped to Discord option definitions.
// Options are registered alongside the command in registerSlashCommands.
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
func (ch *DiscordChannel) Run(ctx context.Context, handler channel.MessageHandler) error {
	// Resolve token directly from config (already resolved in factory).
	sess, err := discordgo.New("Bot " + ch.cfg.Token)
	if err != nil {
		return fmt.Errorf("discord: create session: %w", err)
	}
	ch.session = sess
	ch.messageHandler = handler

	// Apply proxy configuration if set.
	if err := ch.applyProxy(ctx, sess); err != nil {
		return fmt.Errorf("discord: proxy config: %w", err)
	}

	// Register event handlers.
	// AddHandler returns a func() that removes the handler when called.
	// We store these to prevent duplicate registration if Run() is called
	// again (see design doc §2.3).
	removeReady := sess.AddHandler(ch.onReady())
	removeMessage := sess.AddHandler(ch.onMessageCreate(handler))
	removeInteraction := sess.AddHandler(ch.onInteractionCreate(handler))

	// Also register GUILD_CREATE for member cache warmup,
	// GUILD_MEMBER_UPDATE for incremental cache updates, and
	// THREAD_CREATE to capture the initial message when a thread
	// is created with a message (Discord doesn't send MESSAGE_CREATE
	// for the initial message typed in the thread creation dialog).
	removeGuildCreate := sess.AddHandler(ch.onGuildCreate())
	removeGuildMemberUpdate := sess.AddHandler(ch.onGuildMemberUpdate())
	removeThreadCreate := sess.AddHandler(ch.onThreadCreate())

	// Configure Intents.
	// IntentsGuilds: needed for GUILD_CREATE member cache warmup,
	// and THREAD_CREATE to capture initial thread messages.
	// IntentsGuildMessages: receive guild channel messages.
	// IntentsDirectMessages: receive DMs.
	// IntentsMessageContent: privileged intent — read message content.
	sess.Identify.Intents = discordgo.IntentsGuilds |
		discordgo.IntentsGuildMessages |
		discordgo.IntentsDirectMessages |
		discordgo.IntentsMessageContent

	// Open Gateway connection.
	if err := sess.Open(); err != nil {
		return fmt.Errorf("discord: gateway open: %w", err)
	}
	defer sess.Close()

	ch.logger.Info(ctx, "discord: connected to gateway")

	// Send greeting if configured.
	ch.sendGreeting()

	// Wait for context cancellation.
	<-ctx.Done()
	ch.logger.Info(ctx, "discord: context cancelled, shutting down")

	// Remove handlers to prevent re-registration on future Run() calls.
	removeReady()
	removeMessage()
	removeInteraction()
	removeGuildCreate()
	removeGuildMemberUpdate()
	removeThreadCreate()

	return nil
}

// onReady returns a handler for the READY event, capturing the bot's user ID
// and registering slash commands.
func (ch *DiscordChannel) onReady() any {
	return func(s *discordgo.Session, r *discordgo.Ready) {
		ch.mu.Lock()
		ch.botUserID = r.User.ID
		ch.mu.Unlock()
		ch.logger.Info(context.Background(), "discord: ready", "bot_user_id", r.User.ID)

		// Register slash commands now that the ApplicationID is known.
		if err := ch.registerSlashCommands(s); err != nil {
			ch.logger.Error(context.Background(), "discord: register slash commands", err)
		}
	}
}

// onGuildCreate returns a handler for GUILD_CREATE events.
// Used for member cache warmup on first connect.
func (ch *DiscordChannel) onGuildCreate() any {
	return func(s *discordgo.Session, g *discordgo.GuildCreate) {
		ch.memberCache.warmupFromGuildCreate(s, g)
	}
}

// onGuildMemberUpdate returns a handler for GUILD_MEMBER_UPDATE events.
// Used for incremental member cache updates.
func (ch *DiscordChannel) onGuildMemberUpdate() any {
	return func(s *discordgo.Session, m *discordgo.GuildMemberUpdate) {
		ch.memberCache.handleGuildMemberUpdate(s, m)
	}
}

// onThreadCreate returns a handler for THREAD_CREATE events.
// When a thread is newly created WITH a message (user typed in the
// creation dialog), Discord does NOT send a MESSAGE_CREATE for that
// initial message. We use the thread's LastMessageID to fetch and
// process it directly.
func (ch *DiscordChannel) onThreadCreate() any {
	return func(s *discordgo.Session, t *discordgo.ThreadCreate) {
		if !t.NewlyCreated || t.Channel == nil {
			return
		}
		if !t.IsThread() {
			return
		}

		handler := ch.messageHandler
		if handler == nil {
			return
		}

		// If the thread has no last message, try fetching via
		// ChannelMessages as a fallback (Discord may send
		// THREAD_CREATE before the initial message is persisted).
		if t.LastMessageID == "" {
			msgs, err := s.ChannelMessages(t.ID, 5, "", "", "")
			if err != nil || len(msgs) == 0 {
				return
			}
			// Messages are newest-first; iterate backwards to find the
			// oldest user message (the initial typed message).
			for i := len(msgs) - 1; i >= 0; i-- {
				msg := msgs[i]
				if msg.Type != discordgo.MessageTypeDefault {
					continue
				}
				if msg.Author == nil || msg.Author.Bot {
					continue
				}
				synthetic := &discordgo.MessageCreate{Message: msg}
				ch.handleMessageCreate(s, synthetic, handler)
				return
			}
			return
		}

		// Fetch the (most recent) message in the thread using its
		// LastMessageID. For a newly created thread with a typed
		// message, this is the user's initial message (type 0).
		// For a thread created without typing, it's the starter
		// (type 21) and we skip it.
		msg, err := s.ChannelMessage(t.ID, t.LastMessageID)
		if err != nil || msg == nil {
			return
		}

		// Skip starter messages (type 21) and bot/system messages.
		if msg.Type != discordgo.MessageTypeDefault {
			return
		}
		if msg.Author == nil || msg.Author.Bot {
			return
		}

		// Construct a synthetic MessageCreate and process it
		// through the normal pipeline. The deduper inside
		// handleMessageCreate handles MESSAGE_CREATE dedup.
		synthetic := &discordgo.MessageCreate{
			Message: msg,
		}

		ch.handleMessageCreate(s, synthetic, handler)
	}
}

// onMessageCreate returns a handler for MESSAGE_CREATE events.
// Delegates to handler.go's handleMessageCreate for the full processing pipeline.
// The recover guard is essential: discordgo spawns event handlers in bare
// goroutines (SyncEvents=false), so an unrecovered panic would kill the bot.
func (ch *DiscordChannel) onMessageCreate(handler channel.MessageHandler) any {
	return func(s *discordgo.Session, m *discordgo.MessageCreate) {
		defer func() {
			if r := recover(); r != nil {
				ch.logger.Error(context.Background(), "discord: panic in message handler", fmt.Errorf("%v", r))
			}
		}()
		ch.handleMessageCreate(s, m, handler)
	}
}

// onInteractionCreate returns a handler for INTERACTION_CREATE events.
// Handles APPLICATION_COMMAND (slash commands) and AUTOCOMPLETE interactions.
// See onMessageCreate for why the recover guard is required.
func (ch *DiscordChannel) onInteractionCreate(handler channel.MessageHandler) any {
	return func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		defer func() {
			if r := recover(); r != nil {
				ch.logger.Error(context.Background(), "discord: panic in interaction handler", fmt.Errorf("%v", r))
				// Best-effort fallback reply — may fail if the interaction
				// was already acknowledged.
				_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseChannelMessageWithSource,
					Data: &discordgo.InteractionResponseData{
						Content: "❌ 内部错误，请重试。",
						Flags:   discordgo.MessageFlagsEphemeral,
					},
				})
			}
		}()
		switch i.Type {
		case discordgo.InteractionApplicationCommand:
			ch.handleSlashCommand(s, i)
		case discordgo.InteractionApplicationCommandAutocomplete:
			ch.handleAutocomplete(s, i)
		}
	}
}

// handleSlashCommand processes an APPLICATION_COMMAND interaction.
//
// The interaction is acknowledged immediately with a deferred response —
// commands can take far longer than Discord's 3s interaction window (e.g.
// /review N with multiple adversarial rounds, /commit running git, deep
// research). The final result is delivered afterwards via followup.
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
func (ch *DiscordChannel) sendGreeting() {
	if ch.cfg.Greeting == "" {
		return
	}
	if len(ch.cfg.AllowedUsers) == 0 {
		ch.logger.Warn(context.Background(), "discord: greeting configured but no allowed_users to send to")
		return
	}
	sess := ch.session
	if sess == nil {
		return
	}
	// Create/open a DM channel with the first allowed user.
	dmChannel, err := sess.UserChannelCreate(ch.cfg.AllowedUsers[0])
	if err != nil {
		ch.logger.Error(context.Background(), "discord: create DM channel for greeting", err)
		return
	}
	if _, err := sess.ChannelMessageSend(dmChannel.ID, ch.cfg.Greeting); err != nil {
		ch.logger.Error(context.Background(), "discord: send greeting error", err)
	}
}

// applyProxy configures the discordgo session to use a proxy for both
// REST API calls (sess.Client) and Gateway WebSocket connection (sess.Dialer).
// It is a no-op when cfg.Proxy is empty.
func (ch *DiscordChannel) applyProxy(ctx context.Context, sess *discordgo.Session) error {
	proxyURL := ch.cfg.Proxy
	if proxyURL == "" {
		return nil
	}

	// --- REST API: set HTTP client with proxy transport ---
	httpClient, err := tachiproxy.NewHTTPClient(proxyURL, 30*time.Second)
	if err != nil {
		return fmt.Errorf("create proxy HTTP client: %w", err)
	}
	sess.Client = httpClient

	// --- Gateway WebSocket: set proxy dialer ---
	proxyDialer, err := tachiproxy.NewDialer(proxyURL)
	if err != nil {
		return fmt.Errorf("create proxy dialer: %w", err)
	}

	// Preserve existing Dialer defaults (timeout, handshake settings, etc.)
	// by only replacing the network dial layer.
	if sess.Dialer == nil {
		sess.Dialer = &websocket.Dialer{}
	}
	sess.Dialer.NetDial = proxyDialer

	ch.logger.Info(ctx, "discord: configured proxy", "proxy", proxyURL)
	return nil
}
