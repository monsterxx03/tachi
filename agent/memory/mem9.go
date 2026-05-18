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
// Based on mem9 Claude Code plugin's transcript-parser.mjs design.
var scopeFilters = map[StoreScope]ContentFilter{
	StoreScopeTurn:    {MaxMessages: 4, MaxBytes: 20 * 1024},
	StoreScopeCompact: {MaxMessages: 12, MaxBytes: 120 * 1024},
	StoreScopeSession: {MaxMessages: 4, MaxBytes: 20 * 1024},
}

// Store writes memory to mem9 API. The scope determines which messages are
// sent and how they are filtered. mem9 API deduplicates by session_id, so
// repeated writes to the same session merge rather than duplicate.
func (b *Mem9Backend) Store(ctx context.Context, opts StoreOptions) error {
	// Select message source based on scope
	var messages []Message
	switch opts.Scope {
	case StoreScopeTurn:
		messages = opts.TurnMessages
	case StoreScopeCompact:
		messages = opts.SessionMessages
	case StoreScopeSession:
		messages = opts.TurnMessages
		if len(messages) == 0 {
			// Fall back to last 4 messages from the session
			if len(opts.SessionMessages) > 4 {
				messages = opts.SessionMessages[len(opts.SessionMessages)-4:]
			} else {
				messages = opts.SessionMessages
			}
		}
	}

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
			CreatedAt int64    `json:"created_at"`
		} `json:"memories"`
	}

	if err := b.doRequest(ctx, "GET", u, nil, &result); err != nil {
		return nil, err
	}

	entries := make([]Entry, 0, len(result.Memories))
	for _, m := range result.Memories {
		entries = append(entries, Entry{
			ID:        m.ID,
			SessionID: m.SessionID,
			Summary:   truncateStr(m.Content, 80),
			Content:   m.Content,
			Tags:      m.Tags,
			Score:     m.Score,
			Timestamp: m.CreatedAt,
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
//   - Strips injected <relevant-memories> blocks (prevents memory recursion)
//   - Filters assistant-side system noise prefixes
//   - Truncates by message count and byte budget (from tail, keeping newest)
func (b *Mem9Backend) filterMessages(msgs []Message, filter ContentFilter) []Message {
	var filtered []Message

	for _, m := range msgs {
		if m.Role == "assistant" && isNoiseContent(m.Content) {
			continue
		}

		// Strip injected memory tags to prevent recursive memory storage
		m.Content = stripMemoriesTag(m.Content)

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

	if result != nil {
		return json.NewDecoder(resp.Body).Decode(result)
	}
	return nil
}

// Noise prefixes from mem9's transcript-parser.mjs that should be filtered
// from assistant-side content before storing.
var noisePrefixes = []string{
	"<local-command-caveat>",
	"<local-command-stdout>",
	"<command-name>",
	"<command-message>",
	"<task-notification>",
	"<system-reminder>",
}

// isNoiseContent checks if assistant content starts with a known noise prefix.
func isNoiseContent(content string) bool {
	trimmed := strings.TrimSpace(content)
	for _, prefix := range noisePrefixes {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
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

// truncateStr truncates s to at most maxLen characters, appending "..." if trimmed.
func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}