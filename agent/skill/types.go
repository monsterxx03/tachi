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
	Source      string   `json:"source"` // "project" | "claude" | "cursor" | "global"
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
	SourceClaude  = "claude"
	SourceCursor  = "cursor"
	SourceGlobal  = "global"
)

// isValidSource checks whether a source string is one of the known values.
// Used for read operations (List/Load) that scan all directories.
func isValidSource(s string) bool {
	return s == SourceProject || s == SourceClaude || s == SourceCursor || s == SourceGlobal
}

// isWritableSource checks whether a source string is valid for write
// operations (Create/Update/Delete). Only "project" (.tachi/skills/) and
// "global" (~/.tachi/skills/) are writable; claude/cursor dirs are
// read-only for compatibility import.
func isWritableSource(s string) bool {
	return s == SourceProject || s == SourceGlobal
}

// ValidateName checks whether a skill name is valid:
// non-empty, ≤64 characters.
func ValidateName(name string) error {
	if len(name) == 0 {
		return fmt.Errorf("skill name must not be empty")
	}
	if len(name) > 64 {
		return fmt.Errorf("skill name %q exceeds 64 characters", name)
	}
	return nil
}

// MaxDescriptionLen is the maximum length for a skill description.
const MaxDescriptionLen = 1024