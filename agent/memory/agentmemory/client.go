// Package agentmemory provides an HTTP client for the agentmemory REST API.
//
// agentmemory (https://github.com/rohitg00/agentmemory) is a persistent memory
// server for AI agents. It runs as a standalone process (npx @agentmemory/agentmemory)
// and exposes REST endpoints on port 3111 by default.
//
// This client implements only the subset of endpoints needed by Tachi's
// memory.Backend interface: session management, memory write (remember),
// semantic search (smart-search), and memory deletion (forget).
package agentmemory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DefaultBaseURL is the default agentmemory server address.
const DefaultBaseURL = "http://localhost:3111"

// Client wraps HTTP calls to an agentmemory server.
// All methods accept a context for timeout/cancellation.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient creates a new agentmemory client.
// An empty baseURL defaults to http://localhost:3111.
func NewClient(baseURL string, timeout time.Duration) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Client{
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: timeout},
	}
}

// Health checks whether the agentmemory server is reachable.
func (c *Client) Health(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/agentmemory/health", nil)
	if err != nil {
		return false
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

// StartSession notifies agentmemory that a new session has begun.
// agentmemory expects camelCase JSON fields: sessionId, project, cwd.
func (c *Client) StartSession(ctx context.Context, sessionID, project, cwd string) error {
	body, _ := json.Marshal(map[string]string{
		"sessionId": sessionID,
		"project":   project,
		"cwd":       cwd,
	})
	return c.doPost(ctx, "/agentmemory/session/start", body, nil)
}

// EndSession notifies agentmemory that the current session has ended,
// triggering memory consolidation.
func (c *Client) EndSession(ctx context.Context, sessionID string) error {
	body, _ := json.Marshal(map[string]string{"sessionId": sessionID})
	return c.doPost(ctx, "/agentmemory/session/end", body, nil)
}

// RememberPayload is the request body for storing a memory.
type RememberPayload struct {
	Content   string   `json:"content"`
	Tags      []string `json:"tags,omitempty"`
	SessionID string   `json:"sessionId"`
}

// Remember stores a memory entry in agentmemory.
func (c *Client) Remember(ctx context.Context, p RememberPayload) error {
	body, _ := json.Marshal(p)
	return c.doPost(ctx, "/agentmemory/remember", body, nil)
}

// MemoryEntry is a single memory result returned by SmartSearch.
// agentmemory returns CompactSearchResult format:
//   obsId, sessionId, title, type, score, timestamp
// The title field contains the memory content summary.
type MemoryEntry struct {
	ID        string  `json:"obsId"`
	Title     string  `json:"title"`
	Score     float64 `json:"score"`
	Timestamp string  `json:"timestamp"`
	SessionID string  `json:"sessionId"`
}

// SmartSearch performs hybrid retrieval (BM25 + vector + knowledge graph)
// and returns the top-k matching memories.
func (c *Client) SmartSearch(ctx context.Context, query string, limit int) ([]MemoryEntry, error) {
	if limit <= 0 {
		limit = 5
	}
	body, _ := json.Marshal(map[string]any{"query": query, "limit": limit})

	var result struct {
		Results []MemoryEntry `json:"results"`
	}
	if err := c.doPost(ctx, "/agentmemory/smart-search", body, &result); err != nil {
		return nil, err
	}
	return result.Results, nil
}

// Forget deletes a memory entry by its ID.
// agentmemory expects POST /agentmemory/forget with {"memoryId": id}.
func (c *Client) Forget(ctx context.Context, id string) error {
	body, _ := json.Marshal(map[string]string{"memoryId": id})
	return c.doPost(ctx, "/agentmemory/forget", body, nil)
}

// doPost is a helper that sends a POST request with JSON body and optionally
// decodes the JSON response into result.
func (c *Client) doPost(ctx context.Context, path string, body []byte, result any) error {
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("agentmemory: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	if result != nil {
		return json.NewDecoder(resp.Body).Decode(result)
	}
	return nil
}
