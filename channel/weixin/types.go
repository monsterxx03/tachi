// Package weixin implements the WeChat iLink Bot channel.
//
// Protocol reference: docs/weixin-ilink-protocol.md
//
// This package handles QR-code login, long-polling message reception,
// message sending, CDN media upload/download with AES-128-ECB encryption,
// typing indicators, and context-token management.
package weixin

// --- Login / QR Code ---

// QRCodeResponse is the response from GET /ilink/bot/get_bot_qrcode.
type QRCodeResponse struct {
	QRCode           string `json:"qrcode"`
	QRCodeImgContent string `json:"qrcode_img_content"`
}

// QRCodeStatusResponse is the response from GET /ilink/bot/get_qrcode_status.
type QRCodeStatusResponse struct {
	Status       string `json:"status"`
	BotToken     string `json:"bot_token"`
	ILinkBotID   string `json:"ilink_bot_id"`
	ILinkUserID  string `json:"ilink_user_id"`
	BaseURL      string `json:"baseurl"`
	RedirectHost string `json:"redirect_host"`
}

// QR status constants.
const (
	QRStatusWait              = "wait"
	QRStatusScaned            = "scaned"
	QRStatusConfirmed         = "confirmed"
	QRStatusExpired           = "expired"
	QRStatusScanedButRedirect = "scaned_but_redirect"
	QRStatusNeedVerifyCode    = "need_verifycode"     // v2.3.1+: server requests pair-code
	QRStatusVerifyCodeBlocked = "verify_code_blocked" // v2.3.1+: too many wrong attempts
	QRStatusBindedRedirect    = "binded_redirect"     // v2.3.1+: bot already bound
)

// --- API Common ---

// BaseInfo is included in every API request body.
type BaseInfo struct {
	ChannelVersion string `json:"channel_version"`
	BotAgent       string `json:"bot_agent,omitempty"`
}

// defaultBotAgent is the fallback value when no bot_agent is configured.
const defaultBotAgent = "Tachi"

// Default channel version as observed in protocol.
const defaultChannelVersion = "2.1.7"

// --- QR Login ---

// QRLoginRequest is the body of POST /ilink/bot/get_bot_qrcode (v2.3.1+).
type QRLoginRequest struct {
	LocalTokenList []string `json:"local_token_list,omitempty"`
}

// --- getUpdates ---

// GetUpdatesRequest is the body of POST /ilink/bot/getupdates.
type GetUpdatesRequest struct {
	GetUpdatesBuf string   `json:"get_updates_buf"`
	BaseInfo      BaseInfo `json:"base_info"`
}

// GetUpdatesResponse is the response of POST /ilink/bot/getupdates.
type GetUpdatesResponse struct {
	Ret                  int             `json:"ret"`
	ErrCode              int             `json:"errcode"`
	Msgs                 []WeixinMessage `json:"msgs"`
	GetUpdatesBuf        string          `json:"get_updates_buf"`
	LongpollingTimeoutMs int             `json:"longpolling_timeout_ms"`
}

// GetUpdates error codes.
const (
	ErrCodeSessionExpired = -14
)

// --- Messages ---

// MessageType constants.
const (
	MessageTypeNone = 0
	MessageTypeUser = 1
	MessageTypeBot  = 2
)

// MessageItemType constants.
const (
	MessageItemTypeNone  = 0
	MessageItemTypeText  = 1
	MessageItemTypeImage = 2
	MessageItemTypeVoice = 3
	MessageItemTypeFile  = 4
	MessageItemTypeVideo = 5
)

// MessageState constants.
const (
	MessageStateNew        = 0
	MessageStateGenerating = 1
	MessageStateCompleted  = 2
)

