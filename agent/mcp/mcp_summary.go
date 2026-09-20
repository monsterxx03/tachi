package mcp

import (
	"sort"
	"strings"

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

	out := make([]ServerSummary, 0, len(byServer))
	for server, tools := range byServer {
		// Sorted so a generated summary (and the cache key that guards it) sees
		// one order for one pool.
		sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
		text, source := s.text(server, tools)
		out = append(out, ServerSummary{Server: server, Text: text, Source: source})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Server < out[j].Server })
	return out
}

// text picks a description for one server — the server's own words, else a
// generated summary, else the local heuristic — and names where it came from.
func (s *ServerSummarizer) text(server string, tools []*DeferredTool) (string, string) {
	if s.Instructions != nil {
		if text := collapse(s.Instructions(server)); text != "" {
			return text, ServerInstructionsSource
		}
	}
	if s.Generate != nil {
		if text, err := s.Generate(server, tools); err == nil {
			if text = collapse(text); text != "" {
				return text, GeneratedSource
			}
		}
		// A failed generation is not an error here: the heuristic below is what
		// a reader gets, and the cache layer logs its own failure. A reminder
		// that could not be built would be worse than a thin one.
	}
	return heuristicText(server, tools), HeuristicSource
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
