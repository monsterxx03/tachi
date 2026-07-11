package discord

import (
	"fmt"
	"os"
	"time"
)

// DiscordConfig holds all configuration for the Discord channel.
//
// The config is loaded from the generic `channel.channels.discord` YAML path.
// All default values are set in NewChannel() — not via yaml struct tags —
// because Go's yaml.Unmarshal does not support `default` tags.
type DiscordConfig struct {
	Enabled       bool   `yaml:"enabled"`
	Token         string `yaml:"token"`          // Bot token; supports direct string or env reference
	ApplicationID string `yaml:"application_id"` // Application ID (optional, accelerates startup)

	// Access control
	AllowedUsers  []string `yaml:"allowed_users"`   // User ID allowlist
	AllowedRoles  []string `yaml:"allowed_roles"`   // Role ID allowlist
	AllowAllUsers bool     `yaml:"allow_all_users"` // Global switch (dev/private servers only)

	// Mention strategy
	RequireMention       bool     `yaml:"require_mention"`        // Guild channels require @mention (default: true)
	ThreadRequireMention bool     `yaml:"thread_require_mention"` // Threads require @mention (default: false)
	FreeResponseChannels []string `yaml:"free_response_channels"` // Channels that don't need @mention
	IgnoreOtherMentions  bool     `yaml:"ignore_other_mentions"`  // Ignore @others that don't @bot (default: true)

	// Channel routing
	IgnoredChannels []string `yaml:"ignored_channels"` // Never reply in these channels
	AllowedChannels []string `yaml:"allowed_channels"` // Only reply in these channels (empty = all allowed)
	HomeChannel     string   `yaml:"home_channel"`     // Channel for proactive messages (cron/notifications)

	// Thread
	AutoThread       bool     `yaml:"auto_thread"`        // Auto-create thread on @mention (default: false)
	NoThreadChannels []string `yaml:"no_thread_channels"` // Channels that skip auto-thread

	// Attachments
	MaxAttachmentBytes int64 `yaml:"max_attachment_bytes"` // Max attachment size (default: 32MiB)
	AllowAnyAttachment bool  `yaml:"allow_any_attachment"` // Allow arbitrary file types (default: false)

	// Feedback
	Reactions bool `yaml:"reactions"` // Enable 👀✅❌ reactions (default: true)
	Typing    bool `yaml:"typing"`    // Enable typing indicator (default: true)

	// Session
	GroupSessionsPerUser bool `yaml:"group_sessions_per_user"` // Per-user session isolation (default: false)

	// History backfill (Phase 2)
	HistoryBackfill      bool `yaml:"history_backfill"`       // Enable context backfill (default: true)
	HistoryBackfillLimit int  `yaml:"history_backfill_limit"` // Backfill scan limit (default: 50)

	// Per-channel system prompt overrides
	ChannelPrompts map[string]string `yaml:"channel_prompts"`

	// Greeting
	Greeting string `yaml:"greeting"` // Startup greeting message

	// Slash commands
	DevGuildID string `yaml:"dev_guild_id"` // Dev guild ID for instant guild-level commands (Phase 3)

	// Embed
	EmbedEnabled bool `yaml:"embed_enabled"` // Enable Embed message sending (Phase 2+, default: false)

	// Proxy
	Proxy string `yaml:"proxy"` // HTTP/HTTPS/SOCKS5 proxy URL (e.g. "http://127.0.0.1:7890")

	// Reconnect
	ReconnectMaxAttempts int           `yaml:"reconnect_max_attempts"` // Max reconnect attempts (0 = infinite)
	ReconnectBackoff     time.Duration `yaml:"reconnect_backoff"`      // Initial backoff (default: 1s)
	ReconnectMaxBackoff  time.Duration `yaml:"reconnect_max_backoff"`  // Max backoff (default: 30s)
}

// setDefaults populates unset fields with their default values.
// Called by NewChannel() after yaml unmarshal.
func (c *DiscordConfig) setDefaults() {
	if c.MaxAttachmentBytes == 0 {
		c.MaxAttachmentBytes = 32 * 1024 * 1024 // 32 MiB
	}
	if c.ReconnectBackoff == 0 {
		c.ReconnectBackoff = 1 * time.Second
	}
	if c.ReconnectMaxBackoff == 0 {
		c.ReconnectMaxBackoff = 30 * time.Second
	}
	if c.HistoryBackfillLimit == 0 {
		c.HistoryBackfillLimit = 50
	}

	// Bools and zero-value fields: safe to set unconditionally since yaml
	// won't populate them if they were explicitly set to false/0.
	// We only override when the zero value equals the default.

	// Note: we cannot distinguish "user set RequireMention=false" from
	// "user didn't set it" since bool zero value is false. The yaml
	// unmarshaler doesn't distinguish missing from false.
	// Documented defaults are applied in newDefaultConfig().
}

// newDefaultConfig returns a DiscordConfig with all documented defaults applied.
func newDefaultConfig() DiscordConfig {
	return DiscordConfig{
		RequireMention:       true,
		ThreadRequireMention: false,
		IgnoreOtherMentions:  true,
		AutoThread:           false,
		MaxAttachmentBytes:   32 * 1024 * 1024,
		Reactions:            true,
		Typing:               true,
		GroupSessionsPerUser: false,
		HistoryBackfill:      true,
		HistoryBackfillLimit: 50,
		ReconnectBackoff:     1 * time.Second,
		ReconnectMaxBackoff:  30 * time.Second,
	}
}

// tokenFromConfig resolves the bot token, supporting two formats:
//
// Direct string:
//
//	channel:
//	  channels:
//	    discord:
//	      token: "MTIzNDU2Nzg5MDEyMzQ1Njc4OQ.Gabcde..."
//
// Env reference:
//
//	channel:
//	  channels:
//	    discord:
//	      token:
//	        source: env
//	        id: DISCORD_BOT_TOKEN
func tokenFromConfig(rawCfg map[string]any) (string, error) {
	tokenRaw, ok := rawCfg["token"]
	if !ok {
		return "", fmt.Errorf("discord: token is required")
	}

	switch v := tokenRaw.(type) {
	case string:
		if v == "" {
			return "", fmt.Errorf("discord: token is empty")
		}
		return v, nil
	case map[string]any:
		source, _ := v["source"].(string)
		if source == "env" {
			id, _ := v["id"].(string)
			if id == "" {
				return "", fmt.Errorf("discord: env token reference has empty 'id'")
			}
			val := os.Getenv(id)
			if val == "" {
				return "", fmt.Errorf("discord: env %q is empty or not set", id)
			}
			return val, nil
		}
		return "", fmt.Errorf("discord: unsupported token source: %q", source)
	default:
		return "", fmt.Errorf("discord: token must be a string or env reference")
	}
}
