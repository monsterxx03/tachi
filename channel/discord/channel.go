package discord

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"
	"github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/channel"
	"github.com/monsterxx03/tachi/pkg/debuglog"
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
	logger  *debuglog.Logger

	// httpClient is used for CDN downloads and other HTTP requests.
	// Configured with proxy support when cfg.Proxy is set.
	httpClient *http.Client

	// Runtime state
	mu        sync.RWMutex
	botUserID string // Bot's own UserID (for @mention detection)

	memberCache *memberCache // Guild member info cache
	deduper     *messageDeduper

	// Component callback registry (Phase 2+)
	componentHandlers map[string]componentHandler

	// Slash command handler (injected by Manager via CommandChannel interface)
	cmdHandler channel.CommandHandler

	// Provider names for slash command autocomplete (injected by Manager)
	providerNames []string

	// Attachment cache directory
	cacheDir string

	// Per-channel topic status tracking — avoids unnecessary API calls
	// when directory and git branch haven't changed.
	topicStatus   map[string]topicEntry // channelID → last topic info
	topicStatusMu sync.Mutex
}

// topicEntry holds the last known directory and git branch for a channel topic.
type topicEntry struct {
	dir    string
	branch string
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
		cfg:               dc,
		logger:            debuglog.DefaultLogger.WithSource("channel:discord"),
		httpClient:        httpClient,
		cacheDir:          cacheDir,
		componentHandlers: make(map[string]componentHandler),
		memberCache:       newMemberCache(),
		deduper:           newMessageDeduper(),
		topicStatus:       make(map[string]topicEntry),
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

// SystemPromptSuffix implements channel.SystemPromptSuffixer.
// Appends Discord-specific instructions to the agent's system prompt
// so the LLM knows about platform limitations and the Embed format.
func (ch *DiscordChannel) SystemPromptSuffix() string {
	return `## Discord Platform Limitations

Discord message content has limited markdown support:
- ❌ No tables (| col1 | col2 |)
- ❌ No HTML
- ❌ No horizontal rules ---

For structured/tabular data, use the [EMBED] format with fields:
EMBED:title|description|color
field:Name|Value|true(optional, inline)

Example:
EMBED:📊 Overview|Project stats|#3498DB
field:Name|Tachi|true
field:Language|Go|true
field:Status|Active

Notes:
- EMBED: must be the first line of the message
- field: lines follow the EMBED: line, unlimited
- inline=true makes fields display side by side (default false)`
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
		_, err := sess.ChannelMessageSend(channelID, msg.Content)
		if err != nil {
			return fmt.Errorf("discord: Send text: %w", err)
		}
	}

	// Send each attachment.
	for _, att := range msg.Attachments {
		var data []byte
		if att.Data != nil {
			data = att.Data
		} else if att.LocalPath != "" {
			var err error
			data, err = os.ReadFile(att.LocalPath)
			if err != nil {
				ch.logger.Log("discord: Send read attachment %s: %v", att.FileName, err)
				continue
			}
		} else {
			continue
		}
		_, err := sess.ChannelFileSend(channelID, att.FileName, bytes.NewReader(data))
		if err != nil {
			ch.logger.Log("discord: Send attachment %s error: %v (continuing)", att.FileName, err)
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

	ch.logger.Log("discord: cache dir ready at %s", ch.cacheDir)
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
	ch.logger.Log("discord: registered %d slash commands (%s)", len(registered), loc)
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

	// Apply proxy configuration if set.
	if err := ch.applyProxy(sess); err != nil {
		return fmt.Errorf("discord: proxy config: %w", err)
	}

	// Register event handlers.
	// AddHandler returns a func() that removes the handler when called.
	// We store these to prevent duplicate registration if Run() is called
	// again (see design doc §2.3).
	removeReady := sess.AddHandler(ch.onReady())
	removeMessage := sess.AddHandler(ch.onMessageCreate(handler))
	removeInteraction := sess.AddHandler(ch.onInteractionCreate(handler))

	// Also register GUILD_CREATE for member cache warmup and
	// GUILD_MEMBER_UPDATE for incremental cache updates.
	removeGuildCreate := sess.AddHandler(ch.onGuildCreate())
	removeGuildMemberUpdate := sess.AddHandler(ch.onGuildMemberUpdate())

	// Configure Intents.
	// IntentsGuilds: needed for GUILD_CREATE member cache warmup.
	// IntentsGuildMessages: receive guild channel messages.
	// IntentsDirectMessages: receive DMs.
	// IntentsMessageContent: privileged intent — read message content.
	sess.Identify.Intents = discordgo.IntentsGuildMessages |
		discordgo.IntentsDirectMessages |
		discordgo.IntentsMessageContent

	// Open Gateway connection.
	if err := sess.Open(); err != nil {
		return fmt.Errorf("discord: gateway open: %w", err)
	}
	defer sess.Close()

	ch.logger.Log("discord: connected to gateway")

	// Send greeting if configured.
	ch.sendGreeting()

	// Wait for context cancellation.
	<-ctx.Done()
	ch.logger.Log("discord: context cancelled, shutting down")

	// Remove handlers to prevent re-registration on future Run() calls.
	removeReady()
	removeMessage()
	removeInteraction()
	removeGuildCreate()
	removeGuildMemberUpdate()

	return nil
}

// onReady returns a handler for the READY event, capturing the bot's user ID
// and registering slash commands.
func (ch *DiscordChannel) onReady() any {
	return func(s *discordgo.Session, r *discordgo.Ready) {
		ch.mu.Lock()
		ch.botUserID = r.User.ID
		ch.mu.Unlock()
		ch.logger.Log("discord: ready — bot user ID: %s", r.User.ID)

		// Register slash commands now that the ApplicationID is known.
		if err := ch.registerSlashCommands(s); err != nil {
			ch.logger.Log("discord: register slash commands (non-fatal): %v", err)
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

// onMessageCreate returns a handler for MESSAGE_CREATE events.
// Delegates to handler.go's handleMessageCreate for the full processing pipeline.
func (ch *DiscordChannel) onMessageCreate(handler channel.MessageHandler) any {
	return func(s *discordgo.Session, m *discordgo.MessageCreate) {
		ch.handleMessageCreate(s, m, handler)
	}
}

// onInteractionCreate returns a handler for INTERACTION_CREATE events.
// Handles APPLICATION_COMMAND (slash commands) and AUTOCOMPLETE interactions.
func (ch *DiscordChannel) onInteractionCreate(handler channel.MessageHandler) any {
	return func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		switch i.Type {
		case discordgo.InteractionApplicationCommand:
			ch.handleSlashCommand(s, i)
		case discordgo.InteractionApplicationCommandAutocomplete:
			ch.handleAutocomplete(s, i)
		}
	}
}

// handleSlashCommand processes an APPLICATION_COMMAND interaction.
func (ch *DiscordChannel) handleSlashCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()
	if data.Name == "" {
		return
	}

	ch.logger.Log("discord: slash cmd=%q options=%d", data.Name, len(data.Options))

	cmdHandler := ch.cmdHandler
	if cmdHandler == nil {
		ch.respondInteraction(s, i, "❌ Slash command handler not initialized")
		return
	}

	// Build args from command options.
	args := buildSlashArgs(data.Options)
	ch.logger.Log("discord: slash cmd=%q args=%q", data.Name, args)

	// Determine thread ID from the interaction context.
	threadID := ""
	if i.GuildID != "" {
		threadID = threadIDForGuild(i.GuildID, i.ChannelID)
	} else {
		threadID = threadIDForDM(i.User.ID)
	}

	// Execute the command via the Manager's CommandHandler.
	reply, workDir, err := cmdHandler(context.Background(), channel.SlashCommand{
		Name:     data.Name,
		ThreadID: threadID,
		Args:     args,
	})
	if err != nil {
		ch.respondInteraction(s, i, "❌ "+err.Error())
		return
	}
	if reply == "" {
		reply = "✅ Done"
	}
	ch.respondInteraction(s, i, reply)

	// Update channel topic with the thread's current working directory.
	if workDir != "" && !isDM(i.GuildID) {
		ch.updateChannelTopic(i.ChannelID, workDir)
	}
}

// handleAutocomplete processes an AUTOCOMPLETE interaction.
// Returns provider name suggestions for the /model command.
func (ch *DiscordChannel) handleAutocomplete(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()
	if data.Name != "model" || len(data.Options) == 0 {
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionApplicationCommandAutocompleteResult,
			Data: &discordgo.InteractionResponseData{Choices: []*discordgo.ApplicationCommandOptionChoice{}},
		})
		return
	}

	// Get user's current input for prefix filtering (empty = show all).
	prefix := strings.ToLower(data.Options[0].StringValue())

	// Build choices from configured provider names, filtered by prefix.
	choices := make([]*discordgo.ApplicationCommandOptionChoice, 0, len(ch.providerNames))
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
	if choices == nil {
		choices = []*discordgo.ApplicationCommandOptionChoice{}
	}

	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionApplicationCommandAutocompleteResult,
		Data: &discordgo.InteractionResponseData{Choices: choices},
	})
}

