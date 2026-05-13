package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

// SkillLoader loads a skill by name and returns its full content.
type SkillLoader interface {
	LoadSkill(name string) (*SkillData, error)
}

// SkillData is the full data for a loaded skill.
type SkillData struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Body        string            `json:"body"`
	Source      string            `json:"source"`
	Dir         string            `json:"dir"`
	Files       map[string]string `json:"files"` // relative path → content
}

// SkillViewTool allows the agent to load skill content on demand.
type SkillViewTool struct {
	loader SkillLoader
}

// NewSkillViewTool creates a SkillViewTool backed by the given loader.
func NewSkillViewTool(loader SkillLoader) *SkillViewTool {
	return &SkillViewTool{loader: loader}
}

func (t *SkillViewTool) Name() string { return ToolNameSkillView }

func (t *SkillViewTool) Description() string {
	return "Load a skill's full instructions. Use skills_list first to see available skills."
}

func (t *SkillViewTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"name": {
			Type:        "string",
			Description: "Skill name to load",
		},
		"file_path": {
			Type:        "string",
			Description: "Optional: path to a supporting file within the skill directory (e.g., \"references/checklist.md\")",
		},
	}
}

func (t *SkillViewTool) Required() []string { return []string{"name"} }

func (t *SkillViewTool) Parallel() bool { return false }

func (t *SkillViewTool) ExecuteContext(ctx context.Context, args string) (string, error) {
	var params struct {
		Name     string `json:"name"`
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	sk, err := t.loader.LoadSkill(params.Name)
	if err != nil {
		return "", fmt.Errorf("skill %q not found: %w", params.Name, err)
	}

	// If file_path is specified, return that specific file
	if params.FilePath != "" {
		content, ok := sk.Files[params.FilePath]
		if !ok {
			return "", fmt.Errorf("file %q not found in skill %q", params.FilePath, params.Name)
		}
		return content, nil
	}

	// Return full skill content
	var result struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Body        string   `json:"body"`
		Source      string   `json:"source"`
		Dir         string   `json:"dir"`
		Files       []string `json:"files"` // list of available file paths
	}
	result.Name = sk.Name
	result.Description = sk.Description
	result.Body = sk.Body
	result.Source = sk.Source
	result.Dir = sk.Dir
	for path := range sk.Files {
		result.Files = append(result.Files, path)
	}
	sort.Strings(result.Files)

	// Return as formatted text so the LLM can see the full instructions
	var output string
	output += fmt.Sprintf("Skill: %s\n", result.Name)
	output += fmt.Sprintf("Description: %s\n", result.Description)
	output += fmt.Sprintf("Source: %s\n", result.Source)
	output += fmt.Sprintf("Directory: %s\n", result.Dir)
	if len(result.Files) > 0 {
		output += fmt.Sprintf("Supporting files:\n")
		for _, f := range result.Files {
			output += fmt.Sprintf("  - %s\n", f)
		}
		output += fmt.Sprintf("\n")
	}
	output += fmt.Sprintf("\n--- Skill Instructions ---\n\n")
	output += sk.Body

	return output, nil
}