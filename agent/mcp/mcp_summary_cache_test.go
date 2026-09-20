package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSummarizerProvider answers the summary call without a model.
type fakeSummarizerProvider struct {
	text   string
	err    error
	calls  int
	prompt string
	model  string
}

func (f *fakeSummarizerProvider) CreateChat(_ context.Context, msgs []llm.Message, _ []llm.Tool, _ llm.ChatOptions) (*llm.Response, error) {
	f.calls++
	for _, m := range msgs {
		if m.Role == "user" {
			f.prompt = m.Content
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	return &llm.Response{Content: f.text}, nil
}
func (f *fakeSummarizerProvider) Name() string         { return "fake" }
func (f *fakeSummarizerProvider) ProviderName() string { return "fake" }
func (f *fakeSummarizerProvider) Model() string        { return f.model }
func (f *fakeSummarizerProvider) CreateChatStream(context.Context, []llm.Message, []llm.Tool, llm.ChatOptions) (<-chan llm.StreamEvent, error) {
	return nil, errors.New("not used")
}

// testGenerator is a generator over a throwaway cache directory.
func testGenerator(t *testing.T, provider llm.Provider) *SummaryGenerator {
	t.Helper()
	return &SummaryGenerator{
		Provider: func() llm.Provider { return provider },
		Cache:    NewSummaryCache(t.TempDir()),
	}
}

func summaryTools(names ...string) []*DeferredTool {
	out := make([]*DeferredTool, 0, len(names))
	for _, n := range names {
		tool := testDeferredTool(n, "srv", "does something useful")
		tool.SearchHint = "srv, " + n
		out = append(out, tool)
	}
	return out
}

// The first call generates and stores; the next one is served from disk.
func TestSummaryGeneratorGeneratesThenCaches(t *testing.T) {
	provider := &fakeSummarizerProvider{text: "股票与指数数据（共 1 个工具）", model: "test-model"}
	gen := testGenerator(t, provider)
	tools := summaryTools("mcp__srv__query")

	// A cold cache is one call, waited for: the reminder renders the real
	// summary on its first message.
	text, err := gen.Generate("srv", tools)
	require.NoError(t, err)
	assert.Equal(t, "股票与指数数据（共 1 个工具）", text)
	assert.Equal(t, 1, provider.calls)

	// The next message reads what the background call stored.
	text, err = gen.Generate("srv", tools)
	require.NoError(t, err)
	assert.Equal(t, "股票与指数数据（共 1 个工具）", text)
	assert.Equal(t, 1, provider.calls, "a cached summary must not call the model again")

	// And it survives a restart: a fresh generator over the same directory.
	restarted := &SummaryGenerator{Provider: func() llm.Provider { return provider }, Cache: gen.Cache}
	text, err = restarted.Generate("srv", tools)
	require.NoError(t, err)
	assert.Equal(t, "股票与指数数据（共 1 个工具）", text)
	assert.Equal(t, 1, provider.calls)
}

// The key is the tool SET: an edit, an addition or a rename invalidates; the
// same set in a different order does not.
func TestSummaryKeyFollowsTheTools(t *testing.T) {
	tools := summaryTools("mcp__srv__query", "mcp__srv__list")
	same := summaryTools("mcp__srv__list", "mcp__srv__query")
	assert.Equal(t, SummaryKey(tools), SummaryKey(same), "order must not matter")

	edited := summaryTools("mcp__srv__query", "mcp__srv__list")
	edited[0].Description = "does something else"
	assert.NotEqual(t, SummaryKey(tools), SummaryKey(edited), "an edited description is a different tool set")

	added := append(summaryTools("mcp__srv__query", "mcp__srv__list"), summaryTools("mcp__srv__create")...)
	assert.NotEqual(t, SummaryKey(tools), SummaryKey(added))
}

// A tool set that already failed is not retried on every message: the call is
// waited for, so a failing provider would put its timeout in front of every
// message forever.
func TestSummaryGeneratorDoesNotRetryAFailedToolSet(t *testing.T) {
	provider := &fakeSummarizerProvider{err: errors.New("provider down")}
	gen := testGenerator(t, provider)
	tools := summaryTools("mcp__srv__query")

	// The first attempt fails and says so; the caller falls back to the heuristic.
	text, err := gen.Generate("srv", tools)
	require.Error(t, err)
	assert.Empty(t, text)
	// Every later message gets the quiet answer instead: this tool set is known
	// bad, and asking again would put the same timeout in front of each of them.
	for range 2 {
		text, err := gen.Generate("srv", tools)
		require.NoError(t, err)
		assert.Empty(t, text)
	}
	assert.Equal(t, 1, provider.calls, "give up on this tool set, not on the provider")

	// A changed tool set is a new question, so it is asked again.
	gen.Generate("srv", summaryTools("mcp__srv__query", "mcp__srv__other"))
	assert.Equal(t, 2, provider.calls)
}

// The same tool set in two servers is two summaries: the files are per server.
func TestSummaryCacheIsPerServer(t *testing.T) {
	dir := t.TempDir()
	cache := NewSummaryCache(dir)
	require.NoError(t, cache.Store("alpha", "h1", "A 的摘要", "m", 3))

	if text, ok := cache.Load("alpha", "h1"); !ok || text != "A 的摘要" {
		t.Errorf("alpha = %q, ok = %v", text, ok)
	}
	if _, ok := cache.Load("beta", "h1"); ok {
		t.Error("beta must not read alpha's summary")
	}
	if _, ok := cache.Load("alpha", "h2"); ok {
		t.Error("a different tool-set hash must miss")
	}
}

// Anything unreadable is a miss, never an error and never a panic: the fallback
// is a thinner reminder, which is not worth failing a turn over.
func TestSummaryCacheSurvivesGarbage(t *testing.T) {
	dir := t.TempDir()
	cache := NewSummaryCache(dir)
	require.NoError(t, cache.Store("srv", "h1", "text", "m", 1))

	path := filepath.Join(dir, "srv.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))
	if _, ok := cache.Load("srv", "h1"); ok {
		t.Error("corrupt file must miss")
	}
	require.NoError(t, os.WriteFile(path, []byte(`{"hash":"h1","text":"   "}`), 0o600))
	if _, ok := cache.Load("srv", "h1"); ok {
		t.Error("blank text must miss")
	}

	empty := NewSummaryCache("")
	if _, ok := empty.Load("srv", "h1"); ok {
		t.Error("a cache with no directory must always miss")
	}
	assert.NoError(t, empty.Store("srv", "h1", "text", "m", 1), "no dir configured is not an error")
}

// A server name comes from mcp.json: a separator in it must not let a write
// escape the cache directory.
func TestSummaryCacheSanitizesTheServerName(t *testing.T) {
	dir := t.TempDir()
	cache := NewSummaryCache(dir)
	require.NoError(t, cache.Store("../../etc/passwd", "h1", "text", "m", 1))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.NotContains(t, entries[0].Name(), "/")
	assert.True(t, strings.HasSuffix(entries[0].Name(), ".json"))
}

// The prompt shows the model what it is describing and tells it what not to do.
func TestSummaryPromptShape(t *testing.T) {
	tools := summaryTools("mcp__srv__query", "mcp__srv__list")
	prompt := summaryPrompt("srv", tools)
	assert.Contains(t, prompt, "mcp__srv__query")
	assert.Contains(t, prompt, "does something useful")
	assert.Contains(t, prompt, "N = 2")
	assert.Contains(t, summarySystemPrompt, "Do NOT enumerate")
}

// A generation that answers nothing usable must not be cached as a summary.
func TestSummaryGeneratorRejectsBlankResponses(t *testing.T) {
	provider := &fakeSummarizerProvider{text: "   \n  "}
	gen := testGenerator(t, provider)
	tools := summaryTools("mcp__srv__query")

	text, err := gen.Generate("srv", tools)
	require.Error(t, err, "an empty answer is a failure, not a summary")
	assert.Empty(t, text)
	if _, ok := gen.Cache.Load("srv", SummaryKey(tools)); ok {
		t.Error("a blank response must not be stored")
	}
}