// buildSlashArgs converts discordgo ApplicationCommandInteractionDataOption
// into a space-separated argument string.
func buildSlashArgs(options []*discordgo.ApplicationCommandInteractionDataOption) string {
	var parts []string
	for _, opt := range options {
		if opt.StringValue() != "" {
			parts = append(parts, opt.StringValue())
		}
	}
	return strings.Join(parts, " ")
}

// respondInteraction sends an ephemeral response to a Discord interaction.
func (ch *DiscordChannel) respondInteraction(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: content,
			Flags:   1 << 6, // ephemeral
		},
	}); err != nil {
		ch.logger.Log("discord: interaction respond error: %v", err)
	}
}

// sendGreeting sends the startup greeting as a DM to the first allowed user.
func (ch *DiscordChannel) sendGreeting() {
	if ch.cfg.Greeting == "" {
		return
	}
	if len(ch.cfg.AllowedUsers) == 0 {
		ch.logger.Log("discord: greeting configured but no allowed_users to send to")
		return
	}
	sess := ch.session
	if sess == nil {
		return
	}
	// Create/open a DM channel with the first allowed user.
	dmChannel, err := sess.UserChannelCreate(ch.cfg.AllowedUsers[0])
	if err != nil {
		ch.logger.Log("discord: create DM channel for greeting: %v", err)
		return
	}
	if _, err := sess.ChannelMessageSend(dmChannel.ID, ch.cfg.Greeting); err != nil {
		ch.logger.Log("discord: send greeting error: %v", err)
	}
}

// applyProxy configures the discordgo session to use a proxy for both
// REST API calls (sess.Client) and Gateway WebSocket connection (sess.Dialer).
// It is a no-op when cfg.Proxy is empty.
func (ch *DiscordChannel) applyProxy(sess *discordgo.Session) error {
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

	ch.logger.Log("discord: configured proxy %s", proxyURL)
	return nil
}
