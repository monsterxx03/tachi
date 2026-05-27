package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/monsterxx03/tachi/config"
)

// ---- Interfaces ----

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

// SkillCreator creates a new skill on the filesystem.
type SkillCreator interface {
	CreateSkill(params SkillCreateParams) (*SkillCreateResult, error)
}

// SkillDeleter deletes a skill from the filesystem.
type SkillDeleter interface {
	DeleteSkill(name string, source string) error
}

// SkillUpdater updates an existing skill on the filesystem.
type SkillUpdater interface {
	UpdateSkill(params SkillUpdateParams) (*SkillUpdateResult, error)
}

// SkillCreateParams holds the parameters for creating a new skill.
type SkillCreateParams struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Body        string   `json:"body"`
	Tags        []string `json:"tags"`
	Source      string   `json:"source"`    // "project" or "global"
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

// SkillUpdateParams holds the parameters for updating a skill.
type SkillUpdateParams struct {
	Name        string   `json:"name"`
	Description string   `json:"description"` // optional: if empty, keep existing
	Body        string   `json:"body"`        // optional: if empty, keep existing
	Tags        []string `json:"tags"`        // optional: if nil, keep existing
	Source      string   `json:"source"`      // optional: narrow search scope
}

// SkillUpdateResult holds the result after successful skill update.
type SkillUpdateResult struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Source      string   `json:"source"`
	Path        string   `json:"path"` // full path to the updated SKILL.md
}

// SkillManager combines all skill operations into a single interface.
type SkillManager interface {
	SkillLister
	SkillLoader
	SkillCreator
	SkillDeleter
	SkillUpdater
}

// ---- Merged Tool ----

// SkillTool is a unified tool for managing skills: list, view, and create.
type SkillTool struct {
	mgr SkillManager
}

// NewSkillTool creates a SkillTool backed by the given manager.
func NewSkillTool(mgr SkillManager) *SkillTool {
	return &SkillTool{mgr: mgr}
}

func (t *SkillTool) Name() string { return ToolNameSkill }

func (t *SkillTool) Description() string {
	return "Manage skills: list available skills, view skill content, create new skills, update existing skills, or delete skills. " +
		"Use operation=\"list\" to browse available skills. " +
		"Use operation=\"view\" (with name) to load a skill's full instructions. " +
		"Use operation=\"create\" (with name, description, body) to create a new skill. " +
		"Use operation=\"update\" (with name) to modify an existing skill. " +
		"Use operation=\"delete\" (with name) to delete a skill. " +
		fmt.Sprintf("Defaults to project-level (.tachi/skills/); set source=\"global\" for %s/.", config.GlobalSkillsDir())
}

func (t *SkillTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"operation": {
			Type:        "string",
			Description: `Operation to perform: "list" (browse available skills), "view" (load skill content), "create" (create a new skill), "update" (modify an existing skill), or "delete" (delete a skill)`,
		},
		"name": {
			Type:        "string",
			Description: "Skill name (≤64 characters). Required for view, create, update, and delete operations.",
		},
		"description": {
			Type:        "string",
			Description: "One-line description of what this skill does (≤1024 characters). Required for create operation. Optional for update (omit to keep existing).",
		},
		"body": {
			Type:        "string",
			Description: "Skill instructions in Markdown. Required for create operation. Optional for update (omit to keep existing).",
		},
		"tag": {
			Type:        "string",
			Description: "Filter skills by tag (only used with operation=\"list\")",
		},
		"file_path": {
			Type:        "string",
			Description: "Optional: path to a supporting file within the skill directory (e.g., \"references/checklist.md\"). Only used with operation=\"view\".",
		},
		"tags": {
			Type:        "array",
			Description: "Optional tags for categorization (e.g., [\"git\", \"review\"]). Used with create and update operations.",
		},
		"source": {
			Type:        "string",
			Description: `Where to operate: "project" (default, .tachi/skills/) or "global" (~/.tachi/skills/). Used with create, update, and delete operations. For delete/update, narrows search scope.`,
		},
		"overwrite": {
			Type:        "boolean",
			Description: "Whether to overwrite an existing skill with the same name (default: false). Only used with operation=\"create\".",
		},
	}
}

func (t *SkillTool) Required() []string {
	return []string{"operation"}
}

func (t *SkillTool) Parallel() bool { return false }

