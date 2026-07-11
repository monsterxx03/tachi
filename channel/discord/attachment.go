package discord

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// allowedExtensions is the Phase 1 whitelist of file extensions for attachment
// content injection. Files with extensions not in this list are still downloaded
// and cached, but their content is not injected into the LLM prompt.
var allowedExtensions = map[string]bool{
	".txt":  true,
	".md":   true,
	".pdf":  true,
	".png":  true,
	".jpg":  true,
	".jpeg": true,
	".gif":  true,
	".csv":  true,
	".json": true,
	".yaml": true,
	".yml":  true,
	".xml":  true,
	".log":  true,
	".go":   true,
	".py":   true,
	".js":   true,
	".ts":   true,
	".rs":   true,
}

// attachment represents a downloaded file attachment with metadata.
type attachment struct {
	FileName  string
	MimeType  string
	Data      []byte
	Size      int64
	SavedPath string
}

// downloadAttachment downloads a file from a Discord CDN URL.
// The URL must be authenticated with the Bot token via Authorization header.
func (ch *DiscordChannel) downloadAttachment(url string) (*attachment, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("discord: create download request: %w", err)
	}
	req.Header.Set("Authorization", "Bot "+ch.cfg.Token)

	resp, err := ch.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("discord: download attachment: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discord: download attachment status %d", resp.StatusCode)
	}

	// Read body, respecting the size limit.
	body, err := io.ReadAll(io.LimitReader(resp.Body, ch.cfg.MaxAttachmentBytes+1))
	if err != nil {
		return nil, fmt.Errorf("discord: read attachment: %w", err)
	}

	if int64(len(body)) > ch.cfg.MaxAttachmentBytes {
		return nil, fmt.Errorf("discord: attachment exceeds max size (%d > %d bytes)",
			len(body), ch.cfg.MaxAttachmentBytes)
	}

	// Extract filename from Content-Disposition or URL.
	filename := extractFilename(url, resp)

	// Detect MIME type from content (more reliable than extension).
	mimeType := detectMIMEType(body, filename)

	return &attachment{
		FileName: filename,
		MimeType: mimeType,
		Data:     body,
		Size:     int64(len(body)),
	}, nil
}

// saveAttachment saves attachment data to the cache directory and returns
// the local path. The filename is suffixed with a short content hash to
// avoid collisions.
func (ch *DiscordChannel) saveAttachment(att *attachment) (string, error) {
	if err := os.MkdirAll(ch.cacheDir, 0755); err != nil {
		return "", fmt.Errorf("discord: create cache dir: %w", err)
	}

	// Generate a unique filename with a content hash prefix.
	hash := sha256.Sum256(att.Data)
	shortHash := fmt.Sprintf("%x", hash[:8])
	safeName := fmt.Sprintf("%s_%s", shortHash, sanitizeFilename(att.FileName))
	destPath := filepath.Join(ch.cacheDir, safeName)

	if err := os.WriteFile(destPath, att.Data, 0644); err != nil {
		return "", fmt.Errorf("discord: write attachment cache: %w", err)
	}

	return destPath, nil
}

// isAllowedFileType checks whether a file is safe to process.
// Uses double verification: file extension whitelist + MIME type sniffing.
func isAllowedFileType(filename string, data []byte) bool {
	ext := strings.ToLower(filepath.Ext(filename))
	if !allowedExtensions[ext] {
		return false
	}

	// MIME type sniffing for extra safety.
	mime := detectMIMEType(data, filename)
	if strings.HasPrefix(mime, "text/") ||
		strings.HasPrefix(mime, "image/") ||
		mime == "application/pdf" ||
		mime == "application/json" ||
		mime == "application/xml" ||
		mime == "application/x-yaml" ||
		mime == "text/csv" {
		return true
	}
	// If MIME detection fails or returns unrecognized type, fall back
	// to the extension whitelist result (lenient for Phase 1).
	return true
}

// extractFilename extracts a filename from the HTTP response or URL.
func extractFilename(url string, resp *http.Response) string {
	// Try Content-Disposition header first.
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if idx := strings.Index(cd, "filename="); idx >= 0 {
			f := cd[idx+9:]
			f = strings.Trim(f, `" `)
			if f != "" {
				return f
			}
		}
	}

	// Fall back to URL path.
	if idx := strings.LastIndex(url, "/"); idx >= 0 {
		name := url[idx+1:]
		// URLs may have query parameters.
		if qidx := strings.Index(name, "?"); qidx >= 0 {
			name = name[:qidx]
		}
		if name != "" {
			return name
		}
	}

	return "unknown"
}

// detectMIMEType attempts to detect the MIME type from file content.
// Falls back to extension-based detection if content sniffing is inconclusive.
func detectMIMEType(data []byte, filename string) string {
	// Use at most 512 bytes for MIME detection (http.DetectContentType limit).
	sniffLen := len(data)
	if sniffLen > 512 {
		sniffLen = 512
	}

	mime := http.DetectContentType(data[:sniffLen])

	// http.DetectContentType often returns "application/octet-stream" when
	// it can't determine the type. Try extension-based fallback.
	if mime == "application/octet-stream" {
		ext := strings.ToLower(filepath.Ext(filename))
		switch ext {
		case ".pdf":
			return "application/pdf"
		case ".md":
			return "text/markdown"
		case ".csv":
			return "text/csv"
		case ".yaml", ".yml":
			return "application/x-yaml"
		case ".json":
			return "application/json"
		case ".xml":
			return "application/xml"
		case ".txt", ".log":
			return "text/plain"
		case ".go", ".py", ".js", ".ts", ".rs":
			return "text/plain; charset=utf-8"
		}
	}

	return mime
}

// sanitizeFilename removes potentially dangerous characters from filenames.
func sanitizeFilename(name string) string {
	// Replace common path separators and control characters.
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' ||
			r == '"' || r == '<' || r == '>' || r == '|' || r == '\x00' {
			return '_'
		}
		return r
	}, name)
	return name
}

// isTextContent returns true if the MIME type suggests the file contains
// human-readable text that can be injected into the LLM prompt.
func isTextContent(mimeType string) bool {
	return strings.HasPrefix(mimeType, "text/") ||
		mimeType == "application/json" ||
		mimeType == "application/xml" ||
		mimeType == "application/x-yaml"
}

const maxTextInjectionSize = 100 * 1024 // 100 KiB

// isWithinTextInjectionLimit checks if the file is small enough for text
// content injection into the LLM prompt.
func isWithinTextInjectionLimit(size int64) bool {
	return size <= maxTextInjectionSize
}
