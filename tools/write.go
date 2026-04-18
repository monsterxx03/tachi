package tools

import (
	"encoding/json"
	"fmt"
	"os"
)

// WriteTool writes content to a file
type WriteTool struct{}

func (t WriteTool) Name() string        { return "Write" }
func (t WriteTool) Description() string { return "Write content to a file" }
func (t WriteTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"path":    {Type: "string", Description: "The path to write to"},
		"content": {Type: "string", Description: "The content to write"},
	}
}
func (t WriteTool) Required() []string    { return []string{"path", "content"} }
func (t WriteTool) Parallel() bool       { return false }
func (t WriteTool) Execute(args string) (string, error) {
	var argsMap struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(args), &argsMap); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if err := os.WriteFile(argsMap.Path, []byte(argsMap.Content), 0644); err != nil {
		return "", fmt.Errorf("failed to write file: %w", err)
	}

	return fmt.Sprintf("Successfully wrote to %s (%d bytes)", argsMap.Path, len(argsMap.Content)), nil
}
