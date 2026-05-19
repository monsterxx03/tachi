package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/pkg/debuglog"
	"github.com/monsterxx03/tachi/pkg/proxy"
)

// Mem9Backend stores memory in the mem9 cloud vector database.
// It writes at all three scopes (turn/compact/session) with content filtering,
// and uses vector semantic search for recall.
type Mem9Backend struct {
	http           *http.Client
	baseURL        string
	apiKey         string
	agentID        string
	mode           string
	requestTimeout time.Duration
}

// NewMem9Backend creates a Mem9Backend using the given config.
func NewMem9Backend(cfg Config) (*Mem9Backend, error) {
	baseURL := strings.TrimRight(cfg.Mem9.APIURL, "/")
	if baseURL == "" {
		baseURL = "https://api.mem9.ai"
	}
	agentID := cfg.Mem9.AgentID
	if agentID == "" {
		agentID = "tachi"
	}
	mode := cfg.Mem9.Mode
	if mode == "" {
		mode = "smart"
	}

	httpClient, err := proxy.NewHTTPClient(cfg.Mem9.Proxy, cfg.Mem9.RequestTimeout)
	if err != nil {
		return nil, fmt.Errorf("mem9: create http client: %w", err)
	}

	return &Mem9Backend{
		http:           httpClient,
		baseURL:        baseURL,
		apiKey:         cfg.Mem9.APIKey,
		agentID:        agentID,
		mode:           mode,
		requestTimeout: cfg.Mem9.RequestTimeout,
	}, nil
}

// ContentFilter defines filtering parameters per scope.
type ContentFilter struct {
	MaxMessages int
	MaxBytes    int
}

// scopeFilters maps each StoreScope to its filtering parameters.
// Only Turn is used for mem9; Compact and Session are kept for the
// interface contract but mem9 no-ops them.
var scopeFilters = map[StoreScope]ContentFilter{
	StoreScopeTurn: {MaxMessages: 4, MaxBytes: 20 * 1024},
}

// Store writes memory to mem9 API. Only StoreScopeTurn is processed;
// compact and session scopes are no-ops since mem9 already receives
// turn-level data incrementally (API deduplicates by session_id).
//
// When opts.DirectContent is set, the content is written directly via the
// content field (not ingest-based). Otherwise, opts.TurnMessages are filtered
// and ingested via the messages field.
func (b *Mem9Backend) Store(ctx context.Context, opts StoreOptions) error {
	// Direct content write — no message filtering, uses the content API path.
	if opts.DirectContent != "" {
		body := map[string]any{
			"content":    opts.DirectContent,
			"tags":       opts.Tags,
			"agent_id":   b.agentID,
			"session_id": opts.SessionID,
		}
		return b.doRequest(ctx, "POST", "/v1alpha2/mem9s/memories", body, nil)
	}

	if opts.Scope != StoreScopeTurn {
		return nil
	}

	messages := opts.TurnMessages

	// Apply content filtering
	filter := scopeFilters[opts.Scope]
	messages = b.filterMessages(messages, filter)
	if len(messages) == 0 {
		return nil
	}

	// Build mem9 API body
	body := map[string]any{
		"session_id": opts.SessionID,
		"agent_id":   b.agentID,
		"mode":       b.mode,
		"tags":       opts.Tags,
		"messages":   messages,
	}

	return b.doRequest(ctx, "POST", "/v1alpha2/mem9s/memories", body, nil)
}

// Recall searches mem9 for semantically relevant memories.
// Uses the user's raw prompt as the query. Cross-agent recall is enabled by
// omitting the agent_id filter, so memories from other tools (Claude Code,
// OpenClaw, etc.) under the same account are also retrieved.
func (b *Mem9Backend) Recall(ctx context.Context, query string, limit int) ([]Entry, error) {
	if query == "" {
		return nil, nil
	}

	if limit <= 0 {
		limit = 5
	}

	u := fmt.Sprintf("/v1alpha2/mem9s/memories?q=%s&limit=%d",
		url.QueryEscape(query), limit)

	var result struct {
		Memories []struct {
			ID        string   `json:"id"`
			SessionID string   `json:"session_id"`
			Content   string   `json:"content"`
			Tags      []string `json:"tags"`
			Score     float64  `json:"confidence"`
			CreatedAt string   `json:"created_at"`
		} `json:"memories"`
	}

	if err := b.doRequest(ctx, "GET", u, nil, &result); err != nil {
		return nil, err
	}

	entries := make([]Entry, 0, len(result.Memories))
	for _, m := range result.Memories {
		ts := int64(0)
		if t, err := time.Parse(time.RFC3339, m.CreatedAt); err == nil {
			ts = t.Unix()
		}
		entries = append(entries, Entry{
			ID:        m.ID,
			SessionID: m.SessionID,
			Summary:   truncateStr(m.Content, 80),
			Content:   m.Content,
			Tags:      m.Tags,
			Score:     m.Score,
			Timestamp: ts,
		})
	}
	return entries, nil
}

