package skill

import (
	"fmt"
	"sort"
	"strings"
)

// BuildActivationMessage constructs the user message injected when a skill
// is activated (via slash command or LLM routing).
//
// If userInstruction is non-empty (e.g. "test-mcp"), it is inserted between
// the activation header and the skill body as "[Skill <name> additional input: ...]",
// so the LLM can unambiguously associate it with the skill's instructions.
func BuildActivationMessage(sk *Skill, userInstruction string) string {
	var b strings.Builder

	b.WriteString(fmt.Sprintf("[The user has activated the %q skill. ", sk.Meta.Name))
	b.WriteString("Follow its instructions below.]\n\n")

	// User instruction (e.g., "main.go" from "/code-review main.go")
	if userInstruction != "" {
		b.WriteString(fmt.Sprintf("[Skill %q additional input: %s]\n\n", sk.Meta.Name, userInstruction))
	}

	b.WriteString(sk.Body)

	// Supporting files reference
	if len(sk.Files) > 0 {
		b.WriteString(fmt.Sprintf("\n\n[Skill directory: %s]\n", sk.Dir))
		b.WriteString("[Supporting files:\n")

		// Sort for deterministic output
		names := make([]string, 0, len(sk.Files))
		for name := range sk.Files {
			names = append(names, name)
		}
		sort.Strings(names)

		for _, name := range names {
			content := sk.Files[name]
			preview := firstLineOrTruncate(content, 100)
			b.WriteString(fmt.Sprintf("  %s → %s\n", name, preview))
		}
		b.WriteString(fmt.Sprintf("Load with Skill(operation=\"view\", name=%q, path=<path>)]\n", sk.Meta.Name))
	}

	return b.String()
}

// BuildDirectiveMessage constructs a short message for re-invoking an
// already-active skill with new arguments. Unlike BuildActivationMessage, it
// does NOT include the skill body — that's already in the conversation context.
func BuildDirectiveMessage(skillName, directive string) string {
	if directive == "" {
		return fmt.Sprintf("[Skill %q directive: (none)]", skillName)
	}
	return fmt.Sprintf("[Skill %q directive: %s]", skillName, directive)
}

// BuildSkillListPrompt constructs the compact skill catalog injected into
// the system-reminder block for LLM-based routing.
//
// Format (XML-like, compact):
//
//	<available_skills>
//	  <skill name="code-review" description="Review code for bugs and security" tags="review,security"/>
//	  <skill name="git-commit" description="Generate conventional commit messages" tags="git"/>
//	</available_skills>
//
//	To use a skill, call Skill(operation="view", name=...) or the user can type /skill-name.
//
// Returns an empty string when metas is empty.
func BuildSkillListPrompt(metas []SkillMeta) string {
	if len(metas) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("<available_skills>\n")

	for _, m := range metas {
		desc := xmlEscape(m.Description)
		tagsStr := ""
		if len(m.Tags) > 0 {
			tagsStr = fmt.Sprintf(" tags=%q", strings.Join(m.Tags, ","))
		}
		b.WriteString(fmt.Sprintf("  <skill name=%q description=%q%s/>\n", m.Name, desc, tagsStr))
	}

	b.WriteString("</available_skills>\n")
	b.WriteString("\nTo use a skill, call Skill(operation=\"view\", name=...) or the user can type /skill-name.")

	return b.String()
}

// xmlEscape escapes special XML/HTML characters in a string.
func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	s = strings.ReplaceAll(s, "'", "&apos;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// firstLineOrTruncate returns the first line or first maxLen characters (runes).
func firstLineOrTruncate(s string, maxLen int) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = s[:idx]
	}
	runes := []rune(s)
	if len(runes) > maxLen {
		s = string(runes[:maxLen]) + "..."
	}
	return s
}