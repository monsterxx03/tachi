package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/monsterxx03/tachi/agent/wdctx"
)

// WriteTool writes content to a file
type WriteTool struct{}

func (t WriteTool) Name() string        { return ToolNameWrite }
func (t WriteTool) Description() string { return "Writes a file to the local filesystem." }
func (t WriteTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"path":    {Type: "string", Description: "The path to write to"},
		"content": {Type: "string", Description: "The content to write"},
	}
}
func (t WriteTool) Required() []string { return []string{"path", "content"} }
func (t WriteTool) Parallel() bool     { return false }

func (t WriteTool) ExecuteContext(ctx context.Context, args string) (string, error) {
	var argsMap struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(args), &argsMap); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	filePath := argsMap.Path
	if !filepath.IsAbs(filePath) {
		filePath = filepath.Join(wdctx.Dir(ctx), filePath)
	}

	// Enforce path policy (used by Dream sub-agent sandbox).
	if policy := GetPathPolicy(ctx); policy != nil {
		absPath, _ := filepath.Abs(filePath)
		if err := policy.CheckPath(absPath); err != nil {
			return "", err
		}
	}

	// Ensure parent directory exists
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("failed to create directory: %w", err)
	}

	if err := os.WriteFile(filePath, []byte(argsMap.Content), 0644); err != nil {
		return "", fmt.Errorf("failed to write file: %w", err)
	}

	return fmt.Sprintf("Successfully wrote to %s (%d bytes)", argsMap.Path, len(argsMap.Content)), nil
}
