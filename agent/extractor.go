package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
)

const keywordExtractionPrompt = "Extract 3-5 most important search keywords from the user query. " +
	"Return each keyword TWICE — once in Chinese and once in English — one per line. " +
	"Output short phrases (1-3 words each, no full sentences). " +
	"For example: \"数据库连接池\" and \"database connection pool\" should both appear as separate lines. " +
	"No numbering, bullets, explanations, or any other text."

// LLMKeywordExtractor uses an LLM to extract search keywords from a user query.
// It is injected into TopicBackend to improve text-based recall quality
// by converting natural-language queries into targeted keywords for ripgrep.
type LLMKeywordExtractor struct {
	provider llm.Provider
	model    string
	timeout  time.Duration
	logger   *logger.Logger
}

// NewLLMKeywordExtractor creates an extractor backed by an LLM provider.
// timeout defaults to 15s if zero is passed — keyword extraction is best-effort
// and should not significantly delay the conversation flow.
func NewLLMKeywordExtractor(provider llm.Provider, model string, timeout time.Duration) *LLMKeywordExtractor {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &LLMKeywordExtractor{
		provider: provider,
		model:    model,
		timeout:  timeout,
		logger:   logger.New("memory:keyword-extractor"),
	}
}

// ExtractKeywords calls the LLM to extract 3-5 search keywords from the query.
// Returns an error if the LLM call fails, times out, or returns no keywords.
// The caller (TopicBackend.Recall) falls back to the raw query on error.
func (e *LLMKeywordExtractor) ExtractKeywords(ctx context.Context, query string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	messages := []llm.Message{
		{Role: "system", Content: keywordExtractionPrompt},
		{Role: "user", Content: query},
	}

	resp, err := e.provider.CreateChat(ctx, messages, nil, llm.ChatOptions{
		MaxTokens: 4096,
		Thinking:  new(false),
	})
	if err != nil {
		return nil, fmt.Errorf("keyword extraction LLM call: %w", err)
	}

	e.logger.Debug(ctx, fmt.Sprintf("LLM raw response: %q", resp.Content))

	keywords := parseKeywordResponse(resp.Content)
	if len(keywords) == 0 {
		return nil, fmt.Errorf("no keywords extracted from LLM response")
	}

	return keywords, nil
}

// parseKeywordResponse splits the LLM response into individual keywords.
// Handles common LLM formatting artifacts: numbered lists, bullet points,
// explanatory text, and trailing punctuation.
func parseKeywordResponse(content string) []string {
	lines := strings.Split(strings.TrimSpace(content), "\n")
	var keywords []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		// Strip common list prefixes: numbers, bullets, dashes, etc.
		line = strings.TrimLeft(line, "-*•#0123456789. )\t")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Reject lines that look like natural-language explanations.
		if len(line) > 50 {
			continue
		}
		// Reject lines that are meta-commentary about keywords.
		if strings.HasPrefix(strings.ToLower(line), "keywords") ||
			strings.HasPrefix(strings.ToLower(line), "keyword") {
			continue
		}
		keywords = append(keywords, line)
	}
	return keywords
}
