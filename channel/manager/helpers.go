package manager

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/channel"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

// buildUserMessageWithAttachments constructs the user message text sent to
// the LLM, prepending any file/attachment content before the user's own text.
//
// For text files the content is included inline (good for quick context).
// For all files (including text ones), the local SavedPath is also provided
// so the LLM can use the Bash tool to read/parse the file directly — useful
// for PDFs (pdftotext), Excel (openpyxl), archives, or any format that needs
// programmatic extraction.
//
// Image attachments with raw bytes are returned as ContentParts for multi-modal
// LLM input (vision). A text placeholder is still included in the message text
// so the LLM sees the image reference even when ContentParts are not supported.
func buildUserMessageWithAttachments(msg channel.IncomingMessage) (string, []llm.ContentPart) {
	return buildUserContent(msg.Content, msg.Attachments)
}

// buildUserContent renders a user message (text + attachments) into the text
// sent to the LLM plus any image ContentParts for multi-modal input. It is
// shared by the first-message path (buildUserMessageWithAttachments) and the
// steer injection path: attachments arriving mid-turn while an agent is
// already running must reach the vision pipeline exactly like attachments on
// the message that started the turn — otherwise a queued screenshot degrades
// to a bare "[图片]" placeholder and the LLM never sees its content.
func buildUserContent(content string, attachments []channel.Attachment) (string, []llm.ContentPart) {
	if len(attachments) == 0 {
		return content, nil
	}

	var parts []string
	var images []llm.ContentPart

	for _, att := range attachments {
		if att.Error != "" {
			parts = append(parts, fmt.Sprintf("[文件: %s (下载失败: %s)]", att.FileName, att.Error))
			continue
		}

		switch att.Type {
		case channel.AttachmentTypeText, channel.AttachmentTypeFile:
			if att.TextContent != "" {
				// Text content included inline.
				fileHeader := fmt.Sprintf("[文件: %s]", att.FileName)
				if att.SavedPath != "" {
					fileHeader = fmt.Sprintf("[文件: %s (已保存到 %s)]", att.FileName, att.SavedPath)
				}
				parts = append(parts, fmt.Sprintf("%s\n```\n%s\n```", fileHeader, att.TextContent))
			} else if att.SavedPath != "" {
				// Binary file saved to disk — tell the LLM the path and
				// let it use Bash tools (pdftotext, python, etc.) to parse it.
				parts = append(parts, fmt.Sprintf(
					"[文件: %s (%s, %s)]\n文件已保存到本地: %s\n你可以使用 Bash 工具来解析这个文件（例如 pdftotext 解析 PDF、python 解析 Excel 等）。",
					att.FileName, att.MimeType, strutil.HumanBytes(att.Size), att.SavedPath))
			} else {
				parts = append(parts, fmt.Sprintf("[文件: %s (%s, %s)]",
					att.FileName, att.MimeType, strutil.HumanBytes(att.Size)))
			}

		case channel.AttachmentTypeImage:
			imgMsg := fmt.Sprintf("[图片: %s (%s)]", att.FileName, strutil.HumanBytes(att.Size))
			if att.SavedPath != "" {
				imgMsg = fmt.Sprintf("[图片: %s (已保存到 %s, %s)]", att.FileName, att.SavedPath, strutil.HumanBytes(att.Size))
			}
			parts = append(parts, imgMsg)

			// If we have the raw image bytes, include as a multi-modal ContentPart
			// so the LLM can actually "see" the image via vision.
			if len(att.Content) > 0 {
				mimeType := att.MimeType
				if mimeType == "" {
					mimeType = "image/jpeg" // default fallback
				}
				images = append(images, llm.ContentPart{
					Type:      llm.ContentPartImage,
					MediaType: mimeType,
					Data:      base64.StdEncoding.EncodeToString(att.Content),
				})
			}
		}
	}

	if content != "" {
		parts = append(parts, content)
	}

	return strings.Join(parts, "\n\n"), images
}

// sanitizeFilename replaces characters that are problematic in filenames.
func sanitizeFilename(s string) string {
	// Replace problematic chars with underscore, then trim to a reasonable
	// length (rune-aware).
	return strutil.SanitizeFilename(s, 60)
}

// humanSize formats a byte count as a human-readable string.
func humanSize(n int) string {
	return strutil.HumanBytes(int64(n))
}

// --- Streaming callback context ---

type streamingCtxKey struct{}

// StreamEventType distinguishes text deltas from tool call info in a
// streaming callback, so channel implementations don't need to parse
// ad-hoc formatting to identify tool calls.
type StreamEventType int

const (
	StreamEventTextDelta StreamEventType = iota // LLM text output
	StreamEventToolCall                         // tool call with name + args
)

// StreamEvent carries structured data from drainEvents to the channel's
// streaming callback, replacing the old approach of encoding tool calls
// as formatted strings with HTML markers.
type StreamEvent struct {
	Type     StreamEventType
	Text     string // for TextDelta
	ToolName string // for ToolCall
	ToolArgs string // for ToolCall
}

// StreamingCallback is called for each text delta or tool call during an
// agent turn. The channel implementation uses this to push real-time
// progress to the user (e.g. Discord embed with text + tool calls).
type StreamingCallback func(event StreamEvent) error

// WithStreamingCallback attaches a streaming callback to the context.
// The callback is extracted in runAgentTurn and passed to drainEvents,
// which calls it for every AgentEventTextDelta and AgentEventToolCallArgs.
func WithStreamingCallback(ctx context.Context, cb StreamingCallback) context.Context {
	return context.WithValue(ctx, streamingCtxKey{}, cb)
}

// streamingCallbackFromCtx extracts the StreamingCallback from context, if any.
func streamingCallbackFromCtx(ctx context.Context) StreamingCallback {
	cb, _ := ctx.Value(streamingCtxKey{}).(StreamingCallback)
	return cb
}
