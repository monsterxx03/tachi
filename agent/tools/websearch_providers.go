package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/monsterxx03/tachi/config"
)

// ── Brave ────────────────────────────────────────────────────────────────

const braveDefaultBaseURL = "https://api.search.brave.com"

// braveProvider searches via the Brave Search API.
type braveProvider struct {
	apiKey  string
	baseURL string // optional override (default: braveDefaultBaseURL)
}

func (p *braveProvider) Type() string { return config.WebSearchProviderBrave }

func (p *braveProvider) Search(ctx context.Context, client *http.Client, query string, num int) ([]SearchResult, *searchErr) {
	base := p.baseURL
	if base == "" {
		base = braveDefaultBaseURL
	}

	params := url.Values{}
	params.Set("q", query)
	params.Set("count", fmt.Sprintf("%d", min(num, 20))) // Brave allows up to 20 results per request
	params.Set("offset", "0")
	params.Set("mkt", "en-US")

	reqURL := fmt.Sprintf("%s/res/v1/web/search?%s", strings.TrimSuffix(base, "/"), params.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, &searchErr{kind: errOther, message: fmt.Sprintf("failed to create request: %v", err)}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", p.apiKey)

	resp, err := client.Do(req)
	if err != nil {
		return nil, &searchErr{kind: errOther, message: fmt.Sprintf("request failed: %v", err)}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &searchErr{kind: errOther, message: fmt.Sprintf("failed to read response: %v", err)}
	}

	if resp.StatusCode != http.StatusOK {
		return nil, classifyError(resp.StatusCode, string(body), resp.Header)
	}

	var braveResp struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(body, &braveResp); err != nil {
		return nil, &searchErr{kind: errOther, message: fmt.Sprintf("failed to parse response: %v", err)}
	}

	results := make([]SearchResult, 0, len(braveResp.Web.Results))
	for _, item := range braveResp.Web.Results {
		results = append(results, SearchResult{
			Title:   item.Title,
			Link:    item.URL,
			Snippet: item.Description,
		})
	}
	return results, nil
}

// ── Exa ──────────────────────────────────────────────────────────────────

const exaDefaultBaseURL = "https://api.exa.ai"

// exaSnippetMaxChars caps the text snippet returned per result so responses
// stay small (the LLM can WebFetch a page for full content).
const exaSnippetMaxChars = 500

// exaProvider searches via the Exa API (POST /search).
//
// https://exa.ai/docs/reference/search
type exaProvider struct {
	apiKey  string
	baseURL string // optional override (default: exaDefaultBaseURL)
}

func (p *exaProvider) Type() string { return config.WebSearchProviderExa }

func (p *exaProvider) Search(ctx context.Context, client *http.Client, query string, num int) ([]SearchResult, *searchErr) {
	base := p.baseURL
	if base == "" {
		base = exaDefaultBaseURL
	}
	reqURL := strings.TrimSuffix(base, "/") + "/search"

	payload := map[string]any{
		"query":      query,
		"numResults": num,
		"contents": map[string]any{
			"text": map[string]any{"maxCharacters": exaSnippetMaxChars},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, &searchErr{kind: errOther, message: fmt.Sprintf("failed to encode request: %v", err)}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return nil, &searchErr{kind: errOther, message: fmt.Sprintf("failed to create request: %v", err)}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", p.apiKey)

	resp, err := client.Do(req)
	if err != nil {
		return nil, &searchErr{kind: errOther, message: fmt.Sprintf("request failed: %v", err)}
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &searchErr{kind: errOther, message: fmt.Sprintf("failed to read response: %v", err)}
	}

	if resp.StatusCode != http.StatusOK {
		return nil, classifyError(resp.StatusCode, string(respBody), resp.Header)
	}

	var exaResp struct {
		Results []struct {
			Title  string `json:"title"`
			URL    string `json:"url"`
			Text   string `json:"text"`
			Author string `json:"author"`
		} `json:"results"`
	}
	if err := json.Unmarshal(respBody, &exaResp); err != nil {
		return nil, &searchErr{kind: errOther, message: fmt.Sprintf("failed to parse response: %v", err)}
	}

	results := make([]SearchResult, 0, len(exaResp.Results))
	for _, item := range exaResp.Results {
		snippet := strings.TrimSpace(item.Text)
		if snippet == "" && item.Author != "" {
			snippet = "By " + item.Author
		}
		results = append(results, SearchResult{
			Title:   item.Title,
			Link:    item.URL,
			Snippet: snippet,
		})
	}
	return results, nil
}

// classifyError converts a non-200 API response into a *searchErr.
//
// Classification follows the providers' documented error codes:
//
//   - HTTP 402 (Payment Required) → errQuota. Exa documents 402 as credits
//     exhausted / API key or team budget exceeded (tags NO_MORE_CREDITS,
//     API_KEY_BUDGET_EXCEEDED, TEAM_BUDGET_EXCEEDED).
//   - HTTP 429 (Too Many Requests) → errRateLimit, unless the response shows
//     the monthly quota is actually exhausted (Brave's X-RateLimit-Remaining
//     header with a 0 monthly remainder, or explicit credit-exhausted text),
//     in which case → errQuota.
//   - Explicit credit-exhausted wording in the body → errQuota.
//   - Everything else → errOther.
func classifyError(status int, body string, headers http.Header) *searchErr {
	msg := fmt.Sprintf("API error (status %d): %s", status, body)
	switch {
	case status == http.StatusPaymentRequired:
		return &searchErr{kind: errQuota, message: msg}
	case status == http.StatusTooManyRequests:
		if monthlyQuotaExhausted(headers) || quotaExhaustedBody(body) {
			return &searchErr{kind: errQuota, message: msg}
		}
		return &searchErr{kind: errRateLimit, message: msg}
	case quotaExhaustedBody(body):
		return &searchErr{kind: errQuota, message: msg}
	default:
		return &searchErr{kind: errOther, message: msg}
	}
}

// quotaExhaustedBody detects explicit credit/budget exhaustion wording in an
// API error body. Deliberately narrow — a bare "rate limit" or "quota" word
// is NOT enough (e.g. Brave's 429 body embeds quota_limit/quota_current
// metadata even when only the per-second limit was hit).
func quotaExhaustedBody(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, "no more credits") ||
		strings.Contains(lower, "credits exhausted") ||
		strings.Contains(lower, "insufficient credits") ||
		strings.Contains(lower, "budget exceeded")
}

// monthlyQuotaExhausted reports whether the monthly quota window is empty,
// based on Brave's X-RateLimit-Remaining response header:
//
//	X-RateLimit-Remaining: 1, 0   (1 request left this second, 0 this month)
//
// Only the second (per-month) value is inspected; a missing or malformed
// header conservatively returns false so a transient rate limit is not
// mistaken for an exhausted monthly quota.
func monthlyQuotaExhausted(headers http.Header) bool {
	remaining := headers.Get("X-RateLimit-Remaining")
	if remaining == "" {
		return false
	}
	parts := strings.Split(remaining, ",")
	if len(parts) < 2 {
		return false
	}
	n, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return false
	}
	return n <= 0
}
