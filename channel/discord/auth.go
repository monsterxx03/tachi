package discord

import "slices"

// isAuthorized checks whether a user is allowed to interact with the bot.
//
// Guild channels: checked against allowed_users, allowed_roles, and allow_all_users.
// DM: only checked against allowed_users (roles don't apply in DMs).
//
// memberRoles is the user's current role list for the guild (can be nil).
func (ch *DiscordChannel) isAuthorized(userID string, memberRoles []string, isDM bool) bool {
	cfg := ch.cfg

	if isDM {
		// DM: only user-level allowlist applies.
		if len(cfg.AllowedUsers) == 0 {
			// No allowlist = no one allowed in DMs.
			return false
		}
		return slices.Contains(cfg.AllowedUsers, userID)
	}

	// Guild channel: global override.
	if cfg.AllowAllUsers {
		return true
	}

	// Check user allowlist.
	if slices.Contains(cfg.AllowedUsers, userID) {
		return true
	}

	// Check role allowlist.
	if len(cfg.AllowedRoles) > 0 && memberRoles != nil {
		for _, role := range memberRoles {
			if slices.Contains(cfg.AllowedRoles, role) {
				return true
			}
		}
	}

	return false
}

// isAllowedChannel checks whether the channel ID is permitted for interaction.
// Returns true if:
//   - allowed_channels is empty (no restriction), OR
//   - the channel is in the allowed_channels list
//
// AND
//   - the channel is NOT in the ignored_channels list
//
// When allowed_channels is non-empty, channels not in the list are denied
// even if they're not explicitly ignored.
func (ch *DiscordChannel) isAllowedChannel(channelID string) bool {
	cfg := ch.cfg

	// Check ignored channels (deny list takes precedence).
	if slices.Contains(cfg.IgnoredChannels, channelID) {
		return false
	}

	// Check allowed channels (empty = all allowed).
	if len(cfg.AllowedChannels) == 0 {
		return true
	}
	return slices.Contains(cfg.AllowedChannels, channelID)
}

// isFreeResponseChannel returns true if the channel doesn't require @mention.
func (ch *DiscordChannel) isFreeResponseChannel(channelID string) bool {
	return slices.Contains(ch.cfg.FreeResponseChannels, channelID)
}

// resolveMemberRoles retrieves the member's roles, using the cache if possible.
// Returns nil if the roles can't be determined (DM or API error).
func (ch *DiscordChannel) resolveMemberRoles(guildID, userID string) []string {
	if guildID == "" {
		return nil // DM has no roles
	}
	return ch.memberCache.getOrFetch(ch.session, guildID, userID)
}
