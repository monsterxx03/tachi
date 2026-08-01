package weixin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/channel"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

// Max file sizes for CDN download.
const (
	maxFileDownloadSize  = 10 * 1024 * 1024 // 10 MB
	maxImageDownloadSize = 20 * 1024 * 1024 // 20 MB
	maxTextContentChars  = 100 * 1024       // 100 KB — text content sent to LLM
)

// filesDir returns the base directory for persisting downloaded files:
//
//	~/.tachi/weixin/files/<normalizedAccountID>/
func (ch *Channel) filesDir() string {
	return filepath.Join(config.WeixinStateDir(), "files", normalizeID(ch.accountID))
}

// saveFile persists decrypted data to a file on disk and returns its path.
// Files are saved under filesDir()/<normalizedUserID>/ with the original
// filename. A random suffix is added to avoid collisions.
func (ch *Channel) saveFile(userID string, filename string, data []byte) (string, error) {
	dir := filepath.Join(ch.filesDir(), normalizeID(userID))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("create files dir: %w", err)
	}

	f, err := os.CreateTemp(dir, filename+"-*")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}

	path := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return "", fmt.Errorf("write file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("close file: %w", err)
	}

	ch.logger.Info(context.Background(), "weixin: saved file", "file", filename, "size", strutil.HumanBytes(int64(len(data))), "path", path)
	return path, nil
}

// isTextExtension reports whether a filename has a text-like extension that
// we can safely read as UTF-8 and include in the LLM context.
func isTextExtension(filename string) bool {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".txt", ".md", ".go", ".py", ".js", ".ts", ".tsx", ".jsx",
		".java", ".c", ".cpp", ".h", ".hpp", ".rs", ".rb", ".php",
		".css", ".html", ".json", ".yaml", ".yml", ".toml", ".xml",
		".sh", ".bash", ".zsh", ".conf", ".ini", ".cfg", ".log",
		".csv", ".sql", ".swift", ".kt", ".scala", ".lua", ".pl",
		".r", ".m", ".mm", ".dart", ".gradle", ".tf", ".env",
		".gitignore", ".dockerfile", ".makefile", ".cmake", ".svelte",
		".vue", ".nix", ".lock", ".sum", ".mod", ".gohtml",
		".sqlite", ".ps1", ".bat", ".fish":
		return true
	}
	return false
}

// guessMimeType returns a best-effort MIME type based on file extension.
func guessMimeType(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".pdf":
		return "application/pdf"
	case ".zip":
		return "application/zip"
	case ".tar", ".gz", ".bz2", ".xz":
		return "application/x-compressed"
	case ".doc", ".docx":
		return "application/msword"
	case ".xls", ".xlsx":
		return "application/vnd.ms-excel"
	case ".ppt", ".pptx":
		return "application/vnd.ms-powerpoint"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	case ".mp3":
		return "audio/mpeg"
	case ".mp4":
		return "video/mp4"
	case ".json":
		return "application/json"
	default:
		return "application/octet-stream"
	}
}

