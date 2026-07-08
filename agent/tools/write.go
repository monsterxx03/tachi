package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/coder/acp-go-sdk"
	"github.com/monsterxx03/tachi/agent/acpctx"
	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/pkg/debuglog"
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
	debuglog.DefaultLogger.Log("ACP write: ExecuteContext called, conn=%v", acpctx.Conn(ctx) != nil)
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

	// In ACP mode, route through ACP client for Zed inline diff + accept/reject.
	if conn := acpctx.Conn(ctx); conn != nil {
		_, err := conn.WriteTextFile(ctx, acp.WriteTextFileRequest{
			Path:    filePath,
			Content: argsMap.Content,
		})
		if err != nil {
			return "", fmt.Errorf("ACP writeTextFile failed: %w", err)
		}
		return fmt.Sprintf("Successfully wrote via ACP to %s (%d bytes)", argsMap.Path, len(argsMap.Content)), nil
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
