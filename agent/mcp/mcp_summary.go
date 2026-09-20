package mcp

import (
	"sort"
	"strings"
	"sync"

	"github.com/monsterxx03/tachi/pkg/strutil"
)

// Server summaries: one line per MCP server instead of one line per tool.
//
// The DeferredToolReminder's per-tool catalog costs its full text every time —
// 63 tools is ~4.6k estimated tokens (measured; the estimator runs ~18% low on
// mixed CJK), it sits in the message history for the rest of the session, and
// 200 tools would be ~15k. A server summary is what the reader actually needs
// there: WHICH domains this server covers, so a turn that needs one of them
// knows to search for it. The names and schemas arrive from the search itself.
//
// It only makes sense while the search can read a sentence — i.e. under the Jev
// matcher (agent.agent_jev.go). The keyword matcher matches the names and
// English words in the tools' own text, so it keeps the per-tool catalog, and
// the adapter asks the live config which shape to use (agent_configure.go).
//
// Sources, best first:
//
//  1. the server's own `initialize.instructions` — free, current, written by
//     whoever built the server (many servers leave it empty);
//  2. a generated summary (mcp_summary_cache.go) — one LLM call per tool set,
//     cached on disk, keyed by a hash of what it summarized;
//  3. a local heuristic — server name, tool count, a few words from the tool
//     names. Never fails, costs nothing, says less.
//
// The rendering is always one line: `mcp__server（N 个工具）— <text>`, with the
// text whitespace-collapsed so a multi-line instruction block still reads as
// the bullet it is.
type ServerSummary struct {
	// Server is the MCP server name ("lixinger").
	Server string
	// Text is the summary, whitespace-collapsed, already bounded.
	Text string
	// Source names where Text came from: ServerInstructionsSource,
	// GeneratedSource or HeuristicSource. It is carried so a log line or a
	// future UI can say why a server reads the way it does.
	Source string
}

const (
	// ServerInstructionsSource is the server's own `initialize.instructions`.
	ServerInstructionsSource = "instructions"
	// GeneratedSource is an LLM summary.
	GeneratedSource = "generated"
	// HeuristicSource is the local fallback: no LLM, no server text.
	HeuristicSource = "heuristic"

	// summaryTextLimit bounds the text put on the line. The server's
	// instructions are kept whole up to here — the caveats inside them ("at
	// most 100 codes per call") are exactly what a reader needs — and a
	// generated summary is asked for two sentences, so this only ever trims.
	summaryTextLimit = 600
	// heuristicKeywordLimit is how many tool-name words the fallback shows.
	heuristicKeywordLimit = 8
	// summaryGenerateConcurrency bounds how many servers are summarized at
	// once. A handful of MCP servers is the common case; beyond a few, the
	// provider's rate limit is what would suffer, and the calls would not get
	// any faster.
	summaryGenerateConcurrency = 4
)

// ServerSummarizer builds the one-line summaries for the reminder. The zero
// value is usable and returns heuristics only; wire Instructions and Generate
// to get the better sources.
type ServerSummarizer struct {
	// Pool is the deferred pool: the source of which tools belong to which
	// server. Required.
	Pool *DeferredPool
	// Instructions returns a server's own description (Manager.ServerInstructions).
	// Optional.
	Instructions func(server string) string
	// Generate produces a summary from the server's tools (mcp_summary_cache.go).
	// Optional.
	Generate func(server string, tools []*DeferredTool) (string, error)
}

// Summaries returns one entry per server that still has something to discover,
// ordered by server name. An empty slice means "nothing to summarize" — the
// caller falls back to the per-tool catalog rather than rendering an empty
// block.
func (s *ServerSummarizer) Summaries() []ServerSummary {
	if s.Pool == nil {
		return nil
	}
	byServer := map[string][]*DeferredTool{}
	for _, t := range s.Pool.snapshot() {
		byServer[t.ServerName] = append(byServer[t.ServerName], t)
	}

	// Sorted by server name before anything is generated: the order of the
	// block, the order the calls go out in, and the tool order a cache key is
	// computed from all have to be the same on every render.
	servers := make([]string, 0, len(byServer))
	for server := range byServer {
		servers = append(servers, server)
	}
	sort.Strings(servers)
	for _, server := range servers {
		tools := byServer[server]
		sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	}

	// The cheap layer first, for every server: a server that describes itself
	// never reaches the model at all.
	out := make([]ServerSummary, len(servers))
	var pending []int
	for i, server := range servers {
		text, source := s.instructionsFor(server)
		out[i] = ServerSummary{Server: server, Text: text, Source: source}
		if text == "" {
			pending = append(pending, i) // still needs a summary, generated or local
		}
	}

	// Then the rest, IN PARALLEL: the wait is the slowest call rather than their
	// sum, which for a cold cache is the difference between one provider round
	// trip in front of the message and one per server. Each goroutine writes its
	// own slot, so nothing here needs a lock.
	if len(pending) > 0 {
		var wg sync.WaitGroup
		slots := make(chan struct{}, summaryGenerateConcurrency)
		for _, i := range pending {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				slots <- struct{}{}
				defer func() { <-slots }()

				server, tools := servers[i], byServer[servers[i]]
				if s.Generate != nil {
					if text, err := s.Generate(server, tools); err == nil {
						if text = collapse(text); text != "" {
							out[i].Text, out[i].Source = text, GeneratedSource
							return
						}
					}
				}
				// No generated summary: the tool names are what is left, and
				// they are better than an empty line.
				out[i].Text, out[i].Source = heuristicText(server, tools), HeuristicSource
			}(i)
		}
		wg.Wait()
	}
	return out
}

// instructionsFor is the free layer: the server's own description, or "" when it
// gave none.
func (s *ServerSummarizer) instructionsFor(server string) (string, string) {
	if s.Instructions == nil {
		return "", HeuristicSource
	}
	if text := collapse(s.Instructions(server)); text != "" {
		return text, ServerInstructionsSource
	}
	return "", HeuristicSource
}

// heuristicText describes a server from its tool names alone: the count plus
// the words those names repeat. It says less than anything else here, and it is
// the only layer that cannot fail.
func heuristicText(server string, tools []*DeferredTool) string {
	counts := map[string]int{}
	for _, t := range tools {
		// nameParts is the cached parse a tool built by the pool carries; a tool
		// built by hand (tests, a future caller) has none, so parse its name
		// here rather than describing the server as "nothing".
		parts := t.nameParts
		if len(parts) == 0 {
			parts = parseToolName(t.Name)
		}
		for _, part := range parts {
			if part == "" || part == strings.ToLower(server) || isStopWord(part) {
				continue
			}
			counts[part]++
		}
	}
	type word struct {
		text string
		n    int
	}
	words := make([]word, 0, len(counts))
	for w, n := range counts {
		words = append(words, word{w, n})
	}
	// Most repeated first, then alphabetical: a stable line for the same pool.
	sort.Slice(words, func(i, j int) bool {
		if words[i].n != words[j].n {
			return words[i].n > words[j].n
		}
		return words[i].text < words[j].text
	})
	if len(words) > heuristicKeywordLimit {
		words = words[:heuristicKeywordLimit]
	}
	parts := make([]string, 0, len(words))
	for _, w := range words {
		parts = append(parts, w.text)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, ", ")
}

// collapse turns a multi-line description into the single line the reminder
// renders, and bounds it.
func collapse(text string) string {
	return strutil.Truncate(strings.Join(strings.Fields(text), " "), summaryTextLimit)
}
