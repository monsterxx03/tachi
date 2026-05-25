package manager

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/channel"
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
	if len(msg.Attachments) == 0 {
		return msg.Content, nil
	}

	var parts []string
	var images []llm.ContentPart

	for _, att := range msg.Attachments {
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
					att.FileName, att.MimeType, humanSize(int(att.Size)), att.SavedPath))
			} else {
				parts = append(parts, fmt.Sprintf("[文件: %s (%s, %s)]",
					att.FileName, att.MimeType, humanSize(int(att.Size))))
			}

		case channel.AttachmentTypeImage:
			imgMsg := fmt.Sprintf("[图片: %s (%s)]", att.FileName, humanSize(int(att.Size)))
			if att.SavedPath != "" {
				imgMsg = fmt.Sprintf("[图片: %s (已保存到 %s, %s)]", att.FileName, att.SavedPath, humanSize(int(att.Size)))
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

	if msg.Content != "" {
		parts = append(parts, msg.Content)
	}

	return strings.Join(parts, "\n\n"), images
}

// zipFile creates an in-memory ZIP archive containing a single file.
func zipFile(name string, data []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, err := w.Create(name)
	if err != nil {
		return nil, fmt.Errorf("create zip entry: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		return nil, fmt.Errorf("write zip entry: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("close zip: %w", err)
	}
	return buf.Bytes(), nil
}

// sanitizeFilename replaces characters that are problematic in filenames.
func sanitizeFilename(s string) string {
	if s == "" {
		return ""
	}
	// Replace problematic chars with underscore.
	replacer := strings.NewReplacer(
		" ", "_",
		"/", "_",
		"\\", "_",
		":", "_",
		"*", "",
		"?", "",
		"\"", "",
		"<", "",
		">", "",
		"|", "",
	)
	result := replacer.Replace(s)
	// Trim to reasonable length (rune-aware).
	runes := []rune(result)
	if len(runes) > 60 {
		result = string(runes[:60])
	}
	return result
}

// --- Tool call summary helpers (used by drainEvents in verbose mode) ---

// summarizeToolCall produces a one-line summary of a tool invocation.
func summarizeToolCall(name, args string) string {
	summary := summarizeToolArgs(name, args)
	if summary == "" {
		return name
	}
	return name + "(" + summary + ")"
}

// summarizeToolArgs extracts the most informative fields from tool call JSON.
func summarizeToolArgs(name, args string) string {
	switch name {
	case tools.ToolNameRead:
		var p struct {
			Path   string `json:"path"`
			Offset int    `json:"offset"`
			Limit  int    `json:"limit"`
		}
		_ = json.Unmarshal([]byte(args), &p)
		if p.Path == "" {
			return ""
		}
		if p.Offset > 0 && p.Limit > 0 {
			return fmt.Sprintf("%s L%d+%d", p.Path, p.Offset, p.Limit)
		}
		if p.Offset > 0 {
			return fmt.Sprintf("%s L%d", p.Path, p.Offset)
		}
		if p.Limit > 0 {
			return fmt.Sprintf("%s +%d", p.Path, p.Limit)
		}
		return p.Path

	case tools.ToolNameBash:
		var p struct{ Command string `json:"command"` }
		_ = json.Unmarshal([]byte(args), &p)
		return truncateForDisplay(p.Command, 60)

	case tools.ToolNameWrite, tools.ToolNameEdit:
		var p struct{ Path string `json:"path"` }
		_ = json.Unmarshal([]byte(args), &p)
		return p.Path

	case tools.ToolNameGrep:
		var p struct {
			Path    string `json:"path"`
			Pattern string `json:"pattern"`
		}
		_ = json.Unmarshal([]byte(args), &p)
		if p.Path != "" && p.Pattern != "" {
			return p.Path + " " + truncateForDisplay(p.Pattern, 30)
		}
		if p.Pattern != "" {
			return truncateForDisplay(p.Pattern, 40)
		}
		return p.Path

	case tools.ToolNameWebSearch:
		var p struct{ Query string `json:"query"` }
		_ = json.Unmarshal([]byte(args), &p)
		return truncateForDisplay(p.Query, 40)

	case tools.ToolNameWebFetch:
		var p struct{ URL string `json:"url"` }
		_ = json.Unmarshal([]byte(args), &p)
		return truncateForDisplay(p.URL, 50)

	case tools.ToolNameGlob:
		var p struct{ Pattern string `json:"pattern"` }
		_ = json.Unmarshal([]byte(args), &p)
		return p.Pattern

	case tools.ToolNameSubAgent:
		var p struct{ Prompt string `json:"prompt"` }
		_ = json.Unmarshal([]byte(args), &p)
		return truncateForDisplay(p.Prompt, 60)

	default:
		return truncateForDisplay(args, 60)
	}
}

// summarizeToolResult produces a one-line summary of a tool execution result.
func summarizeToolResult(name, result string) string {
	lineCount := strings.Count(result, "\n") + 1
	byteLen := len(result)

	switch name {
	case tools.ToolNameRead:
		return fmt.Sprintf("读取 %d 行", lineCount)
	case tools.ToolNameWrite:
		return "写入完成"
	case tools.ToolNameEdit:
		return "编辑完成"
	case tools.ToolNameBash:
		if byteLen <= 200 {
			return result
		}
		return fmt.Sprintf("输出 %d 行 (%s)", lineCount, humanSize(byteLen))
	case tools.ToolNameGrep:
		return fmt.Sprintf("匹配 %d 行", lineCount)
	case tools.ToolNameGlob:
		return fmt.Sprintf("匹配 %d 个文件", lineCount)
	case tools.ToolNameWebSearch:
		return "搜索完成"
	case tools.ToolNameWebFetch:
		return fmt.Sprintf("抓取完成 (%s)", humanSize(byteLen))
	default:
		if byteLen <= 200 {
			return result
		}
		return fmt.Sprintf("%d 行 (%s)", lineCount, humanSize(byteLen))
	}
}

// truncateToolResult limits an error string for display (rune-aware).
func truncateToolResult(s string) string {
	runes := []rune(s)
	if len(runes) <= 150 {
		return s
	}
	return string(runes[:150]) + "..."
}

// humanSize formats a byte count as a human-readable string.
func humanSize(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%dB", n)
	}
	if n < 1024*1024 {
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1fMB", float64(n)/(1024*1024))
}

// truncateForDisplay limits a string for display in channel messages.
// Uses rune-aware truncation to handle multi-byte characters (e.g. Chinese).
func truncateForDisplay(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}

// formatToolDuration formats a time.Duration as a concise human-readable string
// for channel display of tool execution results.
func formatToolDuration(d time.Duration) string {
	if d < time.Microsecond {
		return "(<1µs)"
	}
	if d < time.Millisecond {
		return fmt.Sprintf("(%dµs)", d.Microseconds())
	}
	if d < time.Second {
		return fmt.Sprintf("(%.0fms)", float64(d.Microseconds())/1000)
	}
	if d < time.Minute {
		return fmt.Sprintf("(%.1fs)", d.Seconds())
	}
	minutes := int(d.Minutes())
	seconds := d.Seconds() - float64(minutes*60)
	return fmt.Sprintf("(%dm%.0fs)", minutes, seconds)
}
