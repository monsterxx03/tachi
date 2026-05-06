package weixin

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/pkg/debuglog"
)

// --- Message Extraction ---

// extractMessageText extracts the text body from a message's item_list.
// Returns the text content and whether media attachments exist.
func extractMessageText(items []MessageItem) (string, bool) {
	var textParts []string
	hasMedia := false

	for _, item := range items {
		switch item.Type {
		case MessageItemTypeText:
			if item.TextItem != nil && item.TextItem.Text != "" {
				t := item.TextItem.Text
				// Handle quoted message.
				if item.RefMsg != nil {
					refText := extractRefText(item.RefMsg)
					if refText != "" {
						t = fmt.Sprintf("[引用: %s | %s]\n%s", item.RefMsg.Title, refText, t)
					} else if item.RefMsg.Title != "" {
						t = fmt.Sprintf("[引用: %s]\n%s", item.RefMsg.Title, t)
					}
				}
				textParts = append(textParts, t)
			}

		case MessageItemTypeVoice:
			// If voice-to-text is available, use it directly.
			if item.VoiceItem != nil && item.VoiceItem.Text != "" {
				textParts = append(textParts, item.VoiceItem.Text)
			} else {
				hasMedia = true
			}

		case MessageItemTypeImage, MessageItemTypeFile, MessageItemTypeVideo:
			hasMedia = true
		}
	}

	text := strings.Join(textParts, "\n")
	return text, hasMedia
}

// extractRefText extracts text from a referenced message.
func extractRefText(ref *RefMessage) string {
	items := []MessageItem{ref.MessageItem}
	text, _ := extractMessageText(items)
	return text
}

// --- Message Sending ---

// sendTextReply sends a plain-text reply message to a user.
func (ch *Channel) sendTextReply(toUserID, contextToken, text string) error {
	if text == "" {
		return nil
	}

	// Apply markdown filter.
	text = filterMarkdown(text)

	// If text is empty after filtering, skip.
	if strings.TrimSpace(text) == "" {
		return nil
	}

	msg := WeixinMessage{
		FromUserID:   "",
		ToUserID:     toUserID,
		ClientID:     generateClientID(),
		MessageType:  MessageTypeBot,
		MessageState: MessageStateCompleted,
		ItemList: []MessageItem{
			{
				Type:     MessageItemTypeText,
				TextItem: &TextItem{Text: text},
			},
		},
		ContextToken: contextToken,
	}

	req := &SendMessageRequest{
		Msg:      msg,
		BaseInfo: BaseInfo{ChannelVersion: defaultChannelVersion},
	}

	resp, err := ch.cli.sendMessage(req)
	if err != nil {
		debuglog.Log("weixin: sendMessage error to %s: %v", toUserID, err)
		return err
	}

	if resp.ErrCode != 0 {
		debuglog.Log("weixin: sendMessage to %s: errcode=%d errmsg=%s", toUserID, resp.ErrCode, resp.ErrMsg)
		return fmt.Errorf("sendMessage errcode=%d %s", resp.ErrCode, resp.ErrMsg)
	}

	debuglog.Log("weixin: sent text reply to %s (%d chars)", toUserID, len(text))
	return nil
}

