package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/coder/acp-go-sdk"
	"github.com/monsterxx03/tachi/agent/acpctx"
	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/pkg/fileutil"
)

// WriteTool writes content to a file
type WriteTool struct {
	acpMode bool // true = route writes through ACP writeTextFile when a connection is available
}

// SetACPMode enables ACP mode. In ACP mode ExecuteContext routes writes
// through conn.WriteTextFile (client-side file system) instead of the local
// filesystem.
func (t *WriteTool) SetACPMode(v bool) { t.acpMode = v }

// NewWriteTool creates a WriteTool.
func NewWriteTool() *WriteTool { return &WriteTool{} }

func (t *WriteTool) Name() string        { return ToolNameWrite }
func (t *WriteTool) Description() string { return "Writes a file to the local filesystem." }
func (t *WriteTool) IsDestructive() bool { return true }
func (t *WriteTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"path":    {Type: "string", Description: "The path to write to"},
		"content": {Type: "string", Description: "The content to write"},
	}
}
func (t *WriteTool) Required() []string { return []string{"path", "content"} }
func (t *WriteTool) Parallel() bool     { return false }

func (t *WriteTool) ExecuteContext(ctx context.Context, args string) (string, error) {
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
	if t.acpMode {
		if conn := acpctx.Conn(ctx); conn != nil {
			_, err := conn.WriteTextFile(ctx, acp.WriteTextFileRequest{
				SessionId: acpctx.SessionID(ctx),
				Path:      filePath,
				Content:   argsMap.Content,
			})
			if err != nil {
				return "", fmt.Errorf("ACP writeTextFile failed: %w", err)
			}
			return fmt.Sprintf("Successfully wrote via ACP to %s (%d bytes)", argsMap.Path, len(argsMap.Content)), nil
		}
	}

	if err := fileutil.WriteFileShared(filePath, []byte(argsMap.Content)); err != nil {
		return "", fmt.Errorf("failed to write file: %w", err)
	}

	return fmt.Sprintf("Successfully wrote to %s (%d bytes)", argsMap.Path, len(argsMap.Content)), nil
}
