package deepresearch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/pkg/strutil"
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

func (m *mockProvider) Name() string         { return "mock" }
func (m *mockProvider) Model() string        { return "mock-model" }
func (m *mockProvider) Close()               {}
func (m *mockProvider) SetAPIKey(_ string)   {}
func (m *mockProvider) ContextWindow() int64 { return 128000 }

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

// mockProviderFail always fails CreateChat — used to exercise the engine's
// report-generation error path.
type mockProviderFail struct{ mockProvider }

func (m *mockProviderFail) CreateChat(_ context.Context, _ []llm.Message, _ []llm.Tool, _ llm.ChatOptions) (*llm.Response, error) {
	return nil, fmt.Errorf("mock provider failure")
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
	logger := logger.Default()

	engine := New(&cfg.DeepResearch, cfg.Providers, mockProv, runner, logger, cfg.MaxTokens)
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
			got := strutil.Truncate(tt.input, tt.maxLen)
			if got != tt.want {
				t.Errorf("strutil.Truncate() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildPartialReport(t *testing.T) {
	cfg := testConfig()
	engine := New(&cfg.DeepResearch, cfg.Providers, &mockProvider{}, &mockSubagentRunner{}, nil, cfg.MaxTokens)

	report := engine.buildPartialReport("test topic", []string{"learning 1", "learning 2"}, []string{"https://example.com"}, nil)

	if report == "" {
		t.Fatal("expected non-empty report")
	}

	// Should contain topic
	if !strings.Contains(report, "test topic") {
		t.Errorf("report should contain topic, got: %s", report)
	}

	// Should contain learnings
	if !strings.Contains(report, "learning 1") {
		t.Errorf("report should contain learnings, got: %s", report)
	}

	// Should contain Sources section
	if !strings.Contains(report, "Sources") {
		t.Errorf("report should contain Sources section, got: %s", report)
	}
}

func TestBuildPartialReportWithError(t *testing.T) {
	cfg := testConfig()
	engine := New(&cfg.DeepResearch, cfg.Providers, &mockProvider{}, &mockSubagentRunner{}, nil, cfg.MaxTokens)

	err := fmt.Errorf("test error")
	report := engine.buildPartialReport("test", []string{"learning"}, nil, err)

	if !strings.Contains(report, "interrupted") {
		t.Errorf("report should mention interruption, got: %s", report)
	}
}

func TestBuildPartialReportNoLearnings(t *testing.T) {
	cfg := testConfig()
	engine := New(&cfg.DeepResearch, cfg.Providers, &mockProvider{}, &mockSubagentRunner{}, nil, cfg.MaxTokens)

	report := engine.buildPartialReport("test", nil, nil, nil)

	if !strings.Contains(report, "No findings") {
		t.Errorf("report should mention no findings, got: %s", report)
	}
}

// TestWriteReport_SavesHTMLAndReturnsSummary verifies the report writer
// generates HTML via a direct LLM call, the engine writes it to outputPath
// itself, and the returned summary references the saved file — the announced
// path must always match what is actually on disk.
func TestWriteReport_SavesHTMLAndReturnsSummary(t *testing.T) {
	cfg := testConfig()
	prov := &mockProvider{responses: map[string]string{
		"Write the research report for: test topic": "<!DOCTYPE html><html><head><title>R</title></head><body><h1>Report</h1></body></html>",
	}}
	engine := New(&cfg.DeepResearch, cfg.Providers, prov, &mockSubagentRunner{}, nil, cfg.MaxTokens)

	outputPath := filepath.Join(t.TempDir(), "2026-08-02_1932-test-topic.html")
	summary, err := engine.writeReport(context.Background(), "test topic",
		[]string{"learning 1", "learning 2"}, []string{"https://example.com"}, outputPath)
	if err != nil {
		t.Fatalf("writeReport: %v", err)
	}

	// The file must exist on disk at the announced path.
	data, readErr := os.ReadFile(outputPath)
	if readErr != nil {
		t.Fatalf("report file not found at outputPath: %v", readErr)
	}
	if !strings.Contains(string(data), "<html>") {
		t.Errorf("file content should be HTML, got: %s", data)
	}

	// Summary must be concise and reference the file path.
	if !strings.Contains(summary, outputPath) {
		t.Errorf("summary should contain output path, got: %q", summary)
	}
	if strings.Contains(summary, "<html>") {
		t.Errorf("summary must not contain raw HTML, got: %q", summary)
	}
}

// TestWriteReport_EmptyLearningsStillSavesFile verifies that a research run
// with no learnings still produces a partial report file on disk instead of
// announcing a save that never happened.
func TestWriteReport_EmptyLearningsStillSavesFile(t *testing.T) {
	cfg := testConfig()
	engine := New(&cfg.DeepResearch, cfg.Providers, &mockProvider{}, &mockSubagentRunner{}, nil, cfg.MaxTokens)

	outputPath := filepath.Join(t.TempDir(), "empty.html")
	report, err := engine.writeReport(context.Background(), "empty topic", nil, nil, outputPath)
	if err != nil {
		t.Fatalf("writeReport: %v", err)
	}

	if _, readErr := os.Stat(outputPath); readErr != nil {
		t.Fatalf("partial report file not saved: %v", readErr)
	}
	if !strings.Contains(report, "No findings") {
		t.Errorf("report should mention no findings, got: %q", report)
	}
}

// TestWriteReport_LLMFailureReturnsError verifies a report-generation LLM
// failure surfaces as an error so Run() falls back to a partial report
// instead of announcing a bogus save.
func TestWriteReport_LLMFailureReturnsError(t *testing.T) {
	cfg := testConfig()
	prov := &mockProviderFail{}
	engine := New(&cfg.DeepResearch, cfg.Providers, prov, &mockSubagentRunner{}, nil, cfg.MaxTokens)

	outputPath := filepath.Join(t.TempDir(), "fail.html")
	_, err := engine.writeReport(context.Background(), "topic",
		[]string{"learning"}, []string{"https://example.com"}, outputPath)
	if err == nil {
		t.Fatal("expected error from failing LLM provider")
	}
}

// TestBuildReportSummary verifies the summary format.
func TestBuildReportSummary(t *testing.T) {
	summary := buildReportSummary("topic", []string{"a", "b"}, []string{"u1"}, "/tmp/r.html")
	if !strings.Contains(summary, "topic") || !strings.Contains(summary, "2 条") ||
		!strings.Contains(summary, "1 个") || !strings.Contains(summary, "/tmp/r.html") {
		t.Errorf("unexpected summary: %q", summary)
	}
}

// TestReportPath_SetByRun verifies Run() records the output path so callers
// can register the report as a session artifact.
func TestReportPath_SetByRun(t *testing.T) {
	cfg := testConfig()
	prov := &mockProvider{responses: map[string]string{
		"Write the research report for: t": "<html><body>ok</body></html>",
	}}
	engine := New(&cfg.DeepResearch, cfg.Providers, prov, &mockSubagentRunner{}, nil, cfg.MaxTokens)

	if engine.ReportPath() != "" {
		t.Fatalf("ReportPath before Run should be empty, got %q", engine.ReportPath())
	}

	// Redirect the research dir so the report lands in a temp dir.
	researchDir := t.TempDir()
	config.SetBaseDir(researchDir)
	t.Cleanup(func() { config.SetBaseDir("") })

	_, err := engine.Run(context.Background(), "t", 1, 1, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	p := engine.ReportPath()
	if p == "" {
		t.Fatal("ReportPath empty after Run")
	}
	if _, statErr := os.Stat(p); statErr != nil {
		t.Fatalf("reported path does not exist on disk: %v", statErr)
	}
}

// ---- helpers ----