// Forget deletes a memory by its mem9 ID.
func (b *Mem9Backend) Forget(ctx context.Context, id string) error {
	u := fmt.Sprintf("/v1alpha2/mem9s/memories/%s", id)
	return b.doRequest(ctx, "DELETE", u, nil, nil)
}

// filterMessages applies content filtering to messages before uploading:
//   - Discards entire batch if any user message is trivial (hello, 你好, etc.)
//   - Strips injected <relevant-memories> blocks (prevents memory recursion)
//   - Filters assistant-side system noise prefixes
//   - Truncates by message count and byte budget (from tail, keeping newest)
func (b *Mem9Backend) filterMessages(msgs []Message, filter ContentFilter) []Message {
	// Reject batch if any user message is trivial
	for _, m := range msgs {
		if m.Role == "user" && isTrivialUserMessage(m.Content) {
			return nil
		}
	}

	var filtered []Message

	for _, m := range msgs {
		// Strip noise blocks (e.g. <system-reminder>...</system-reminder>) and
		// injected memory tags to prevent recursive memory storage
		m.Content = stripNoiseTags(m.Content)
		m.Content = stripMemoriesTag(m.Content)

		if m.Content == "" {
			continue
		}

		filtered = append(filtered, m)
	}

	// Truncate by byte budget from tail (keep newest messages)
	var totalBytes int
	var result []Message
	for i := len(filtered) - 1; i >= 0 && len(result) < filter.MaxMessages; i-- {
		size := len(filtered[i].Content)
		if len(result) > 0 && totalBytes+size > filter.MaxBytes {
			break
		}
		result = append([]Message{filtered[i]}, result...)
		totalBytes += size
	}

	return result
}

// isTrivialUserMessage returns true if the user message is a trivial
// greeting/test noise that should not be persisted to memory.
func isTrivialUserMessage(content string) bool {
	s := strings.TrimSpace(content)
	if s == "" {
		return true
	}
	lower := strings.ToLower(s)
	switch lower {
	case "hello", "helo", "hi", "hey", "heyy", "heyyy",
		"yo", "sup", "hola",
		"你好", "哈喽", "在吗", "在？", "在?",
		"测试", "test", "ceshi", "试用", "试试":
		return true
	}
	return false
}

// doRequest makes an HTTP request to the mem9 API with standard headers.
// The caller is responsible for setting a deadline on ctx.
func (b *Mem9Backend) doRequest(ctx context.Context, method, path string, body, result any) error {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("mem9: marshal request: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, b.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("mem9: create request: %w", err)
	}
	req.Header.Set("X-API-Key", b.apiKey)
	req.Header.Set("X-Mnemo-Agent-Id", b.agentID)
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.http.Do(req)
	if err != nil {
		return fmt.Errorf("mem9: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("mem9: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	debuglog.Log(ctx, "mem9: %s %s -> %d", method, path, resp.StatusCode)

	if result != nil {
		return json.NewDecoder(resp.Body).Decode(result)
	}
	return nil
}

// noiseTags defines XML-like block tags that should be stripped from messages
// before storage. These are system-injected blocks (e.g. <system-reminder>)
// prepended to user messages that are not meaningful for memory recall.
var noiseTags = []string{
	"<local-command-caveat>",
	"<local-command-stdout>",
	"<command-name>",
	"<command-message>",
	"<task-notification>",
	"<system-reminder>",
	"<available-skills>",
	"<available-deferred-tools>",
}

// stripNoiseTags removes noise block tags and their content from s.
// Each tag is expected to appear as a paired block (<tag>...</tag>).
// The closing tag is derived by inserting "/" after the leading "<".
func stripNoiseTags(s string) string {
	for _, tag := range noiseTags {
		endTag := tag[:1] + "/" + tag[1:]
		for {
			start := strings.Index(s, tag)
			if start == -1 {
				break
			}
			end := strings.Index(s[start:], endTag)
			if end == -1 {
				// Unmatched opening tag, remove from start to end
				s = strings.TrimSpace(s[:start])
				break
			}
			s = strings.TrimSpace(s[:start] + s[start+end+len(endTag):])
		}
	}
	return s
}

// stripMemoriesTag removes <relevant-memories>...</relevant-memories> blocks
// from content to prevent recursive memory storage.
func stripMemoriesTag(content string) string {
	startTag := "<relevant-memories>"
	endTag := "</relevant-memories>"
	for {
		start := strings.Index(content, startTag)
		if start == -1 {
			break
		}
		end := strings.Index(content[start:], endTag)
		if end == -1 {
			content = content[:start]
			break
		}
		content = content[:start] + content[start+end+len(endTag):]
	}
	return strings.TrimSpace(content)
}

// truncateStr truncates s to at most maxLen characters (runes), appending
// "..." if trimmed. Unlike byte-based slicing, this handles multi-byte
// UTF-8 text (e.g. Chinese, emoji) correctly: 120 runes ≈ 120 characters
// regardless of encoding.
func truncateStr(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}