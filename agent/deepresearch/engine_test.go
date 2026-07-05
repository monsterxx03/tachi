package deepresearch

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/debuglog"
)

// ---- mockSubagentRunner implements SubagentRunner for testing ----

type mockSubagentRunner struct {
	results map[string]string // prompt prefix → result
}

func (m *mockSubagentRunner) Run(_ context.Context, prompt string, _ []string) (string, error) {
	// Check for exact match first
	if r, ok := m.results[prompt]; ok {
		return r, nil
	}
	// Fallback: return a generic result
	return "Test learning: mock research result.\nSource: https://example.com/test", nil
}

// ---- mockProvider implements llm.Provider for testing ----

type mockProvider struct {
	responses map[string]string // prompt prefix → response
}

func (m *mockProvider) Name() string              { return "mock" }
func (m *mockProvider) Model() string              { return "mock-model" }
func (m *mockProvider) Close()                     {}
func (m *mockProvider) SetAPIKey(_ string)        {}
func (m *mockProvider) ContextWindow() int64       { return 128000 }

func (m *mockProvider) CreateChat(_ context.Context, messages []llm.Message, _ []llm.Tool, _ llm.ChatOptions) (*llm.Response, error) {
	// Extract the last user message content
	var lastUserContent string
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			lastUserContent = messages[i].Content
			break
		}
	}

	// Check for exact match
	if r, ok := m.responses[lastUserContent]; ok {
		return &llm.Response{Content: r}, nil
	}

	// Default: return a simple JSON query list
	return &llm.Response{
		Content: `[{"query": "test query 1", "researchGoal": "understand test topic"}]`,
	}, nil
}

func (m *mockProvider) CreateChatStream(_ context.Context, _ []llm.Message, _ []llm.Tool, _ llm.ChatOptions) (<-chan llm.StreamEvent, error) {
	return nil, fmt.Errorf("not implemented")
}

// ---- test config helper ----

func testConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.DeepResearch.DefaultDepth = 1
	cfg.DeepResearch.DefaultBreadth = 2
	cfg.DeepResearch.MaxDepth = 2
	cfg.DeepResearch.MaxBreadth = 4
	cfg.DeepResearch.Timeout = 30 * time.Second
	cfg.DeepResearch.MaxLearnings = 100
	return cfg
}

// ---- Tests ----

func TestNewEngine(t *testing.T) {
	cfg := testConfig()
	mockProv := &mockProvider{}
	runner := &mockSubagentRunner{}
	logger := debuglog.DefaultLogger

	engine := New(&cfg.DeepResearch, cfg.Providers, mockProv, runner, logger)
	if engine == nil {
		t.Fatal("New returned nil")
	}
}

func TestExtractJSONArray(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "plain json array",
			input: `[{"query": "test", "researchGoal": "goal"}]`,
			want:  `[{"query": "test", "researchGoal": "goal"}]`,
		},
		{
			name:  "json in code block",
			input: "```json\n[{\"query\": \"test\"}]\n```",
			want:  `[{"query": "test"}]`,
		},
		{
			name:  "json in code block without language",
			input: "```\n[{\"query\": \"test\"}]\n```",
			want:  `[{"query": "test"}]`,
		},
		{
			name:  "empty input",
			input: "",
			want:  "",
		},
		{
			name:  "no json in text",
			input: "Some plain text without JSON",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractJSONArray(tt.input)
			if got != tt.want {
				t.Errorf("extractJSONArray() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractLearningsAndURLs(t *testing.T) {
	tests := []struct {
		name          string
		output        string
		wantLearnings int
		wantURLCount  int
	}{
		{
			name:          "empty output",
			output:        "",
			wantLearnings: 0,
			wantURLCount:  0,
		},
		{
			name:          "plain text",
			output:        "Some learning content without URLs",
			wantLearnings: 1,
			wantURLCount:  0,
		},
		{
			name:          "text with URLs",
			output:        "Finding: AI models are improving. Source: https://example.com/ai",
			wantLearnings: 1,
			wantURLCount:  1,
		},
		{
			name:          "multiple URLs",
			output:        "Learning 1. Ref: https://example.com/1 and https://example.com/2",
			wantLearnings: 1,
			wantURLCount:  2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			learnings, urls := extractLearningsAndURLs(tt.output)
			if len(learnings) != tt.wantLearnings {
				t.Errorf("extractLearningsAndURLs() learnings = %d, want %d", len(learnings), tt.wantLearnings)
			}
			if len(urls) != tt.wantURLCount {
				t.Errorf("extractLearningsAndURLs() urls = %d, want %d", len(urls), tt.wantURLCount)
			}
		})
	}
}

func TestTruncateText(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		maxLen int
		want   string
	}{
		{"short string", "hello", 10, "hello"},
		{"exact length", "hello", 5, "hello"},
		{"long string", "hello world", 5, "hello..."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateText(tt.input, tt.maxLen)
			if got != tt.want {
				t.Errorf("truncateText() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildPartialReport(t *testing.T) {
	cfg := testConfig()
	engine := New(&cfg.DeepResearch, cfg.Providers, &mockProvider{}, &mockSubagentRunner{}, nil)

	report := engine.buildPartialReport("test topic", []string{"learning 1", "learning 2"}, []string{"https://example.com"}, nil)

	if report == "" {
		t.Fatal("expected non-empty report")
	}

	// Should contain topic
	if !contains(report, "test topic") {
		t.Errorf("report should contain topic, got: %s", report)
	}

	// Should contain learnings
	if !contains(report, "learning 1") {
		t.Errorf("report should contain learnings, got: %s", report)
	}

	// Should contain Sources section
	if !contains(report, "Sources") {
		t.Errorf("report should contain Sources section, got: %s", report)
	}
}

func TestBuildPartialReportWithError(t *testing.T) {
	cfg := testConfig()
	engine := New(&cfg.DeepResearch, cfg.Providers, &mockProvider{}, &mockSubagentRunner{}, nil)

	err := fmt.Errorf("test error")
	report := engine.buildPartialReport("test", []string{"learning"}, nil, err)

	if !contains(report, "interrupted") {
		t.Errorf("report should mention interruption, got: %s", report)
	}
}

func TestBuildPartialReportNoLearnings(t *testing.T) {
	cfg := testConfig()
	engine := New(&cfg.DeepResearch, cfg.Providers, &mockProvider{}, &mockSubagentRunner{}, nil)

	report := engine.buildPartialReport("test", nil, nil, nil)

	if !contains(report, "No findings") {
		t.Errorf("report should mention no findings, got: %s", report)
	}
}

// ---- helpers ----

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
