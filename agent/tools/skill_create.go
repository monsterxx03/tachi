package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

// SkillCreator creates a new skill on the filesystem.
type SkillCreator interface {
	CreateSkill(params SkillCreateParams) (*SkillCreateResult, error)
}

// SkillCreateParams holds the parameters for creating a new skill.
type SkillCreateParams struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Body        string   `json:"body"`
	Tags        []string `json:"tags"`
	Source      string   `json:"source"`   // "project" or "global"
	Overwrite   bool     `json:"overwrite"` // overwrite existing
}

// SkillCreateResult holds the result after successful skill creation.
type SkillCreateResult struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Source      string   `json:"source"`
	Path        string   `json:"path"` // full path to the created SKILL.md
}

// SkillCreateTool allows the agent to create a new skill.
type SkillCreateTool struct {
	creator SkillCreator
}

// NewSkillCreateTool creates a SkillCreateTool backed by the given creator.
func NewSkillCreateTool(creator SkillCreator) *SkillCreateTool {
	return &SkillCreateTool{creator: creator}
}

func (t *SkillCreateTool) Name() string { return ToolNameSkillCreate }

func (t *SkillCreateTool) Description() string {
	return "Create a new skill. The skill will appear in skills_list and can be activated with /name or skill_view. Defaults to project-level; set source to \"global\" for ~/.tachi/skills/."
}

func (t *SkillCreateTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"name": {
			Type:        "string",
			Description: "Skill name (lowercase letters, digits, and hyphens, ≤64 characters)",
		},
		"description": {
			Type:        "string",
			Description: "One-line description of what this skill does (≤1024 characters)",
		},
		"body": {
			Type:        "string",
			Description: "Skill instructions in Markdown. This is the main content the agent follows when the skill is activated.",
		},
		"tags": {
			Type:        "array",
			Description: "Optional tags for categorization (e.g., [\"git\", \"review\"])",
		},
		"source": {
			Type:        "string",
			Description: `Where to create the skill: "project" (default, .tachi/skills/) or "global" (~/.tachi/skills/)`,
		},
		"overwrite": {
			Type:        "boolean",
			Description: "Whether to overwrite an existing skill with the same name (default: false, returns an error if the skill already exists)",
		},
	}
}

func (t *SkillCreateTool) Required() []string {
	return []string{"name", "description", "body"}
}

func (t *SkillCreateTool) Parallel() bool { return false }

func (t *SkillCreateTool) ExecuteContext(ctx context.Context, args string) (string, error) {
	var params struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Body        string   `json:"body"`
		Tags        []string `json:"tags"`
		Source      string   `json:"source"`
		Overwrite   bool     `json:"overwrite"`
	}
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if params.Source == "" {
		params.Source = "project"
	}

	result, err := t.creator.CreateSkill(SkillCreateParams{
		Name:        params.Name,
		Description: params.Description,
		Body:        params.Body,
		Tags:        params.Tags,
		Source:      params.Source,
		Overwrite:   params.Overwrite,
	})
	if err != nil {
		return "", err
	}

	// Return structured result so the LLM can confirm what was created
	var output struct {
		Success bool             `json:"success"`
		Skill   SkillCreateResult `json:"skill"`
		Message string           `json:"message"`
	}
	output.Success = true
	output.Skill = *result
	output.Message = fmt.Sprintf("Skill %q created at %s. It will appear in skills_list immediately.", result.Name, result.Path)

	b, err := json.Marshal(output)
	if err != nil {
		return "", fmt.Errorf("failed to marshal result: %w", err)
	}
	return string(b), nil
}