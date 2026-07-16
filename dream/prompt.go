package dream

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent/memory"
	"github.com/monsterxx03/tachi/session"
)

// BuildPrompt constructs the system+user prompt pair for the Dream SubAgent.
// The sub-agent will execute the Orient→Gather→Consolidate→Prune pipeline.
// maxMessageChars controls truncation of individual messages (0 = default 2000).
func BuildPrompt(plan Plan, sessionSummaries []SessionSummary, maxMessageChars int) (systemPrompt, userPrompt string) {
	systemPrompt = dreamSystemPrompt
	userPrompt = buildUserPrompt(plan, sessionSummaries, maxMessageChars)
	return
}

// SessionSummary is a pre-filtered session ready for dream consumption.
type SessionSummary struct {
	ID       string
	Title    string
	Messages []MessagePair // pre-filtered user+assistant pairs
}

// MessagePair is a user+assistant exchange.
type MessagePair struct {
	User      string
	Assistant string
	Timestamp time.Time // user message timestamp (when the conversation turn started)
}

const dreamSystemPrompt = `You are a memory consolidation agent. Your job is to review recent conversation sessions and distill important knowledge into structured topic files.

## Your Task

Review the provided session summaries and:
1. **Orient**: Read the existing memory index and topic files to understand current state.
2. **Gather**: Identify important facts from the new sessions — decisions, preferences, patterns, bugs, insights.
3. **Consolidate**: Write new facts into appropriate topic files, merge with or supersede existing ones.
4. **Prune**: Remove stale superseded facts (>30 days old), update index.md.

## Output Format

Each topic file should use this format:

` + "```" + `markdown
# <Topic Name>

## <Date>: <Brief Title>

来源: session <session-id>
状态: active
关键词: <comma-separated keywords and synonyms>

<1-3 sentence description of the fact>

---
` + "```" + `

## Rules

- **One fact per block** separated by ` + "`---`" + `
- **Keep facts concise** — 1-3 sentences max
- If a new fact contradicts an old one, mark the old one as ` + "`状态: superseded`" + `
- Generate ` + "`关键词:`" + ` with synonyms and related terms to improve grep-based recall
- Do NOT duplicate facts already present in topic files
- Clear inbox.md after integrating its contents into topic files
- Update index.md to reflect the current topic structure (keep ≤200 lines)
- If a topic file exceeds 50 facts, summarize older ones

## Available Tools

- ReadFile: read existing topic files, index, inbox
- Grep: search across session content or existing topics
- Glob: list files in memory directory
- WriteFile: create/update topic files (ONLY within the memory directory)

## Important

- You can ONLY write files within the memory directory provided.
- Focus on information that would be useful for future conversations.
- Skip trivial/routine interactions — only extract meaningful knowledge.
`

func buildUserPrompt(plan Plan, summaries []SessionSummary, maxMessageChars int) string {
	if maxMessageChars <= 0 {
		maxMessageChars = 2000
	}
	var b strings.Builder

	b.WriteString("## Memory Domain\n\n")
	fmt.Fprintf(&b, "- Domain: %s\n", plan.Group.Domain)
	if plan.Group.Root != "" {
		fmt.Fprintf(&b, "- Project: %s\n", plan.Group.Root)
	}
	fmt.Fprintf(&b, "- Memory directory: %s\n", plan.Group.MemoryRoot)
	fmt.Fprintf(&b, "- Active sessions to process: %d\n\n", len(plan.ActiveSessions))

	// Inject decay snapshot if available.
	if len(plan.LastState.FactStates) > 0 {
		buildDecaySnapshot(&b, plan.LastState.FactStates)
	}

	b.WriteString("## Instructions\n\n")
	b.WriteString("1. First, read the existing memory state:\n")
	fmt.Fprintf(&b, "   - `%s/index.md` (may not exist yet)\n", plan.Group.MemoryRoot)
	fmt.Fprintf(&b, "   - `%s/inbox.md` (may not exist yet)\n", plan.Group.MemoryRoot)
	fmt.Fprintf(&b, "   - Files in `%s/topics/`\n\n", plan.Group.MemoryRoot)
	b.WriteString("2. Then review the session summaries below and extract important facts.\n")
	b.WriteString("3. Write consolidated facts into topic files.\n")
	b.WriteString("4. Update index.md and clear inbox.md if you integrated its content.\n\n")

	b.WriteString("## Session Summaries\n\n")

	if len(summaries) == 0 {
		b.WriteString("(No session content available — check inbox.md only)\n")
	}

	for _, s := range summaries {
		fmt.Fprintf(&b, "### Session: %s\n", s.ID)
		if s.Title != "" {
			fmt.Fprintf(&b, "Title: %s\n\n", s.Title)
		}
		for _, m := range s.Messages {
			fmt.Fprintf(&b, "**User**: %s\n\n", truncate(m.User, maxMessageChars))
			fmt.Fprintf(&b, "**Assistant**: %s\n\n", truncate(m.Assistant, maxMessageChars))
		}
		b.WriteString("---\n\n")
	}

	return b.String()
}

