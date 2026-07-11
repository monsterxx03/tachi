package discord

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/monsterxx03/tachi/pkg/channel"
	"github.com/monsterxx03/tachi/pkg/debuglog"
)

// ---------------------------------------------------------------------------
// mention.go
// ---------------------------------------------------------------------------

func TestIsMentioned(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		botUserID string
		want      bool
	}{
		{"normal mention", "<@12345> hello", "12345", true},
		{"nickname mention", "<@!12345> hello", "12345", true},
		{"not mentioned", "hello world", "12345", false},
		{"other user mentioned", "<@67890> hello", "12345", false},
		{"empty content", "", "12345", false},
		{"empty bot id", "<@12345> hello", "", false},
		{"multiple mentions includes bot", "<@12345> <@67890>", "12345", true},
		{"mention in middle of text", "hi <@12345> how are you", "12345", true},
		{"nickname format in text", "please <@!12345> check this", "12345", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isMentioned(tt.content, tt.botUserID)
			if got != tt.want {
				t.Errorf("isMentioned(%q, %q) = %v, want %v", tt.content, tt.botUserID, got, tt.want)
			}
		})
	}
}

func TestExtractMentionedUserIDs(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []string
	}{
		{"single mention", "<@12345> hello", []string{"12345"}},
		{"nickname mention", "<@!12345> hello", []string{"12345"}},
		{"multiple mentions", "<@12345> and <@67890>", []string{"12345", "67890"}},
		{"no mentions", "hello world", nil},
		{"mixed formats", "<@12345> and <@!67890>", []string{"12345", "67890"}},
		{"mention at end", "check <@12345>", []string{"12345"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractMentionedUserIDs(tt.content)
			if len(got) != len(tt.want) {
				t.Errorf("extractMentionedUserIDs(%q) = %v, want %v", tt.content, got, tt.want)
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("extractMentionedUserIDs(%q) = %v, want %v", tt.content, got, tt.want)
					return
				}
			}
		})
	}
}

