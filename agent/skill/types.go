// Package skill provides the Skill mechanism for Tachi - reusable capability
// modules that inject domain-specific instructions into agent conversations.
package skill

import "fmt"

// SkillMeta is the lightweight metadata returned by List().
// Used for LLM routing and /skill list display.
type SkillMeta struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Source      string   `json:"source"` // "project" | "global"
}

// Skill is the full parsed skill, returned by Load().
type Skill struct {
	Meta       SkillMeta         `json:"meta"`
	Body       string            `json:"body"`       // SKILL.md body (minus frontmatter)
	RawContent string            `json:"raw_content"` // complete SKILL.md text
	Dir        string            `json:"dir"`         // absolute path to skill directory
	Files      map[string]string `json:"files"`       // supporting files, relative path → content
}

const (
	SourceProject = "project"
	SourceGlobal  = "global"
)

// ValidateName checks whether a skill name conforms to the naming rules:
// lowercase letters, digits, and hyphens, ≤64 characters.
func ValidateName(name string) error {
	if len(name) == 0 {
		return fmt.Errorf("skill name must not be empty")
	}
	if len(name) > 64 {
		return fmt.Errorf("skill name %q exceeds 64 characters", name)
	}
	for _, c := range name {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			continue
		}
		return fmt.Errorf("skill name %q contains invalid character '%c' (only lowercase letters, digits, and hyphens allowed)", name, c)
	}
	return nil
}

// MaxDescriptionLen is the maximum length for a skill description.
const MaxDescriptionLen = 1024