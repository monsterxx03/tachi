package systemreminder

import (
	"context"
	"fmt"
	"strings"
)

// SkillMetaProvider provides a list of skill metadata for the reminder system.
type SkillMetaProvider interface {
	ListSkillMetas() []SkillMetaRecord
}

// SkillMetaRecord is a minimal representation of a skill for the reminder.
type SkillMetaRecord struct {
	Name        string
	Description string
	Tags        []string
}

// SkillListReminder injects the compact skill catalog so the LLM always knows
// what skills it can activate via SkillView(). Unlike DateReminder, it only
// fires on the first user message of a conversation or when the skill list has
// changed (e.g., after skill_create). This avoids wasting context window on
// repeated injections of a largely static catalog.
type SkillListReminder struct {
	provider SkillMetaProvider
	dirty    bool // true when the skill list has changed and needs re-injection
}

// NewSkillListReminder creates a SkillListReminder backed by the given provider.
// The reminder starts dirty so it fires on the very first user message.
func NewSkillListReminder(provider SkillMetaProvider) *SkillListReminder {
	return &SkillListReminder{provider: provider, dirty: true}
}

// MarkDirty forces the reminder to re-fire on the next user message,
// even if it has already fired in this session.
func (r *SkillListReminder) MarkDirty() {
	r.dirty = true
}

// SetProvider updates the skill store backing and marks the reminder dirty
// so the updated skill list is re-injected.
func (r *SkillListReminder) SetProvider(provider SkillMetaProvider) {
	r.provider = provider
	r.dirty = true
}

func (r *SkillListReminder) Generate(ctx context.Context, rctx Context) []string {
	if r.provider == nil {
		return nil
	}
	// Don't inject skill list at tool-result boundaries — it's not transient
	// contextual information and the LLM already knows what skills are available
	// from the previous injection.
	if rctx.IsToolResult {
		return nil
	}
	// Only fire when the skill list has changed (or on the very first call,
	// since dirty starts as true). We intentionally do NOT use ctx.IsFirstMessage
	// here: in channel mode the session stores raw (unwrapped) user messages,
	// so historyHasReminder() always returns false, making reminderIsFirst=true
	// on every turn and causing the skill list to be re-injected on every message.
	if !r.dirty {
		return nil
	}
	metas := r.provider.ListSkillMetas()
	if len(metas) == 0 {
		return nil
	}
	prompt := buildSkillListPrompt(metas)
	if len(prompt) == 0 {
		return nil
	}
	r.dirty = false
	return prompt
}

// buildSkillListPrompt builds the skill catalog block for the reminder, one line per entry.
//
// The FIRST line is a heading, and that is not decoration. This block is rendered into the same
// <system-reminder> as every other reminder, indented two spaces like theirs, and nothing
// separates it from what came before — so an unlabelled list lands directly under the last line of
// the previous block and reads as ITS children. That is not hypothetical: with the peers reminder
// above it (`- ugreen-nas：…`), this list was read as THAT NODE's advertised skills, and a model
// told the user what a remote machine could do based on the local skill store. The heading is the
// whole fix — the list now says whose it is.
//
// The entries stay XML-ish because that is what a name, a description and tags compress into; the
// tags and the "how to use it" live in the heading, the way the MCP catalog reminder does it.
func buildSkillListPrompt(metas []SkillMetaRecord) []string {
	if len(metas) == 0 {
		return nil
	}

	lines := make([]string, 0, len(metas)+1)
	lines = append(lines, `本机可用的 skill（用 Skill(operation="view", name=...) 查看；用户也可以直接打 /名字）：`)
	for _, m := range metas {
		desc := escapeXMLAttr(m.Description)
		tagsStr := ""
		if len(m.Tags) > 0 {
			tagsStr = fmt.Sprintf(" tags=%q", strings.Join(m.Tags, ","))
		}
		lines = append(lines, fmt.Sprintf("  <skill name=%q description=%q%s/>", m.Name, desc, tagsStr))
	}
	return lines
}

// escapeXMLAttr escapes special XML/HTML characters in an attribute value.
func escapeXMLAttr(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	s = strings.ReplaceAll(s, "'", "&apos;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}
