package discord

import (
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	defaultMemberCacheTTL = 5 * time.Minute
)

// guildMemberEntry caches a member's role list with a TTL for staleness.
type guildMemberEntry struct {
	Roles []string
	TTL   time.Time
}

// expired returns true if the entry is past its TTL and should be refreshed.
func (e *guildMemberEntry) expired() bool {
	return time.Now().After(e.TTL)
}

// memberCache provides TTL-based caching of guild member information.
// Cache keys are "guildID:userID" pairs since the same user may have
// different roles in different guilds.
type memberCache struct {
	mu      sync.RWMutex
	entries map[string]map[string]*guildMemberEntry // guildID → userID → entry
	ttl     time.Duration
}

// newMemberCache creates a member cache with the default 5-minute TTL.
func newMemberCache() *memberCache {
	return &memberCache{
		entries: make(map[string]map[string]*guildMemberEntry),
		ttl:     defaultMemberCacheTTL,
	}
}

// get returns the cached member entry for the given guild and user.
// The second return value is false if the entry doesn't exist or is expired.
func (mc *memberCache) get(guildID, userID string) (*guildMemberEntry, bool) {
	mc.mu.RLock()
	defer mc.mu.RUnlock()

	users, ok := mc.entries[guildID]
	if !ok {
		return nil, false
	}
	entry, ok := users[userID]
	if !ok {
		return nil, false
	}
	if entry.expired() {
		return nil, false
	}
	return entry, true
}

// set stores a member entry in the cache with the configured TTL.
func (mc *memberCache) set(guildID, userID string, roles []string) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	if mc.entries[guildID] == nil {
		mc.entries[guildID] = make(map[string]*guildMemberEntry)
	}
	mc.entries[guildID][userID] = &guildMemberEntry{
		Roles: roles,
		TTL:   time.Now().Add(mc.ttl),
	}
}

// getOrFetch returns cached member roles, or fetches from the Discord API
// on cache miss (cold-start fallback). Returns an empty slice on error.
func (mc *memberCache) getOrFetch(sess *discordgo.Session, guildID, userID string) []string {
	// Try cache first.
	if entry, ok := mc.get(guildID, userID); ok {
		return entry.Roles
	}

	// Cache miss — fetch from API.
	if sess == nil {
		return nil
	}

	member, err := sess.GuildMember(guildID, userID)
	if err != nil {
		// Non-fatal: log is handled by caller.
		return nil
	}

	roles := member.Roles
	if roles == nil {
		roles = []string{}
	}
	mc.set(guildID, userID, roles)
	return roles
}

// Invalidate clears the entire cache. Useful after a long gateway disconnect
// where role data may be stale across all guilds.
func (mc *memberCache) Invalidate() {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	mc.entries = make(map[string]map[string]*guildMemberEntry)
}

// handleGuildMemberUpdate processes a GUILD_MEMBER_UPDATE event to keep
// the cache fresh.
func (mc *memberCache) handleGuildMemberUpdate(s *discordgo.Session, m *discordgo.GuildMemberUpdate) {
	if m.Member == nil {
		return
	}
	roles := m.Member.Roles
	if roles == nil {
		roles = []string{}
	}
	mc.set(m.GuildID, m.Member.User.ID, roles)
}

// warmupFromGuildCreate populates the cache from a GUILD_CREATE event,
// which contains the full member list for small-to-medium guilds.
// For large guilds, members are not included in GUILD_CREATE and must
// be fetched on-demand via getOrFetch.
func (mc *memberCache) warmupFromGuildCreate(s *discordgo.Session, g *discordgo.GuildCreate) {
	if g.Members == nil {
		return
	}
	for _, member := range g.Members {
		if member.User == nil {
			continue
		}
		roles := member.Roles
		if roles == nil {
			roles = []string{}
		}
		mc.set(g.Guild.ID, member.User.ID, roles)
	}
}