// WeixinMessage is the full message structure received via getUpdates.
type WeixinMessage struct {
	Seq          int           `json:"seq"`
	MessageID    int64         `json:"message_id"`
	FromUserID   string        `json:"from_user_id"`
	ToUserID     string        `json:"to_user_id"`
	ClientID     string        `json:"client_id"`
	CreateTimeMs int64         `json:"create_time_ms"`
	UpdateTimeMs int64         `json:"update_time_ms"`
	DeleteTimeMs int64         `json:"delete_time_ms"`
	SessionID    string        `json:"session_id"`
	GroupID      string        `json:"group_id"`
	MessageType  int           `json:"message_type"`
	MessageState int           `json:"message_state"`
	ItemList     []MessageItem `json:"item_list"`
	ContextToken string        `json:"context_token"`
}

// MessageItem is a single item within a message.
type MessageItem struct {
	Type         int         `json:"type"`
	CreateTimeMs int64       `json:"create_time_ms"`
	UpdateTimeMs int64       `json:"update_time_ms"`
	IsCompleted  bool        `json:"is_completed"`
	MsgID        string      `json:"msg_id"`
	RefMsg       *RefMessage `json:"ref_msg"`
	TextItem     *TextItem   `json:"text_item"`
	ImageItem    *MediaItem  `json:"image_item"`
	VoiceItem    *VoiceItem  `json:"voice_item"`
	FileItem     *FileItem   `json:"file_item"`
	VideoItem    *MediaItem  `json:"video_item"`
}

// RefMessage is a quoted/reference message.
type RefMessage struct {
	MessageItem MessageItem `json:"message_item"`
	Title       string      `json:"title"`
}

// TextItem is a text content item.
type TextItem struct {
	Text string `json:"text"`
}

// MediaData holds encrypted media information.
type MediaData struct {
	EncryptQueryParam string `json:"encrypt_query_param"`
	AESKey            string `json:"aes_key"`
	EncryptType       int    `json:"encrypt_type"`
	FullURL           string `json:"full_url"`
}

// MediaItem is an image or video content item.
type MediaItem struct {
	Media      MediaData `json:"media"`
	ThumbMedia MediaData `json:"thumb_media"`
	AESKey     string    `json:"aeskey"`      // hex, preferred over media.aes_key for decryption
	URL        string    `json:"url"`
	MidSize    int       `json:"mid_size"`    // image ciphertext size
	ThumbSize  int       `json:"thumb_size"`
	ThumbH     int       `json:"thumb_height"`
	ThumbW     int       `json:"thumb_width"`
	HDSize     int       `json:"hd_size"`
	VideoSize  int       `json:"video_size"`  // video ciphertext size
	PlayLength int       `json:"play_length"`
	VideoMD5   string    `json:"video_md5"`
}

// VoiceItem is a voice content item.
type VoiceItem struct {
	Media         MediaData `json:"media"`
	EncodeType    int       `json:"encode_type"` // 1=pcm,2=adpcm,3=feature,4=speex,5=amr,6=silk,7=mp3,8=ogg-speex
	BitsPerSample int       `json:"bits_per_sample"`
	SampleRate    int       `json:"sample_rate"`
	Playtime      int       `json:"playtime"` // duration in ms
	Text          string    `json:"text"`     // voice-to-text (optional)
}

// FileItem is a file content item.
type FileItem struct {
	Media    MediaData `json:"media"`
	FileName string    `json:"file_name"`
	MD5      string    `json:"md5"`
	Len      string    `json:"len"` // plaintext size as string
}

// --- sendMessage ---

// SendMessageRequest is the body of POST /ilink/bot/sendmessage.
type SendMessageRequest struct {
	Msg      WeixinMessage `json:"msg"`
	BaseInfo BaseInfo      `json:"base_info"`
}

// SendMessageResponse is the response of POST /ilink/bot/sendmessage.
type SendMessageResponse struct {
	Ret     int    `json:"ret"`
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
}

// --- CDN ---

// MediaType constants for getUploadUrl.
const (
	MediaTypeImage = 1
	MediaTypeVideo = 2
	MediaTypeFile  = 3
	MediaTypeVoice = 4
)

