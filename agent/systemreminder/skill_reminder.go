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
//
// Implements TaggedReminder so its output gets its own <available-skills> block
// independent of <system-reminder>.
type SkillListReminder struct {
	provider SkillMetaProvider
	dirty    bool // true when the skill list has changed and needs re-injection
}

// WrapperTag implements the TaggedReminder interface.
func (r *SkillListReminder) WrapperTag() string {
	return "available-skills"
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
	if prompt == "" {
		return nil
	}
	r.dirty = false
	return []string{prompt}
}

// buildSkillListPrompt builds the XML-like skill catalog block.
func buildSkillListPrompt(metas []SkillMetaRecord) string {
	if len(metas) == 0 {
		return ""
	}

	var b strings.Builder
	for _, m := range metas {
		desc := escapeXMLAttr(m.Description)
		tagsStr := ""
		if len(m.Tags) > 0 {
			tagsStr = fmt.Sprintf(" tags=%q", strings.Join(m.Tags, ","))
		}
		fmt.Fprintf(&b, "  <skill name=%q description=%q%s/>\n", m.Name, desc, tagsStr)
	}

	b.WriteString("\nTo use a skill, call Skill(operation=\"view\", name=...) or the user can type /skill-name.")

	return b.String()
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