// downloadMedia downloads encrypted media from the WeChat CDN and decrypts
// it with AES-128-ECB. Returns the plaintext bytes.
func (ch *Channel) downloadMedia(ref *MediaRef) ([]byte, error) {
	// Determine CDN download URL.
	downloadURL := ref.Media.FullURL
	if downloadURL == "" && ref.Media.EncryptQueryParam != "" {
		downloadURL = fmt.Sprintf("%s/download?encrypted_query_param=%s",
			cdnBaseURL, ref.Media.EncryptQueryParam)
	}
	if downloadURL == "" {
		return nil, fmt.Errorf("no download URL for media item")
	}

	// Download encrypted ciphertext from CDN.
	ciphertext, err := ch.cli.cdnDownload(downloadURL)
	if err != nil {
		return nil, fmt.Errorf("cdn download: %w", err)
	}

	// Enforce size limits.
	var maxSize int
	switch ref.Type {
	case MessageItemTypeImage:
		maxSize = maxImageDownloadSize
	default:
		maxSize = maxFileDownloadSize
	}
	if len(ciphertext) > maxSize {
		return nil, fmt.Errorf("media too large: %d bytes (max %d)", len(ciphertext), maxSize)
	}

	// Resolve AES key.
	var key []byte
	switch ref.Type {
	case MessageItemTypeFile:
		key, err = decodeAESKey(ref.Media.AESKey)
	case MessageItemTypeImage:
		key, err = resolveAESKey(ref.ImageItem)
	default:
		return nil, fmt.Errorf("unsupported media type: %d", ref.Type)
	}
	if err != nil {
		return nil, fmt.Errorf("resolve aes key: %w", err)
	}

	// Decrypt AES-128-ECB + PKCS7.
	plaintext, err := decryptAesEcb(ciphertext, key)
	if err != nil {
		return nil, fmt.Errorf("decrypt media: %w", err)
	}

	return plaintext, nil
}

// processMedia downloads and decrypts a list of MediaRefs, converts them
// into channel.Attachment values, and persists decrypted files to disk so
// the LLM can access them through the Bash tool.
//
// Failed downloads produce attachments with a non-empty Error field so the
// user still gets notified.
func (ch *Channel) processMedia(refs []MediaRef, userID string) []channel.Attachment {
	if len(refs) == 0 {
		return nil
	}

	attachments := make([]channel.Attachment, 0, len(refs))

	for _, ref := range refs {
		data, err := ch.downloadMedia(&ref)
		if err != nil {
			ch.logger.Error(context.Background(), "weixin: processMedia failed", err, "file", ref.FileName)
			attachments = append(attachments, channel.Attachment{
				Type:     attachmentTypeFromMedia(ref.Type),
				FileName: ref.FileName,
				Size:     int64(ref.RawSize),
				Error:    err.Error(),
			})
			continue
		}

		// Save the decrypted file to disk so the LLM can access it via Bash.
		filePath, saveErr := ch.saveFile(userID, ref.FileName, data)
		if saveErr != nil {
			ch.logger.Error(context.Background(), "weixin: save file failed", saveErr, "file", ref.FileName)
			// Non-fatal: still build the attachment without SavedPath.
		}

		switch ref.Type {
		case MessageItemTypeFile:
			att := channel.Attachment{
				Type:      channel.AttachmentTypeFile,
				FileName:  ref.FileName,
				Size:      int64(len(data)),
				Content:   data,
				SavedPath: filePath,
			}

			if isTextExtension(ref.FileName) {
				if len(data) <= maxTextContentChars {
					att.TextContent = string(data)
				} else {
					att.TextContent = string(data[:maxTextContentChars]) +
						"\n... [文件过大，已截断，共 " + strutil.HumanBytes(int64(len(data))) + "]"
				}
			} else {
				att.MimeType = guessMimeType(ref.FileName)
			}

			attachments = append(attachments, att)

		case MessageItemTypeImage:
			// WeChat images are always JPEG (even when FileName lacks an extension).
			mimeType := guessMimeType(ref.FileName)
			if mimeType == "application/octet-stream" {
				mimeType = "image/jpeg"
			}
			attachments = append(attachments, channel.Attachment{
				Type:      channel.AttachmentTypeImage,
				FileName:  ref.FileName,
				Size:      int64(len(data)),
				Content:   data,
				MimeType:  mimeType,
				SavedPath: filePath,
			})
		}
	}

	return attachments
}

// attachmentTypeFromMedia maps WeChat MessageItemType constants to
// channel.AttachmentType.
func attachmentTypeFromMedia(mediaType int) channel.AttachmentType {
	switch mediaType {
	case MessageItemTypeImage:
		return channel.AttachmentTypeImage
	default:
		return channel.AttachmentTypeFile
	}
}
