package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/monsterxx03/tachi/agent/wdctx"
)

// SavePlanTool saves a structured plan document to .tachi/plans/.
// The LLM calls this tool to create or update a plan while in plan mode.
type SavePlanTool struct{}

func (t SavePlanTool) Name() string { return ToolNameSavePlan }
func (t SavePlanTool) Description() string {
	return "Save or update a structured plan document. " +
		"Use this when you have developed a clear plan and want to record it. " +
		"Call multiple times to update the plan as it evolves."
}
func (t SavePlanTool) IsDestructive() bool { return false }
func (t SavePlanTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"title": {
			Type:        "string",
			Description: "A concise title for the plan (e.g. 'Refactor User Module')",
		},
		"content": {
			Type:        "string",
			Description: "Full plan content in markdown — goals, approach, key changes, file list, design decisions",
		},
		"steps": {
			Type:        "array",
			Description: "Structured task list with status tracking",
			Items: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"content": map[string]any{"type": "string", "description": "What needs to be done (imperative form)"},
					"status":  map[string]any{"type": "string", "description": "pending | in_progress | completed"},
				},
				"required": []string{"content", "status"},
			},
		},
	}
}
func (t SavePlanTool) Required() []string { return []string{"title", "content", "steps"} }
func (t SavePlanTool) Parallel() bool     { return false }

// SavePlanParams mirrors the tool's expected JSON arguments.
type SavePlanParams struct {
	Title   string         `json:"title"`
	Content string         `json:"content"`
	Steps   []SavePlanStep `json:"steps"`
}

// SavePlanStep is a single step within a plan.
type SavePlanStep struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

func (t SavePlanTool) ExecuteContext(ctx context.Context, args string) (string, error) {
	var params SavePlanParams
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if params.Title == "" || params.Content == "" {
		return "", fmt.Errorf("title and content are required")
	}

	// Validate step statuses
	for i, s := range params.Steps {
		switch s.Status {
		case "pending", "in_progress", "completed":
		default:
			return "", fmt.Errorf("step %d (%q): invalid status %q (must be pending, in_progress, or completed)", i, s.Content, s.Status)
		}
	}

	// Determine plan directory: .tachi/plans/ under the project root.
	// Uses wdctx.Dir(ctx) if available (set by ACP/agent loop), falls back
	// to ~/.tachi/plans/.
	baseDir := wdctx.Dir(ctx)
	if baseDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("home dir: %w", err)
		}
		baseDir = home
	}
	planDir := filepath.Join(baseDir, ".tachi", "plans")
	if err := os.MkdirAll(planDir, 0755); err != nil {
		return "", fmt.Errorf("create plans dir: %w", err)
	}

	// Generate filename from title + timestamp
	slug := planSlug(params.Title)
	timestamp := time.Now().Format("2006-01-02-150405")
	filename := fmt.Sprintf("%s-%s.json", timestamp, slug)
	filePath := filepath.Join(planDir, filename)

	// Save raw structured data as JSON — preserves the original format from
	// the LLM without flattening to markdown. Consumers (TUI, ACP, external
	// viewers) can render it however they want.
	data, err := json.MarshalIndent(params, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal plan: %w", err)
	}
	if err := os.WriteFile(filePath, data, 0644); err != nil {
		return "", fmt.Errorf("write plan file: %w", err)
	}

	// Compute summary
	var pending, inProg, done int
	for _, s := range params.Steps {
		switch s.Status {
		case "pending":
			pending++
		case "in_progress":
			inProg++
		case "completed":
			done++
		}
	}

	summary := fmt.Sprintf("Plan saved to %s\n\n**%s** — %d steps: %d pending, %d in progress, %d completed",
		filePath, params.Title, len(params.Steps), pending, inProg, done)

	return summary, nil
}

// planSlug converts a plan title into a filesystem-safe slug.
// Supports Unicode letters (CJK, Cyrillic, etc.) — they are preserved
// as-is. Spaces and runs of hyphens are collapsed into a single hyphen.
func planSlug(title string) string {
	var sb strings.Builder
	sb.Grow(len(title))

	prevDash := false
	for _, r := range strings.ToLower(title) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_':
			sb.WriteRune(r)
			prevDash = false
		case r == '-' || r == ' ' || r == '\t':
			if !prevDash {
				sb.WriteRune('-')
				prevDash = true
			}
		}
	}

	slug := strings.Trim(sb.String(), "-")
	if slug == "" {
		slug = "plan"
	}
	if len(slug) > 48 {
		slug = slug[:48]
	}
	return slug
}
