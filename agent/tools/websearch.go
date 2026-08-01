package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/proxy"
)

type SearchResult struct {
	Title       string `json:"title"`
	Link        string `json:"link"`
	Snippet     string `json:"snippet"`
	DisplayLink string `json:"displayLink,omitempty"`
}

type WebSearchResult struct {
	Query        string         `json:"query"`
	NumResults   int            `json:"numResults"`
	Results      []SearchResult `json:"results"`
	DurationMs   int64          `json:"durationMs"`
	Provider     string         `json:"provider"`
	ErrorMessage string         `json:"error,omitempty"`
}

type webSearchArgs struct {
	Query string `json:"query"`
	Num   *int   `json:"num"`
}

type WebSearchTool struct {
	ProviderType string
	APIKey       string
	Timeout      time.Duration
	MaxResults   int
	Proxy        string // Optional proxy URL (e.g. socks5://127.0.0.1:1080)

	getClient func() *http.Client // lazily initialized via sync.OnceValue
}

// getHTTPClient returns a cached *http.Client. The client is built once
// (lazily) and reused across every search call, preserving connection
// pooling and proxy connections.
func (t *WebSearchTool) getHTTPClient() *http.Client {
	if t.getClient == nil {
		t.getClient = sync.OnceValue(func() *http.Client {
			c, err := proxy.NewHTTPClient(t.Proxy, t.Timeout)
			if err != nil {
				return &http.Client{Timeout: t.Timeout}
			}
			return c
		})
	}
	return t.getClient()
}

func (t *WebSearchTool) Name() string { return ToolNameWebSearch }
func (t *WebSearchTool) Description() string {
	return "Performs a web search. Returns search results including titles, links, and snippets."
}
func (t *WebSearchTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"query": {Type: "string", Description: "The search query to execute"},
		"num":   {Type: "integer", Description: "Number of results to return (default: 5, max: 10)"},
	}
}
func (t *WebSearchTool) Required() []string { return []string{"query"} }
func (t *WebSearchTool) Parallel() bool     { return true }

func (t *WebSearchTool) ExecuteContext(ctx context.Context, args string) (string, error) {
	var a webSearchArgs
	if err := parseArgs(args, &a); err != nil {
		return "", err
	}

	if strings.TrimSpace(a.Query) == "" {
		return "", fmt.Errorf("query is required")
	}

	numResults := 5
	if a.Num != nil && *a.Num > 0 {
		numResults = min(*a.Num, t.MaxResults)
	}

	start := time.Now()

	providerType, apiKey := t.ResolveProvider()
	if apiKey == "" {
		return "", fmt.Errorf("no search provider API key configured. Set web_search.key in %s, or set BRAVE_API_KEY environment variable", filepath.Join(config.BaseDir(), "config.yaml"))
	}

	client := t.getHTTPClient()

	var result *WebSearchResult
	switch providerType {
	case "brave":
		result = t.searchWithBrave(ctx, client, a.Query, numResults, apiKey)
	default:
		return "", fmt.Errorf("unsupported web search provider: %s", providerType)
	}

	result.DurationMs = time.Since(start).Milliseconds()
	result.Provider = providerType

	return marshalResult(result)
}

func (t *WebSearchTool) ResolveProvider() (providerType, apiKey string) {
	if t.APIKey != "" && t.ProviderType != "" {
		return t.ProviderType, t.APIKey
	}
	if key := os.Getenv("BRAVE_API_KEY"); key != "" {
		return "brave", key
	}
	return "", ""
}

func (t *WebSearchTool) searchWithBrave(ctx context.Context, client *http.Client, query string, num int, apiKey string) *WebSearchResult {
	result := &WebSearchResult{
		Query: query,
	}

	baseURL := "https://api.search.brave.com/res/v1/web/search"
	params := url.Values{}
	params.Set("q", query)
	params.Set("count", fmt.Sprintf("%d", min(num, 20))) // Brave allows up to 20 results per request
	params.Set("offset", "0")
	params.Set("mkt", "en-US")

	// Merge with provided context if it's cancellable
	var cancelFn context.CancelFunc
	if ctx == nil {
		ctx, cancelFn = context.WithTimeout(context.Background(), t.Timeout)
	} else {
		ctx, cancelFn = context.WithTimeout(ctx, t.Timeout)
	}
	defer cancelFn()

	reqURL := fmt.Sprintf("%s?%s", baseURL, params.Encode())
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		result.ErrorMessage = fmt.Sprintf("failed to create request: %v", err)
		return result
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", apiKey)

	resp, err := client.Do(req)
	if err != nil {
		result.ErrorMessage = fmt.Sprintf("request failed: %v", err)
		return result
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		result.ErrorMessage = fmt.Sprintf("failed to read response: %v", err)
		return result
	}

	if resp.StatusCode != http.StatusOK {
		result.ErrorMessage = fmt.Sprintf("API error (status %d): %s", resp.StatusCode, string(body))
		return result
	}

	var braveResp struct {
		Query struct {
			Original string `json:"original"`
		} `json:"query"`
		Web struct {
			Results []struct {
				Title string `json:"title"`
				URL   string `json:"url"`
				Desc  string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}

	if err := json.Unmarshal(body, &braveResp); err != nil {
		result.ErrorMessage = fmt.Sprintf("failed to parse response: %v", err)
		return result
	}

	results := make([]SearchResult, 0, len(braveResp.Web.Results))
	for _, item := range braveResp.Web.Results {
		results = append(results, SearchResult{
			Title:   item.Title,
			Link:    item.URL,
			Snippet: item.Desc,
		})
	}

	result.Results = results
	result.NumResults = len(results)
	return result
}
