package discord

import (
	"fmt"
	"os"
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

	// Attachments
	//
	// Any file type is accepted, up to MaxAttachmentBytes. There is no
	// allow-list, so there is no `allow_any_attachment` knob: that key existed
	// only as a field in this struct and was never read (see #173).
	MaxAttachmentBytes int64 `yaml:"max_attachment_bytes"` // Max attachment size (default: 32MiB)

	// Feedback
	Typing bool `yaml:"typing"` // Enable typing indicator (default: true)

	// Per-channel system prompt overrides
	ChannelPrompts map[string]string `yaml:"channel_prompts"`

	// Greeting
	Greeting string `yaml:"greeting"` // Startup greeting message

	// Slash commands
	DevGuildID string `yaml:"dev_guild_id"` // Dev guild ID for instant guild-level commands (Phase 3)

	// SuppressEmbeds disables link previews (embeds) for all messages sent
	// by the bot. When true (default), Discord will not fetch or display
	// link previews for URLs in the bot's messages.
	SuppressEmbeds bool `yaml:"suppress_embeds"`

	// Proxy
	Proxy string `yaml:"proxy"` // HTTP/HTTPS/SOCKS5 proxy URL (e.g. "http://127.0.0.1:7890")
}

// setDefaults populates unset fields with their default values.
// Called by NewChannel() after yaml unmarshal.
func (c *DiscordConfig) setDefaults() {
	if c.MaxAttachmentBytes == 0 {
		c.MaxAttachmentBytes = 32 * 1024 * 1024 // 32 MiB
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
		MaxAttachmentBytes:   32 * 1024 * 1024,
		Typing:               true,
		SuppressEmbeds:       true,
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
