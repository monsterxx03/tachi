package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/pkg/strutil"
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
	// sessionID returns the current session ID for usage-ledger anchoring
	// ("" when session-less). Injected by the agent; nil disables anchoring.
	sessionID func() string
}

// maxKeywordExtractionTimeout bounds the extraction call even when memory.keyword_timeout
// asks for more: extraction is best-effort, and a provider that takes 15 seconds to name five
// keywords has already cost the user more than the keywords are worth.
const maxKeywordExtractionTimeout = 15 * time.Second

// defaultKeywordExtractionTimeout is the budget when memory.keyword_timeout is unset. It has
// to clear the model's own latency, not just the network's: the chat model answers this prompt
// in several seconds, so a shorter cap returns nothing while the turn has already paid the wait.
const defaultKeywordExtractionTimeout = 8 * time.Second

// keywordExtractionBudget resolves the extractor's per-call budget from memory.*.
//
// Two bounds meet here: memory.keyword_timeout, and memory.timeout — the deadline Recall
// itself runs under, since extraction happens inside it. A budget above the recall deadline
// can only ever be cut short by the parent context, and the log would then blame the keyword
// call for a timeout the user set for something else.
func keywordExtractionBudget(mem config.MemoryConfig) time.Duration {
	budget := mem.KeywordTimeout
	if budget <= 0 {
		budget = defaultKeywordExtractionTimeout
	}
	if mem.Timeout > 0 && budget > mem.Timeout {
		budget = mem.Timeout
	}
	return budget
}

// NewLLMKeywordExtractor creates an extractor backed by an LLM provider.
// timeout is the per-call budget; zero means defaultKeywordExtractionTimeout,
// and anything above maxKeywordExtractionTimeout is clamped to it.
func NewLLMKeywordExtractor(provider llm.Provider, model string, timeout time.Duration, l *logger.Logger) *LLMKeywordExtractor {
	if timeout <= 0 {
		timeout = defaultKeywordExtractionTimeout
	}
	if timeout > maxKeywordExtractionTimeout {
		timeout = maxKeywordExtractionTimeout
	}
	return &LLMKeywordExtractor{
		provider: provider,
		model:    model,
		timeout:  timeout,
		logger:   l,
	}
}

// SetSessionIDResolver injects a session-ID provider used to anchor keyword
// extraction calls in the usage ledger to the current session.
func (e *LLMKeywordExtractor) SetSessionIDResolver(fn func() string) {
	e.sessionID = fn
}

// ExtractKeywords calls the LLM to extract 3-5 search keywords from the query.
// Returns an error if the LLM call fails, times out, or returns no keywords.
// The caller (TopicBackend.Recall) falls back to the raw query on error.
func (e *LLMKeywordExtractor) ExtractKeywords(ctx context.Context, query string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	// Usage billing: tag the kind and anchor to the current session so
	// keyword extraction costs land in the session's ledger rows.
	ctx = llm.WithUsageKind(ctx, llm.UsageKindKeyword)
	if e.sessionID != nil {
		if sid := e.sessionID(); sid != "" {
			ctx = llm.WithSessionID(ctx, sid)
		}
	}

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

	e.logger.Debug(ctx, "LLM raw response", "content", resp.Content)

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
	var keywords []string
	for _, line := range strutil.SplitBy(content, "\n") {
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
