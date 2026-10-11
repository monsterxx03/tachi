package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/monsterxx03/tachi/pkg/strutil"
)

// SendFileCallback is set by whoever registers the tool, and it is what "sending" MEANS in that
// entry: the channel queues an outgoing attachment and reads the bytes at send time, while the
// node hands the bytes to the task's artifact. Instead of passing file contents, it passes the
// local path so the receiver decides when (and whether) to read it.
//
// It RETURNS AN ERROR because the receiver is the one that knows whether it can take this file at
// all — a size limit, a media type it will not carry, a task whose artifact is already closed. The
// tool reports that as a failed call, so a model is never told a file was handed over when nothing
// took it.
type SendFileCallback func(name, mimeType, localPath string) error

// SendFileTool allows the LLM to hand a file to whoever is on the other end of this entry: the IM
// channel sends it, the desktop renders an attachment card from the recorded call, the node puts
// its bytes in the task's artifact. It validates the file exists and is a file, then hands the
// local path over — the bytes are read by the receiver, so a turn does not hold a whole attachment
// in memory.
//
// Registered per turn, as a run-scoped extra: what "send" means differs per entry (and per turn,
// for the channel), so it cannot live in the shared registry (see agent/toolview.go).
type SendFileTool struct {
	// callback is set by the entry that registers this tool. A NIL callback is not a missing
	// wiring: it means the entry renders the hand-over from the recorded tool call itself (the
	// desktop's attachment card), which is a delivery the model can rely on all the same.
	callback SendFileCallback
}

// NewSendFileTool creates a SendFile tool. The callback is set later by
// the channel manager via SetCallback before the agent runs.
func NewSendFileTool() *SendFileTool {
	return &SendFileTool{}
}

// SetCallback attaches the hand-over callback. Called by the entry that registers the tool, before
// the agent runs; see SendFileCallback for what a nil one means.
func (t *SendFileTool) SetCallback(cb SendFileCallback) {
	t.callback = cb
}

func (t *SendFileTool) Name() string { return ToolNameSendFile }
func (t *SendFileTool) Description() string {
	return "Send a file to the user. " +
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

	// Resolve relative path via the shared rule (see ResolvePath).
	filePath := ResolvePath(ctx, p.Path)

	// Stat the file to check it exists and get size.
	info, err := os.Stat(filePath)
	if err != nil {
		return "", fmt.Errorf("cannot access file %q: %w", p.Path, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("cannot send directory %q — please specify a file", p.Path)
	}

	// The size is the RECEIVER's decision, and it is the callback that refuses (it returns an error
	// on purpose, see SendFileCallback): the IM channel has a platform limit, a node has its own
	// file policy, and the desktop sends nothing at all. A limit here would be one entry's number
	// imposed on all three — and the tighter one would win silently, leaving the other two unable
	// to send what they allow.
	fileName := filepath.Base(filePath)
	mimeType := inferMimeType(fileName)

	// Hand the path over — the receiver reads the bytes (the channel at send time, the node into
	// the task's artifact). A callback that refuses is a FAILED call, not a delivery.
	if t.callback != nil {
		if err := t.callback(fileName, mimeType, filePath); err != nil {
			return "", fmt.Errorf("cannot send %q: %w", fileName, err)
		}
	}

	return fmt.Sprintf("✅ 文件 **%s** (%s) 已交给用户", fileName, strutil.HumanBytes(info.Size())), nil
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