func (t *SkillTool) ExecuteContext(ctx context.Context, args string) (string, error) {
	var params struct {
		Operation   string   `json:"operation"`
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Body        string   `json:"body"`
		Tag         string   `json:"tag"`
		FilePath    string   `json:"file_path"`
		Tags        []string `json:"tags"`
		Source      string   `json:"source"`
		Overwrite   bool     `json:"overwrite"`
	}
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	switch params.Operation {
	case "list":
		return t.executeList(params.Tag)
	case "view":
		return t.executeView(params.Name, params.FilePath)
	case "create":
		return t.executeCreate(params.Name, params.Description, params.Body, params.Tags, params.Source, params.Overwrite)
	case "delete":
		return t.executeDelete(params.Name, params.Source)
	case "update":
		return t.executeUpdate(params.Name, params.Description, params.Body, params.Tags, params.Source)
	default:
		return "", fmt.Errorf("unknown operation %q: must be one of \"list\", \"view\", \"create\", \"update\", \"delete\"", params.Operation)
	}
}

// ---- Operation Implementations ----

func (t *SkillTool) executeList(tag string) (string, error) {
	entries := t.mgr.ListSkills()

	// Filter by tag if specified
	if tag != "" {
		tagLower := strings.ToLower(tag)
		filtered := make([]SkillListEntry, 0)
		for _, e := range entries {
			for _, t := range e.Tags {
				if strings.ToLower(t) == tagLower {
					filtered = append(filtered, e)
					break
				}
			}
		}
		entries = filtered
	}

	type resultShape struct {
		Success bool             `json:"success"`
		Skills  []SkillListEntry `json:"skills"`
		Count   int              `json:"count"`
	}
	result := resultShape{
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

func (t *SkillTool) executeView(name, filePath string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required for view operation")
	}

	sk, err := t.mgr.LoadSkill(name)
	if err != nil {
		return "", fmt.Errorf("skill %q not found: %w", name, err)
	}

	// If file_path is specified, return that specific file
	if filePath != "" {
		content, ok := sk.Files[filePath]
		if !ok {
			return "", fmt.Errorf("file %q not found in skill %q", filePath, name)
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
		Files       []string `json:"files"`
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

	var output strings.Builder
	output.WriteString(fmt.Sprintf("Skill: %s\n", result.Name))
	output.WriteString(fmt.Sprintf("Description: %s\n", result.Description))
	output.WriteString(fmt.Sprintf("Source: %s\n", result.Source))
	output.WriteString(fmt.Sprintf("Directory: %s\n", result.Dir))
	if len(result.Files) > 0 {
		output.WriteString("Supporting files:\n")
		for _, f := range result.Files {
			output.WriteString(fmt.Sprintf("  - %s\n", f))
		}
		output.WriteString("\n")
	}
	output.WriteString("\n--- Skill Instructions ---\n\n")
	output.WriteString(sk.Body)

	return output.String(), nil
}

func (t *SkillTool) executeCreate(name, description, body string, tags []string, source string, overwrite bool) (string, error) {
	if name == "" || description == "" || body == "" {
		return "", fmt.Errorf("name, description, and body are required for create operation")
	}

	if source == "" {
		source = "project"
	}

	result, err := t.mgr.CreateSkill(SkillCreateParams{
		Name:        name,
		Description: description,
		Body:        body,
		Tags:        tags,
		Source:      source,
		Overwrite:   overwrite,
	})
	if err != nil {
		return "", err
	}

	var output struct {
		Success bool              `json:"success"`
		Skill   SkillCreateResult `json:"skill"`
		Message string            `json:"message"`
	}
	output.Success = true
	output.Skill = *result
	output.Message = fmt.Sprintf("Skill %q created at %s. It will appear in SkillsList (use operation=\"list\") immediately.", result.Name, result.Path)

	b, err := json.Marshal(output)
	if err != nil {
		return "", fmt.Errorf("failed to marshal result: %w", err)
	}
	return string(b), nil
}

func (t *SkillTool) executeDelete(name, source string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required for delete operation")
	}

	err := t.mgr.DeleteSkill(name, source)
	if err != nil {
		return "", err
	}

	var output struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	output.Success = true
	output.Message = fmt.Sprintf("Skill %q has been deleted.", name)

	b, err := json.Marshal(output)
	if err != nil {
		return "", fmt.Errorf("failed to marshal result: %w", err)
	}
	return string(b), nil
}

func (t *SkillTool) executeUpdate(name, description, body string, tags []string, source string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required for update operation")
	}

	result, err := t.mgr.UpdateSkill(SkillUpdateParams{
		Name:        name,
		Description: description,
		Body:        body,
		Tags:        tags,
		Source:      source,
	})
	if err != nil {
		return "", err
	}

	var output struct {
		Success bool              `json:"success"`
		Skill   SkillUpdateResult `json:"skill"`
		Message string            `json:"message"`
	}
	output.Success = true
	output.Skill = *result
	output.Message = fmt.Sprintf("Skill %q updated at %s.", result.Name, result.Path)

	b, err := json.Marshal(output)
	if err != nil {
		return "", fmt.Errorf("failed to marshal result: %w", err)
	}
	return string(b), nil
}