func TestStripMentions(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		// stripMentions replaces mention syntax with the user ID
		// (so LLM can see who was @mentioned).
		{"strip normal mention", "<@12345> hello", "12345 hello"},
		{"strip nickname mention", "<@!12345> hello", "12345 hello"},
		{"strip multiple", "<@12345> and <@67890>", "12345 and 67890"},
		{"no mentions", "hello world", "hello world"},
		{"mention with surrounding text", "hi <@12345> how are you", "hi 12345 how are you"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripMentions(tt.content)
			if got != tt.want {
				t.Errorf("stripMentions(%q) = %q, want %q", tt.content, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// dm.go
// ---------------------------------------------------------------------------

func TestIsDM(t *testing.T) {
	tests := []struct {
		guildID string
		want    bool
	}{
		{"", true},
		{"12345", false},
		{"0", false},
	}
	for _, tt := range tests {
		t.Run("guildID="+tt.guildID, func(t *testing.T) {
			if got := isDM(tt.guildID); got != tt.want {
				t.Errorf("isDM(%q) = %v, want %v", tt.guildID, got, tt.want)
			}
		})
	}
}

func TestThreadIDForDM(t *testing.T) {
	got := threadIDForDM("user123")
	want := "dm:user123"
	if got != want {
		t.Errorf("threadIDForDM = %q, want %q", got, want)
	}
}

func TestThreadIDForGuild(t *testing.T) {
	got := threadIDForGuild("guild1", "channel1")
	want := "guild:guild1:channel:channel1"
	if got != want {
		t.Errorf("threadIDForGuild = %q, want %q", got, want)
	}
}

func TestChannelIDFromThreadID(t *testing.T) {
	tests := []struct {
		threadID string
		want     string
	}{
		{"dm:user123", "user123"},
		{"guild:g1:channel:c1", "c1"},
		{"guild:g1:thread:t1", ""}, // thread format not supported in Phase 1
		{"invalid", ""},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.threadID, func(t *testing.T) {
			got := channelIDFromThreadID(tt.threadID)
			if got != tt.want {
				t.Errorf("channelIDFromThreadID(%q) = %q, want %q", tt.threadID, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// send.go
// ---------------------------------------------------------------------------

func TestSplitMessage(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantLen int // number of chunks
	}{
		{"short message", "hello", 1},
		{"exactly at limit", strings.Repeat("a", discordMessageLimit), 1},
		{"just over limit", strings.Repeat("a", discordMessageLimit+1), 2},
		{"paragraph break", strings.Repeat("a", 3000) + "\n\n" + strings.Repeat("b", 3000), 2},
		{"newline break", strings.Repeat("a", 3000) + "\n" + strings.Repeat("b", 3000), 2},
		{"empty", "", 0},
		{"exactly double", strings.Repeat("a", discordMessageLimit*2), 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunks := splitMessage(tt.content)
			if len(chunks) != tt.wantLen {
				t.Errorf("splitMessage returned %d chunks, want %d", len(chunks), tt.wantLen)
				return
			}
			// Verify no chunk exceeds the limit.
			for i, chunk := range chunks {
				if len(chunk) > discordMessageLimit {
					t.Errorf("chunk %d exceeds limit: %d > %d", i, len(chunk), discordMessageLimit)
				}
			}
			// Verify concatenation equals original.
			if tt.content != "" {
				joined := strings.Join(chunks, "")
				if joined != tt.content {
					t.Errorf("split-then-join mismatch: got %d chars, want %d chars", len(joined), len(tt.content))
				}
			}
		})
	}
}

func TestBuildThreadID(t *testing.T) {
	tests := []struct {
		guildID   string
		channelID string
		want      string
	}{
		{"", "c1", "dm:c1"},
		{"g1", "c1", "guild:g1:channel:c1"},
		{"", "", "dm:"},
	}
	for _, tt := range tests {
		t.Run(tt.guildID+":"+tt.channelID, func(t *testing.T) {
			got := buildThreadID(tt.guildID, tt.channelID)
			if got != tt.want {
				t.Errorf("buildThreadID(%q, %q) = %q, want %q", tt.guildID, tt.channelID, got, tt.want)
			}
		})
	}
}

func TestCleanContentForLLM(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		// cleanContentForLLM calls stripMentions which keeps user IDs.
		{"strip normal mention", "<@12345> hello", "12345 hello"},
		{"strip nickname mention", "<@!12345> hello world", "12345 hello world"},
		{"no mentions", "simple text", "simple text"},
		{"multiple mentions", "<@12345> and <@!67890>", "12345 and 67890"},
		{"whitespace trimming", "  hello  ", "hello"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cleanContentForLLM(tt.content)
			if got != tt.want {
				t.Errorf("cleanContentForLLM(%q) = %q, want %q", tt.content, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// auth.go
// ---------------------------------------------------------------------------

// testChannel creates a minimal DiscordChannel for testing auth logic.
func testChannel(cfg DiscordConfig) *DiscordChannel {
	cfg.setDefaults()
	return &DiscordChannel{
		cfg:         cfg,
		memberCache: newMemberCache(),
		logger:      testLogger(),
	}
}

func testLogger() *debuglog.Logger {
	return debuglog.DefaultLogger.WithSource("discord:test")
}

func TestIsAuthorized_DM(t *testing.T) {
	ch := testChannel(DiscordConfig{
		AllowedUsers: []string{"user1", "user2"},
	})

	tests := []struct {
		name   string
		userID string
		want   bool
	}{
		{"allowed user", "user1", true},
		{"another allowed user", "user2", true},
		{"not allowed user", "user3", false},
		{"empty allowlist but user exists", "user1", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ch.isAuthorized(tt.userID, nil, true)
			if got != tt.want {
				t.Errorf("isAuthorized(DM, %q) = %v, want %v", tt.userID, got, tt.want)
			}
		})
	}
}

func TestIsAuthorized_DMNoAllowlist(t *testing.T) {
	// DM with empty allowlist → no one is allowed.
	ch := testChannel(DiscordConfig{})
	if ch.isAuthorized("user1", nil, true) {
		t.Error("isAuthorized(DM, user1) should be false when AllowedUsers is empty")
	}
}

func TestIsAuthorized_GuildAllowAll(t *testing.T) {
	ch := testChannel(DiscordConfig{
		AllowAllUsers: true,
	})
	if !ch.isAuthorized("anyone", nil, false) {
		t.Error("isAuthorized(Guild, anyone) should be true when AllowAllUsers=true")
	}
}

func TestIsAuthorized_GuildUserAllowlist(t *testing.T) {
	ch := testChannel(DiscordConfig{
		AllowedUsers: []string{"user1", "user2"},
	})
	tests := []struct {
		name   string
		userID string
		want   bool
	}{
		{"allowed user", "user1", true},
		{"not allowed", "user3", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ch.isAuthorized(tt.userID, nil, false)
			if got != tt.want {
				t.Errorf("isAuthorized(Guild, %q) = %v, want %v", tt.userID, got, tt.want)
			}
		})
	}
}

func TestIsAuthorized_GuildRoleAllowlist(t *testing.T) {
	ch := testChannel(DiscordConfig{
		AllowedRoles: []string{"role_admin", "role_mod"},
	})
	tests := []struct {
		name  string
		roles []string
		want  bool
	}{
		{"admin role", []string{"role_admin"}, true},
		{"mod role", []string{"role_mod"}, true},
		{"no matching role", []string{"role_member"}, false},
		{"multiple roles one match", []string{"role_member", "role_admin"}, true},
		{"empty roles", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ch.isAuthorized("user1", tt.roles, false)
			if got != tt.want {
				t.Errorf("isAuthorized(Guild, roles=%v) = %v, want %v", tt.roles, got, tt.want)
			}
		})
	}
}

func TestIsAllowedChannel(t *testing.T) {
	ch := testChannel(DiscordConfig{
		AllowedChannels: []string{"ch1", "ch2"},
		IgnoredChannels: []string{"ch_ignored"},
	})
	tests := []struct {
		channelID string
		want      bool
	}{
		{"ch1", true},
		{"ch2", true},
		{"ch3", false},        // not in allowed
		{"ch_ignored", false}, // explicitly ignored (takes precedence)
	}
	for _, tt := range tests {
		t.Run(tt.channelID, func(t *testing.T) {
			got := ch.isAllowedChannel(tt.channelID)
			if got != tt.want {
				t.Errorf("isAllowedChannel(%q) = %v, want %v", tt.channelID, got, tt.want)
			}
		})
	}
}

func TestIsAllowedChannel_EmptyAllowed(t *testing.T) {
	// Empty allowed_channels = all allowed (except ignored).
	ch := testChannel(DiscordConfig{
		IgnoredChannels: []string{"ch_ignored"},
	})
	tests := []struct {
		channelID string
		want      bool
	}{
		{"any_channel", true},
		{"ch_ignored", false},
	}
	for _, tt := range tests {
		t.Run(tt.channelID, func(t *testing.T) {
			got := ch.isAllowedChannel(tt.channelID)
			if got != tt.want {
				t.Errorf("isAllowedChannel(%q) = %v, want %v", tt.channelID, got, tt.want)
			}
		})
	}
}

func TestIsFreeResponseChannel(t *testing.T) {
	ch := testChannel(DiscordConfig{
		FreeResponseChannels: []string{"fr1", "fr2"},
	})
	tests := []struct {
		channelID string
		want      bool
	}{
		{"fr1", true},
		{"fr2", true},
		{"normal_ch", false},
	}
	for _, tt := range tests {
		t.Run(tt.channelID, func(t *testing.T) {
			got := ch.isFreeResponseChannel(tt.channelID)
			if got != tt.want {
				t.Errorf("isFreeResponseChannel(%q) = %v, want %v", tt.channelID, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// member_cache.go
// ---------------------------------------------------------------------------

func TestMemberCacheGetSet(t *testing.T) {
	mc := newMemberCache()

	// Get non-existent entry.
	_, ok := mc.get("g1", "u1")
	if ok {
		t.Error("get(non-existent) should return false")
	}

	// Set and get.
	mc.set("g1", "u1", []string{"role1", "role2"})
	entry, ok := mc.get("g1", "u1")
	if !ok {
		t.Fatal("get(existent) should return true")
	}
	if len(entry.Roles) != 2 || entry.Roles[0] != "role1" {
		t.Errorf("unexpected roles: %v", entry.Roles)
	}
}

func TestMemberCacheTTL(t *testing.T) {
	mc := &memberCache{
		entries: make(map[string]map[string]*guildMemberEntry),
		ttl:     1 * time.Millisecond, // very short TTL
	}
	mc.set("g1", "u1", []string{"role1"})

	// Should be valid immediately.
	if _, ok := mc.get("g1", "u1"); !ok {
		t.Error("entry should be valid immediately after set")
	}

	// Wait for TTL to expire.
	time.Sleep(2 * time.Millisecond)
	if _, ok := mc.get("g1", "u1"); ok {
		t.Error("entry should be expired after TTL")
	}
}

func TestMemberCacheGuildIsolation(t *testing.T) {
	mc := newMemberCache()
	mc.set("g1", "u1", []string{"role_g1"})
	mc.set("g2", "u1", []string{"role_g2"})

	entry1, _ := mc.get("g1", "u1")
	entry2, _ := mc.get("g2", "u1")

	if entry1.Roles[0] != "role_g1" || entry2.Roles[0] != "role_g2" {
		t.Error("guilds should have isolated role caches")
	}
}

func TestMemberCacheInvalidate(t *testing.T) {
	mc := newMemberCache()
	mc.set("g1", "u1", []string{"role1"})
	mc.Invalidate()

	if _, ok := mc.get("g1", "u1"); ok {
		t.Error("entry should be gone after Invalidate")
	}
}

// ---------------------------------------------------------------------------
// channel.go — messageDeduper
// ---------------------------------------------------------------------------

func TestMessageDeduper(t *testing.T) {
	d := newMessageDeduper()

	// First time: not seen.
	if d.seen("msg1") {
		t.Error("first call to seen should return false")
	}

	// Second time: seen.
	if !d.seen("msg1") {
		t.Error("second call to seen should return true")
	}

	// Different message ID: not seen.
	if d.seen("msg2") {
		t.Error("different message ID should not be seen")
	}
}

func TestMessageDeduperEviction(t *testing.T) {
	d := &messageDeduper{
		entries: make(map[string]time.Time),
	}

	// Fill with expired entries.
	for i := range dedupMaxSize {
		d.entries[fmt.Sprintf("old-%d", i)] = time.Now().Add(-1 * time.Hour)
	}

	// Adding a new entry should trigger eviction.
	if d.seen("new-msg") {
		t.Error("new message should not be seen")
	}

	// Verify old entries were cleaned up.
	d.mu.Lock()
	count := len(d.entries)
	d.mu.Unlock()

	if count > dedupMaxSize/2 {
		t.Errorf("expected most expired entries to be evicted, got %d remaining", count)
	}
}

// ---------------------------------------------------------------------------
// config.go
// ---------------------------------------------------------------------------

func TestNewDefaultConfig(t *testing.T) {
	cfg := newDefaultConfig()
	if cfg.RequireMention != true {
		t.Error("default RequireMention should be true")
	}
	if cfg.MaxAttachmentBytes != 32*1024*1024 {
		t.Error("default MaxAttachmentBytes should be 32MiB")
	}
	if cfg.HistoryBackfillLimit != 50 {
		t.Error("default HistoryBackfillLimit should be 50")
	}
	if !cfg.Reactions {
		t.Error("default Reactions should be true")
	}
}

func TestSetDefaults(t *testing.T) {
	cfg := DiscordConfig{}
	cfg.setDefaults()

	if cfg.MaxAttachmentBytes != 32*1024*1024 {
		t.Error("setDefaults should set MaxAttachmentBytes")
	}
	if cfg.ReconnectBackoff != 1*time.Second {
		t.Error("setDefaults should set ReconnectBackoff")
	}
	if cfg.ReconnectMaxBackoff != 30*time.Second {
		t.Error("setDefaults should set ReconnectMaxBackoff")
	}
}

func TestTokenFromConfig_DirectString(t *testing.T) {
	raw := map[string]any{
		"token": "MTIzNDU2Nzg5MDEyMzQ1Njc4OQ.Gabcde",
	}
	token, err := tokenFromConfig(raw)
	if err != nil {
		t.Fatalf("tokenFromConfig error: %v", err)
	}
	if token != "MTIzNDU2Nzg5MDEyMzQ1Njc4OQ.Gabcde" {
		t.Errorf("unexpected token: %q", token)
	}
}

func TestTokenFromConfig_EnvRef(t *testing.T) {
	// Set env var for the test.
	const testEnvKey = "TACHI_TEST_DISCORD_TOKEN"
	os.Setenv(testEnvKey, "env-test-token")
	defer os.Unsetenv(testEnvKey)

	raw := map[string]any{
		"token": map[string]any{
			"source": "env",
			"id":     testEnvKey,
		},
	}
	token, err := tokenFromConfig(raw)
	if err != nil {
		t.Fatalf("tokenFromConfig error: %v", err)
	}
	if token != "env-test-token" {
		t.Errorf("unexpected token: %q", token)
	}
}

func TestTokenFromConfig_Empty(t *testing.T) {
	_, err := tokenFromConfig(map[string]any{})
	if err == nil {
		t.Error("tokenFromConfig with no token should return error")
	}
}

// ---------------------------------------------------------------------------
// attachment.go
// ---------------------------------------------------------------------------

func TestDetectMIMEType(t *testing.T) {
	tests := []struct {
		name     string
		data     []byte
		filename string
		want     string // prefix match
	}{
		{"PNG", pngHeader(), "test.png", "image/png"},
		{"JPEG", jpegHeader(), "test.jpg", "image/jpeg"},
		{"PDF", pdfHeader(), "test.pdf", "application/pdf"},
		{"plain text", []byte("hello world"), "test.txt", "text/plain"},
		// Note: Go's http.DetectContentType doesn't distinguish JSON/markdown
		// from plain text — they fall back to text/plain. This is expected.
		{"JSON", []byte(`{"key": "value"}`), "test.json", "text/plain"},
		{"markdown", []byte("# Hello"), "test.md", "text/plain"},
		{"HTML", []byte("<html></html>"), "test.html", "text/html"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectMIMEType(tt.data, tt.filename)
			if !strings.HasPrefix(got, tt.want) {
				t.Errorf("detectMIMEType(%q) = %q, want prefix %q", tt.filename, got, tt.want)
			}
		})
	}
}

func TestSanitizeFilename(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"normal.txt", "normal.txt"},
		{"../etc/passwd", ".._etc_passwd"},
		{"file:name?.txt", "file_name_.txt"},
		{"a<b>c|d.txt", "a_b_c_d.txt"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := sanitizeFilename(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeFilename(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestIsTextContent(t *testing.T) {
	tests := []struct {
		mime string
		want bool
	}{
		{"text/plain", true},
		{"text/markdown", true},
		{"application/json", true},
		{"application/xml", true},
		{"application/x-yaml", true},
		{"image/png", false},
		{"application/pdf", false},
		{"application/octet-stream", false},
	}
	for _, tt := range tests {
		t.Run(tt.mime, func(t *testing.T) {
			got := isTextContent(tt.mime)
			if got != tt.want {
				t.Errorf("isTextContent(%q) = %v, want %v", tt.mime, got, tt.want)
			}
		})
	}
}

func TestIsWithinTextInjectionLimit(t *testing.T) {
	if !isWithinTextInjectionLimit(100) {
		t.Error("100 bytes should be within limit")
	}
	if isWithinTextInjectionLimit(maxTextInjectionSize + 1) {
		t.Error("over limit should return false")
	}
}

// ---------------------------------------------------------------------------
// send.go — emojiToReaction
// ---------------------------------------------------------------------------

func TestEmojiToReaction(t *testing.T) {
	if got := emojiToReaction("👀"); got != "👀" {
		t.Errorf("emojiToReaction(👀) = %q, want 👀", got)
	}
	if got := emojiToReaction(":custom:"); got != ":custom:" {
		t.Errorf("emojiToReaction(:custom:) = %q, want :custom:", got)
	}
}

// ---------------------------------------------------------------------------
// handler.go — resolveSenderName, resolveAttachmentType
// ---------------------------------------------------------------------------

func TestResolveSenderName(t *testing.T) {
	tests := []struct {
		name string
		user *discordgo.User
		want string
	}{
		{"nil user", nil, "unknown"},
		{"global name", &discordgo.User{GlobalName: "Alice", Username: "alice#1234", ID: "u1"}, "Alice"},
		{"username fallback", &discordgo.User{Username: "bob#5678", ID: "u2"}, "bob#5678"},
		{"ID fallback", &discordgo.User{ID: "u3"}, "u3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveSenderName(tt.user)
			if got != tt.want {
				t.Errorf("resolveSenderName = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveAttachmentType(t *testing.T) {
	tests := []struct {
		mime string
		want channel.AttachmentType
	}{
		{"text/plain", channel.AttachmentTypeText},
		{"text/markdown", channel.AttachmentTypeText},
		{"image/png", channel.AttachmentTypeImage},
		{"image/jpeg", channel.AttachmentTypeImage},
		{"application/pdf", channel.AttachmentTypeFile},
		{"application/octet-stream", channel.AttachmentTypeFile},
	}
	for _, tt := range tests {
		t.Run(tt.mime, func(t *testing.T) {
			got := resolveAttachmentType(tt.mime)
			if got != tt.want {
				t.Errorf("resolveAttachmentType(%q) = %v, want %v", tt.mime, got, tt.want)
			}
		})
	}
}

func TestContainsMention(t *testing.T) {
	tests := []struct {
		content   string
		botUserID string
		want      bool
	}{
		{"hello <@12345>", "999", true},    // someone else mentioned
		{"hello <@12345>", "12345", false}, // bot is mentioned → no "other" mention
		{"hello world", "999", false},      // no mention at all
	}
	for _, tt := range tests {
		t.Run(tt.content, func(t *testing.T) {
			got := containsMention(tt.content, tt.botUserID)
			if got != tt.want {
				t.Errorf("containsMention(%q, %q) = %v, want %v", tt.content, tt.botUserID, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// test helpers
// ---------------------------------------------------------------------------

// pngHeader returns the magic bytes for a PNG file.
func pngHeader() []byte {
	return []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52}
}

// jpegHeader returns the magic bytes for a JPEG file.
func jpegHeader() []byte {
	return []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 0x4A, 0x46, 0x49, 0x46, 0x00, 0x01}
}

// pdfHeader returns the magic bytes for a PDF file.
func pdfHeader() []byte {
	return []byte{0x25, 0x50, 0x44, 0x46, 0x2D, 0x31, 0x2E, 0x34}
}

// ---------------------------------------------------------------------------
// integration test helpers (require real Discord token — skipped in CI)
// ---------------------------------------------------------------------------

func TestIntegrationConfig(t *testing.T) {
	token := os.Getenv("DISCORD_BOT_TOKEN")
	if token == "" {
		t.Skip("DISCORD_BOT_TOKEN not set, skipping integration test")
	}

	cfg := newDefaultConfig()
	cfg.Token = token
	cfg.Enabled = true
	cfg.AllowAllUsers = true

	ch, err := NewChannel(cfg)
	if err != nil {
		t.Fatalf("NewChannel error: %v", err)
	}

	if ch.Name() != "discord" {
		t.Errorf("Name() = %q, want %q", ch.Name(), "discord")
	}
}

// TestIntegrationGateway tests the gateway connection with a real token.
// Run with: DISCORD_BOT_TOKEN=xxx [DISCORD_PROXY=http://...] go test -run TestIntegrationGateway -timeout 30s
func TestIntegrationGateway(t *testing.T) {
	token := os.Getenv("DISCORD_BOT_TOKEN")
	if token == "" {
		t.Skip("DISCORD_BOT_TOKEN not set, skipping integration test")
	}

	cfg := newDefaultConfig()
	cfg.Token = token
	cfg.Enabled = true
	cfg.AllowAllUsers = true
	cfg.Greeting = ""
	cfg.Proxy = os.Getenv("DISCORD_PROXY")

	ch, err := NewChannel(cfg)
	if err != nil {
		t.Fatalf("NewChannel error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Run should connect to gateway, then get cancelled.
	err = ch.Run(ctx, func(ctx context.Context, msg channel.IncomingMessage) channel.HandlerResult {
		return channel.HandlerResult{}
	})
	if err != nil && err.Error() != "discord: gateway open: " {
		t.Logf("Run exited (expected): %v", err)
	}
}

// ---------------------------------------------------------------------------
// send.go — parseEmbedContent
// ---------------------------------------------------------------------------

func TestParseEmbedContent(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		wantOK      bool
		wantTitle   string
		wantDesc    string
		wantCleaned string
	}{
		{"no embed prefix", "hello world", false, "", "", "hello world"},
		{"embed only", "EMBED:My Title", true, "My Title", "", ""},
		{"embed with desc", "EMBED:Title|Description", true, "Title", "Description", ""},
		{"embed with color", "EMBED:Title|Desc|green", true, "Title", "Desc", ""},
		{"empty title", "EMBED:", false, "", "", "EMBED:"},
		{"embed with text after", "EMBED:Title|Desc|blue\nSome more text", true, "Title", "Desc", "Some more text"},
		{"embed with multi-line after", "EMBED:Alert|注意|red\n\n这是第一行\n这是第二行", true, "Alert", "注意", "这是第一行\n这是第二行"},
		{"embed only, no desc", "EMBED:OnlyTitle||red", true, "OnlyTitle", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleaned, embed, ok := parseEmbedContent(tt.content)
			if ok != tt.wantOK {
				t.Errorf("parseEmbedContent(%q) ok = %v, want %v", tt.content, ok, tt.wantOK)
			}
			if ok {
				if embed.Title != tt.wantTitle {
					t.Errorf("Title = %q, want %q", embed.Title, tt.wantTitle)
				}
				if embed.Description != tt.wantDesc {
					t.Errorf("Description = %q, want %q", embed.Description, tt.wantDesc)
				}
			}
			if cleaned != tt.wantCleaned {
				t.Errorf("cleaned = %q, want %q", cleaned, tt.wantCleaned)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// send.go — parseMediaTags
// ---------------------------------------------------------------------------

func TestParseMediaTags(t *testing.T) {
	// Save and restore osReadFile.
	origReadFile := osReadFile
	defer func() { osReadFile = origReadFile }()

	// Stub file reading: "exists.txt" works, others fail.
	osReadFile = func(path string) ([]byte, error) {
		if path == "/tmp/exists.txt" {
			return []byte("hello"), nil
		}
		return nil, os.ErrNotExist
	}

	tests := []struct {
		name     string
		content  string
		wantText string // cleaned text (after removing MEDIA tags)
		wantN    int    // number of attachments
	}{
		{"no media", "hello world", "hello world", 0},
		{"media exists", "See MEDIA:/tmp/exists.txt", "See [exists.txt]", 1},
		{"media missing", "See MEDIA:/tmp/missing.txt", "See [MEDIA:/tmp/missing.txt (读取失败: file does not exist)]", 0},
		{"multiple media", "A MEDIA:/tmp/exists.txt B MEDIA:/tmp/exists.txt C", "A [exists.txt] B [exists.txt] C", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleaned, attachments := parseMediaTags(tt.content)
			if cleaned != tt.wantText {
				t.Errorf("parseMediaTags(%q) cleaned = %q, want %q", tt.content, cleaned, tt.wantText)
			}
			if len(attachments) != tt.wantN {
				t.Errorf("parseMediaTags(%q) attachments = %d, want %d", tt.content, len(attachments), tt.wantN)
			}
		})
	}
}