// GetUploadURLRequest is the body of POST /ilink/bot/getuploadurl.
type GetUploadURLRequest struct {
	FileKey         string   `json:"filekey"`
	MediaType       int      `json:"media_type"`
	ToUserID        string   `json:"to_user_id"`
	RawSize         int      `json:"rawsize"`
	RawFileMD5      string   `json:"rawfilemd5"`
	FileSize        int      `json:"filesize"`
	ThumbRawSize    int      `json:"thumb_rawsize,omitempty"`
	ThumbRawFileMD5 string   `json:"thumb_rawfilemd5,omitempty"`
	ThumbFileSize   int      `json:"thumb_filesize,omitempty"`
	NoNeedThumb     bool     `json:"no_need_thumb"`
	AESKey          string   `json:"aeskey"`
	BaseInfo        BaseInfo `json:"base_info"`
}

// GetUploadURLResponse is the response of POST /ilink/bot/getuploadurl.
type GetUploadURLResponse struct {
	Ret              int    `json:"ret"`
	ErrCode          int    `json:"errcode"`
	UploadParam      string `json:"upload_param"`
	ThumbUploadParam string `json:"thumb_upload_param"`
	UploadFullURL    string `json:"upload_full_url"`
}

// --- getConfig (typing ticket) ---

// GetConfigRequest is the body of POST /ilink/bot/getconfig.
type GetConfigRequest struct {
	ILinkUserID  string   `json:"ilink_user_id"`
	ContextToken string   `json:"context_token"`
	BaseInfo     BaseInfo `json:"base_info"`
}

// GetConfigResponse is the response of POST /ilink/bot/getconfig.
type GetConfigResponse struct {
	Ret          int    `json:"ret"`
	ErrMsg       string `json:"errmsg"`
	TypingTicket string `json:"typing_ticket"`
}

// --- sendTyping ---

// TypingStatus constants.
const (
	TypingStatusTyping = 1
	TypingStatusCancel = 2
)

// SendTypingRequest is the body of POST /ilink/bot/sendtyping.
type SendTypingRequest struct {
	ILinkUserID  string   `json:"ilink_user_id"`
	TypingTicket string   `json:"typing_ticket"`
	Status       int      `json:"status"`
	BaseInfo     BaseInfo `json:"base_info"`
}

// --- Account Persistence ---

// AccountData is the persistent per-account state stored on disk.
type AccountData struct {
	Token   string `json:"token"`
	SavedAt string `json:"savedAt"`
	BaseURL string `json:"baseUrl"`
	UserID  string `json:"userId"`
}

// SyncData is the persistent get_updates_buf state.
type SyncData struct {
	GetUpdatesBuf string `json:"get_updates_buf"`
}

// ContextTokens is the persistent context token map: userId -> token.
type ContextTokens map[string]string

// AllowFromData is the persistent authorized user allow-list.
type AllowFromData struct {
	Version   int      `json:"version"`
	AllowFrom []string `json:"allowFrom"`
}

// --- notifyStart / notifyStop (v2.1.10+) ---

// NotifyStartRequest is the body of POST /ilink/bot/msg/notifystart.
type NotifyStartRequest struct {
	BaseInfo BaseInfo `json:"base_info"`
}

// NotifyStartResponse is the response of POST /ilink/bot/msg/notifystart.
type NotifyStartResponse struct {
	Ret    int    `json:"ret"`
	ErrMsg string `json:"errmsg"`
}

// NotifyStopRequest is the body of POST /ilink/bot/msg/notifystop.
type NotifyStopRequest struct {
	BaseInfo BaseInfo `json:"base_info"`
}

// NotifyStopResponse is the response of POST /ilink/bot/msg/notifystop.
type NotifyStopResponse struct {
	Ret    int    `json:"ret"`
	ErrMsg string `json:"errmsg"`
}
