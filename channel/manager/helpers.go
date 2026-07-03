package manager

import (
	"encoding/base64"
	"fmt"
	"strings"

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
