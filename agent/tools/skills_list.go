package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// SkillLister provides the list of available skills with metadata.
type SkillLister interface {
	ListSkills() []SkillListEntry
}

// SkillListEntry is a lightweight representation of a skill for listing.
type SkillListEntry struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Source      string   `json:"source"` // "project" | "global"
}

// SkillsListTool lists all available skills with metadata.
type SkillsListTool struct {
	lister SkillLister
}

// NewSkillsListTool creates a SkillsListTool backed by the given lister.
func NewSkillsListTool(lister SkillLister) *SkillsListTool {
	return &SkillsListTool{lister: lister}
}

func (t *SkillsListTool) Name() string { return ToolNameSkillsList }

func (t *SkillsListTool) Description() string {
	return "List available skills with name and description. Use SkillView(name) to load full content."
}

func (t *SkillsListTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"tag": {
			Type:        "string",
			Description: "Filter skills by tag",
		},
	}
}

func (t *SkillsListTool) Required() []string { return nil }

func (t *SkillsListTool) Parallel() bool { return false }

type skillsListResult struct {
	Success bool             `json:"success"`
	Skills  []SkillListEntry `json:"skills"`
	Count   int              `json:"count"`
}

func (t *SkillsListTool) ExecuteContext(ctx context.Context, args string) (string, error) {
	var params struct {
		Tag string `json:"tag"`
	}
	if args != "" {
		if err := json.Unmarshal([]byte(args), &params); err != nil {
			// Ignore parse errors for optional params, just list all
		}
	}

	entries := t.lister.ListSkills()

	// Filter by tag if specified
	if params.Tag != "" {
		tag := strings.ToLower(params.Tag)
		filtered := make([]SkillListEntry, 0)
		for _, e := range entries {
			for _, t := range e.Tags {
				if strings.ToLower(t) == tag {
					filtered = append(filtered, e)
					break
				}
			}
		}
		entries = filtered
	}

	result := skillsListResult{
		Success: true,
		Skills:  entries,
		Count:   len(entries),
	}

	b, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("failed to marshal result: %w", err)
	}
	return string(b), nil
}