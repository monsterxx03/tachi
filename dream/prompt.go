package dream

import (
	"fmt"
	"strings"

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
	ID        string
	Title     string
	Messages  []MessagePair // pre-filtered user+assistant pairs
}

// MessagePair is a user+assistant exchange.
type MessagePair struct {
	User      string
	Assistant string
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

	b.WriteString(fmt.Sprintf("## Memory Domain\n\n"))
	b.WriteString(fmt.Sprintf("- Domain: %s\n", plan.Group.Domain))
	if plan.Group.Root != "" {
		b.WriteString(fmt.Sprintf("- Project: %s\n", plan.Group.Root))
	}
	b.WriteString(fmt.Sprintf("- Memory directory: %s\n", plan.Group.MemoryRoot))
	b.WriteString(fmt.Sprintf("- Active sessions to process: %d\n\n", len(plan.ActiveSessions)))

	b.WriteString("## Instructions\n\n")
	b.WriteString("1. First, read the existing memory state:\n")
	b.WriteString(fmt.Sprintf("   - `%s/index.md` (may not exist yet)\n", plan.Group.MemoryRoot))
	b.WriteString(fmt.Sprintf("   - `%s/inbox.md` (may not exist yet)\n", plan.Group.MemoryRoot))
	b.WriteString(fmt.Sprintf("   - Files in `%s/topics/`\n\n", plan.Group.MemoryRoot))
	b.WriteString("2. Then review the session summaries below and extract important facts.\n")
	b.WriteString("3. Write consolidated facts into topic files.\n")
	b.WriteString("4. Update index.md and clear inbox.md if you integrated its content.\n\n")

	b.WriteString("## Session Summaries\n\n")

	if len(summaries) == 0 {
		b.WriteString("(No session content available — check inbox.md only)\n")
	}

	for _, s := range summaries {
		b.WriteString(fmt.Sprintf("### Session: %s\n", s.ID))
		if s.Title != "" {
			b.WriteString(fmt.Sprintf("Title: %s\n\n", s.Title))
		}
		for _, m := range s.Messages {
			b.WriteString(fmt.Sprintf("**User**: %s\n\n", truncate(m.User, maxMessageChars)))
			b.WriteString(fmt.Sprintf("**Assistant**: %s\n\n", truncate(m.Assistant, maxMessageChars)))
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

	for _, m := range msgs {
		switch m.Type {
		case session.MessageTypeUser:
			lastUser = m.Content
		case session.MessageTypeAssistant:
			if lastUser != "" {
				pairs = append(pairs, MessagePair{
					User:      lastUser,
					Assistant: m.Content,
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