// sendMediaReply sends a media reply to a user. It handles the full flow:
// getUploadUrl → CDN upload → sendMessage with media item.
func (ch *Channel) sendMediaReply(toUserID, contextToken string, data []byte, fileName string, mediaType int) error {
	// Compute plaintext properties.
	rawSize := len(data)
	rawMD5 := md5.Sum(data)
	rawMD5Hex := hex.EncodeToString(rawMD5[:])

	// Generate AES key and file key.
	aesKey := randomBytes(16)
	fileKey := hex.EncodeToString(randomBytes(16))
	aesKeyHex := hex.EncodeToString(aesKey)

	// Encrypt.
	ciphertext, err := encryptAesEcb(data, aesKey)
	if err != nil {
		return fmt.Errorf("encrypt media: %w", err)
	}

	fileSize := len(ciphertext)

	// Step 1: Get upload URL.
	uploadReq := &GetUploadURLRequest{
		FileKey:     fileKey,
		MediaType:   mediaType,
		ToUserID:    toUserID,
		RawSize:     rawSize,
		RawFileMD5:  rawMD5Hex,
		FileSize:    fileSize,
		NoNeedThumb: true,
		AESKey:      aesKeyHex,
		BaseInfo:    BaseInfo{ChannelVersion: defaultChannelVersion},
	}

	uploadResp, err := ch.cli.getUploadURL(uploadReq)
	if err != nil {
		return fmt.Errorf("getUploadUrl: %w", err)
	}

	if uploadResp.Ret != 0 || uploadResp.ErrCode != 0 {
		return fmt.Errorf("getUploadUrl ret=%d errcode=%d", uploadResp.Ret, uploadResp.ErrCode)
	}

	// Step 2: CDN upload.
	uploadURL := uploadResp.UploadFullURL
	if uploadURL == "" {
		uploadURL = fmt.Sprintf("%s/upload?encrypted_query_param=%s&filekey=%s",
			cdnBaseURL, uploadResp.UploadParam, fileKey)
	}

	var encryptedParam string
	// Retry up to 3 times.
	for i := 0; i < 3; i++ {
		encryptedParam, err = ch.cli.cdnUpload(uploadURL, ciphertext)
		if err != nil {
			if strings.Contains(err.Error(), "client error") {
				return err // 4xx, don't retry.
			}
			if i < 2 {
				debuglog.Log("weixin: CDN upload retry %d: %v", i+1, err)
				time.Sleep(time.Duration(i+1) * time.Second)
			}
		} else {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("CDN upload after retries: %w", err)
	}

	if encryptedParam == "" {
		return fmt.Errorf("CDN upload missing x-encrypted-param header")
	}

	// Step 3: sendMessage with media item.
	aesKeyBase64 := base64.StdEncoding.EncodeToString(aesKey)
	media := MediaData{
		EncryptQueryParam: encryptedParam,
		AESKey:            aesKeyBase64,
		EncryptType:       1,
	}

	var item MessageItem
	switch mediaType {
	case MediaTypeImage:
		item = MessageItem{
			Type: MessageItemTypeImage,
			ImageItem: &MediaItem{
				Media:   media,
				MidSize: fileSize,
				AESKey:  aesKeyHex,
			},
		}
	case MediaTypeVideo:
		item = MessageItem{
			Type: MessageItemTypeVideo,
			VideoItem: &MediaItem{
				Media:     media,
				VideoSize: fileSize,
			},
		}
	case MediaTypeFile:
		item = MessageItem{
			Type: MessageItemTypeFile,
			FileItem: &FileItem{
				Media:    media,
				FileName: fileName,
				Len:      fmt.Sprintf("%d", rawSize),
			},
		}
	default:
		return fmt.Errorf("unsupported media type: %d", mediaType)
	}

	msg := WeixinMessage{
		FromUserID:   "",
		ToUserID:     toUserID,
		ClientID:     generateClientID(),
		MessageType:  MessageTypeBot,
		MessageState: MessageStateCompleted,
		ItemList:     []MessageItem{item},
		ContextToken: contextToken,
	}

	sendReq := &SendMessageRequest{
		Msg:      msg,
		BaseInfo: BaseInfo{ChannelVersion: defaultChannelVersion},
	}

	sendResp, err := ch.cli.sendMessage(sendReq)
	if err != nil {
		return fmt.Errorf("sendMessage media: %w", err)
	}

	if sendResp.ErrCode != 0 {
		return fmt.Errorf("sendMessage media errcode=%d %s", sendResp.ErrCode, sendResp.ErrMsg)
	}

	debuglog.Log("weixin: sent media reply to %s (%s, %d bytes)", toUserID, fileName, rawSize)
	return nil
}

// --- Helpers ---

// generateClientID creates a unique client ID for message dedup.
func generateClientID() string {
	return fmt.Sprintf("%d-%s", time.Now().UnixMilli(), hex.EncodeToString(randomBytes(4)))
}

// randomBytes generates cryptographically random bytes.
func randomBytes(n int) []byte {
	b := make([]byte, n)
	readRandom(b)
	return b
}

// truncate limits a string to n characters.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