// FilterSessionMessages extracts user+assistant pairs from session messages,
// skipping tool_call/tool_result/thinking messages to reduce token usage.
func FilterSessionMessages(msgs []session.Message) []MessagePair {
	var pairs []MessagePair
	var lastUser string
	var lastUserTime time.Time

	for _, m := range msgs {
		switch m.Type {
		case session.MessageTypeUser:
			lastUser = m.Content
			lastUserTime = m.Timestamp
		case session.MessageTypeAssistant:
			if lastUser != "" {
				pairs = append(pairs, MessagePair{
					User:      lastUser,
					Assistant: m.Content,
					Timestamp: lastUserTime,
				})
				lastUser = ""
			}
		}
	}
	return pairs
}

func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}

// buildDecaySnapshot injects a summary of fact decay states into the dream
// prompt, aggregated by topic file. This helps the LLM sub-agent know which
// files need pruning without exposing opaque FactID hashes it can't resolve.
func buildDecaySnapshot(b *strings.Builder, states map[string]*memory.FactState) {
	type fileStats struct {
		superseded int
		lowDecay   int
		fresh      int
		minDecay   float64 // for sorting stale files by urgency
		maxReinf   int     // for sorting fresh files by importance
	}
	fileMap := make(map[string]*fileStats)

	for _, fs := range states {
		key := fs.TopicFile
		if key == "" {
			continue
		}
		s := fileMap[key]
		if s == nil {
			s = &fileStats{minDecay: 1.0}
			fileMap[key] = s
		}
		if fs.Superseded {
			s.superseded++
		} else if fs.Decay < 0.3 {
			s.lowDecay++
		} else if fs.Decay >= 0.8 {
			s.fresh++
		}
		if fs.Decay < s.minDecay {
			s.minDecay = fs.Decay
		}
		if fs.Reinforcements > s.maxReinf {
			s.maxReinf = fs.Reinforcements
		}
	}

	// Collect files with stale facts and fresh facts.
	type fileEntry struct {
		name  string
		stats *fileStats
	}
	var staleFiles, freshFiles []fileEntry
	for name, s := range fileMap {
		if s.superseded > 0 || s.lowDecay > 0 {
			staleFiles = append(staleFiles, fileEntry{name, s})
		}
		if s.fresh > 0 {
			freshFiles = append(freshFiles, fileEntry{name, s})
		}
	}

	if len(staleFiles) == 0 && len(freshFiles) == 0 {
		return
	}

	b.WriteString("## Fact Decay Snapshot\n\n")
	b.WriteString("Per-topic summary for prune decisions. Use ReadFile to inspect the facts.\n\n")

	if len(staleFiles) > 0 {
		sort.Slice(staleFiles, func(i, j int) bool { return staleFiles[i].stats.minDecay < staleFiles[j].stats.minDecay })
		b.WriteString("Files with stale facts (may need pruning):\n\n")
		for _, fe := range staleFiles {
			var parts []string
			if fe.stats.superseded > 0 {
				parts = append(parts, fmt.Sprintf("%d superseded", fe.stats.superseded))
			}
			if fe.stats.lowDecay > 0 {
				parts = append(parts, fmt.Sprintf("%d low-decay (min %.2f)", fe.stats.lowDecay, fe.stats.minDecay))
			}
			fmt.Fprintf(b, "- `%s`: %s\n", fe.name, strings.Join(parts, ", "))
		}
		b.WriteString("\n")
	}

	if len(freshFiles) > 0 {
		sort.Slice(freshFiles, func(i, j int) bool { return freshFiles[i].stats.maxReinf > freshFiles[j].stats.maxReinf })
		b.WriteString("Files with fresh facts (preserve these):\n\n")
		for _, fe := range freshFiles {
			fmt.Fprintf(b, "- `%s`: %d fresh (max reinforcements: %d)\n",
				fe.name, fe.stats.fresh, fe.stats.maxReinf)
		}
		b.WriteString("\n")
	}
}
