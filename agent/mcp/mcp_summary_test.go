package mcp

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// summaryPool is two servers whose tools carry the fields the summarizer reads.
func summaryPool() *DeferredPool {
	p := NewDeferredPool()
	for _, t := range []struct{ name, server, desc string }{
		{"mcp__lixinger__get_company_fundamental", "lixinger", "获取 A 股公司基本面指标数据"},
		{"mcp__lixinger__get_index_constituents", "lixinger", "获取 A 股指数样本信息（成分股列表）"},
		{"mcp__lixinger__list_companies", "lixinger", "获取 A 股公司详细信息列表"},
		{"mcp__chrome-devtools__navigate_page", "chrome-devtools", "Go to a URL"},
		{"mcp__chrome-devtools__take_snapshot", "chrome-devtools", "Take a text snapshot of the target page"},
	} {
		tool := testDeferredTool(t.name, t.server, t.desc)
		tool.SearchHint = t.server + ", " + t.name
		p.Add(tool)
	}
	return p
}

func summaryByServer(t *testing.T, sum *ServerSummarizer) map[string]ServerSummary {
	t.Helper()
	out := map[string]ServerSummary{}
	for _, s := range sum.Summaries() {
		out[s.Server] = s
	}
	return out
}

// A server that describes itself is not described by anything else: its own
// words are free, current, and written by whoever knows.
func TestServerSummarizerPrefersServerInstructions(t *testing.T) {
	sum := &ServerSummarizer{
		Pool: summaryPool(),
		Instructions: func(server string) string {
			return "## 理杏仁 MCP Server\n\n提供 A 股、港股、美股等市场的数据查询能力。\n- stockCodes 每次最多 100 个\n"
		},
		Generate: func(string, []*DeferredTool) (string, error) {
			t.Error("a server with its own description must not be summarized again")
			return "", nil
		},
	}

	got := summaryByServer(t, sum)
	require.Contains(t, got, "lixinger")
	assert.Equal(t, ServerInstructionsSource, got["lixinger"].Source)
	// One line, markdown flattened, the caveat kept.
	assert.Equal(t, "## 理杏仁 MCP Server 提供 A 股、港股、美股等市场的数据查询能力。 - stockCodes 每次最多 100 个", got["lixinger"].Text)
	assert.NotContains(t, got["lixinger"].Text, "\n")
}

// A generated summary is used when the server said nothing about itself, and is
// bounded like everything else.
func TestServerSummarizerUsesGeneratedSummary(t *testing.T) {
	sum := &ServerSummarizer{
		Pool: summaryPool(),
		Generate: func(server string, tools []*DeferredTool) (string, error) {
			if server == "lixinger" {
				return "A股/港股/美股的公司与指数数据\n", nil
			}
			return "", errors.New("provider down")
		},
	}

	got := summaryByServer(t, sum)
	assert.Equal(t, GeneratedSource, got["lixinger"].Source)
	assert.Equal(t, "A股/港股/美股的公司与指数数据", got["lixinger"].Text)

	// The server whose generation failed still gets a line: a thin one beats a
	// missing one, and the reminder has no way to retry mid-message.
	assert.Equal(t, HeuristicSource, got["chrome-devtools"].Source)
	assert.Contains(t, got["chrome-devtools"].Text, "snapshot")
}

// The layers compose in the order the reminder needs them: with a generator
// wired but nothing cached yet, the reader gets the heuristic immediately and
// the generated summary from the next message on.
func TestServerSummarizerGeneratesOnAColdCache(t *testing.T) {
	provider := &fakeSummarizerProvider{text: "股票与指数数据（共 3 个工具）", model: "test-model"}
	sum := &ServerSummarizer{Pool: summaryPool(), Generate: testGenerator(t, provider).Generate}

	// The generated layer answers on the FIRST render: one call per tool set is
	// worth waiting for, and a placeholder that repairs itself a message later
	// leaves the reader with a reminder it cannot use yet.
	got := summaryByServer(t, sum)["lixinger"]
	assert.Equal(t, GeneratedSource, got.Source)
	assert.Equal(t, "股票与指数数据（共 3 个工具）", got.Text)
	// Both servers in the fixture lack instructions, so both were generated —
	// once each, for this tool set.
	assert.Equal(t, 2, provider.calls)

	// A server with its own description never reaches the generator, and the
	// other one is now a cache hit: rendering the same pool again calls nothing.
	sum.Instructions = func(server string) string {
		if server == "lixinger" {
			return "理杏仁自己写的说明"
		}
		return ""
	}
	got = summaryByServer(t, sum)["lixinger"]
	assert.Equal(t, ServerInstructionsSource, got.Source)
	assert.Equal(t, "理杏仁自己写的说明", got.Text)
	assert.Equal(t, 2, provider.calls, "instructions win, caches hold, nothing is called")
}

// With nothing to generate from, the tool names are the description — the only
// layer that cannot fail.
func TestServerSummarizerHeuristicFromToolNames(t *testing.T) {
	sum := &ServerSummarizer{Pool: summaryPool()}

	got := summaryByServer(t, sum)
	require.Len(t, got, 2)
	dev := got["chrome-devtools"]
	assert.Equal(t, HeuristicSource, dev.Source)
	// The words the names repeat, most repeated first; the server's own name is
	// not a keyword (it is already on the line).
	assert.NotContains(t, dev.Text, "chrome")
	assert.NotContains(t, dev.Text, "devtools")
	for _, want := range []string{"page", "snapshot", "take"} {
		assert.Contains(t, dev.Text, want)
	}
	assert.LessOrEqual(t, len(strings.Split(dev.Text, ", ")), heuristicKeywordLimit)
}

// The same pool must produce the same line twice: a generated summary's cache
// key and the reader's eyes both depend on it.
func TestServerSummarizerIsStable(t *testing.T) {
	sum := &ServerSummarizer{Pool: summaryPool()}
	first := sum.Summaries()
	second := sum.Summaries()
	assert.Equal(t, first, second)
}

func TestServerSummarizerEmptyPool(t *testing.T) {
	assert.Empty(t, (&ServerSummarizer{Pool: NewDeferredPool()}).Summaries())
	assert.Empty(t, (&ServerSummarizer{}).Summaries())
}

// A description too long for a reminder line is bounded, not dropped.
func TestServerSummarizerBoundsTheText(t *testing.T) {
	sum := &ServerSummarizer{
		Pool:         summaryPool(),
		Instructions: func(string) string { return strings.Repeat("很长的说明", 400) },
	}
	got := summaryByServer(t, sum)
	// Truncate bounds RUNES (the unit a CJK character costs a token in) and
	// appends an ellipsis, so the byte length is a poor proxy here.
	assert.LessOrEqual(t, utf8.RuneCountInString(got["lixinger"].Text), summaryTextLimit+3)
	assert.True(t, strings.HasSuffix(got["lixinger"].Text, "..."))
}
