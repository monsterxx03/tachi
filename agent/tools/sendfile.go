package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

// SendFileCallback is set by the channel manager when registering the
// SendFile tool. Instead of passing file contents, it passes the local
// path so the channel can read from disk at send time.
type SendFileCallback func(name, mimeType, localPath string)

// SendFileTool allows the LLM to send a file to the user via the IM channel.
// It validates the file exists, then queues it for deferred delivery — the
// actual file read happens at send time in the channel implementation, so
// file content doesn't stay in memory during the agent turn.
//
// Only registered in channel mode (not in TUI mode) since the TUI has its
// own file display mechanisms.
type SendFileTool struct {
	// callback is set by the channel manager to receive file metadata.
	callback SendFileCallback
}

// NewSendFileTool creates a SendFile tool. The callback is set later by
// the channel manager via SetCallback before the agent runs.
func NewSendFileTool() *SendFileTool {
	return &SendFileTool{}
}

// SetCallback attaches the file delivery callback. Called by the channel
// manager when registering the tool for a specific agent turn.
func (t *SendFileTool) SetCallback(cb SendFileCallback) {
	t.callback = cb
}

func (t *SendFileTool) Name() string        { return ToolNameSendFile }
func (t *SendFileTool) IsDestructive() bool { return true }
func (t *SendFileTool) Description() string {
	return "Send a file to the user via the chat channel. " +
		"Use when the user asks for a file (report, screenshot, document, etc.)."
}
func (t *SendFileTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"path": {
			Type:        "string",
			Description: "The absolute or relative path to the file to send",
		},
	}
}
func (t *SendFileTool) Required() []string { return []string{"path"} }
func (t *SendFileTool) Parallel() bool     { return false }

func (t *SendFileTool) ExecuteContext(ctx context.Context, args string) (string, error) {
	var p struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if p.Path == "" {
		return "", fmt.Errorf("path is required")
	}

	// Resolve relative path via working directory context.
	filePath := p.Path
	if !filepath.IsAbs(filePath) {
		filePath = filepath.Join(wdctx.Dir(ctx), filePath)
	}

	// Stat the file to check it exists and get size.
	info, err := os.Stat(filePath)
	if err != nil {
		return "", fmt.Errorf("cannot access file %q: %w", p.Path, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("cannot send directory %q — please specify a file", p.Path)
	}

	// Reject files larger than 50MB (practical limit for IM channels).
	const maxSendSize = 50 * 1024 * 1024
	if info.Size() > maxSendSize {
		return "", fmt.Errorf("file %q is too large (%d bytes; max %d MB)", p.Path, info.Size(), maxSendSize/(1024*1024))
	}

	fileName := filepath.Base(filePath)
	mimeType := inferMimeType(fileName)

	// Queue the attachment — the channel reads from disk at send time.
	if t.callback != nil {
		t.callback(fileName, mimeType, filePath)
	}

	return fmt.Sprintf("✅ 文件 **%s** (%s) 已加入发送队列", fileName, strutil.HumanBytes(info.Size())), nil
}

// inferMimeType returns a best-effort MIME type for the given filename.
func inferMimeType(name string) string {
	ext := filepath.Ext(name)
	switch ext {
	case ".pdf":
		return "application/pdf"
	case ".txt", ".md":
		return "text/plain"
	case ".json":
		return "application/json"
	case ".csv":
		return "text/csv"
	case ".html", ".htm":
		return "text/html"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	case ".zip":
		return "application/zip"
	case ".tar":
		return "application/x-tar"
	case ".gz":
		return "application/gzip"
	case ".xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case ".docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case ".py":
		return "text/x-python"
	case ".go":
		return "text/x-go"
	case ".js":
		return "text/javascript"
	case ".ts":
		return "text/typescript"
	default:
		return "application/octet-stream"
	}
}
