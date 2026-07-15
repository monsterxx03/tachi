package weixin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/pkg/logger"
)

// API base URLs.
const (
	defaultAPIBaseURL = "https://ilinkai.weixin.qq.com"
	cdnBaseURL        = "https://novac2c.cdn.weixin.qq.com/c2c"
	appID             = "bot"
	appClientVersion  = "0x00020107" // version 2.1.7
)

// client wraps an HTTP client and handles the common iLink headers and JSON
// request/response cycle for the Weixin Bot API.
type client struct {
	http     *http.Client
	baseURL  string
	botToken string
	routeTag string
	botAgent string // v2.3.1+: bot_agent field in base_info

	// Default timeouts per endpoint, in seconds.
	getUpdatesTimeout   time.Duration
	sendMessageTimeout  time.Duration
	getUploadURLTimeout time.Duration
	getConfigTimeout    time.Duration
	sendTypingTimeout   time.Duration
	qrStatusTimeout     time.Duration

	logger *logger.Logger
}

func newClient() *client {
	return &client{
		http:                &http.Client{Timeout: 60 * time.Second},
		baseURL:             defaultAPIBaseURL,
		getUpdatesTimeout:   35 * time.Second,
		sendMessageTimeout:  15 * time.Second,
		getUploadURLTimeout: 15 * time.Second,
		getConfigTimeout:    10 * time.Second,
		sendTypingTimeout:   10 * time.Second,
		qrStatusTimeout:     35 * time.Second,
		botAgent:            defaultBotAgent,
		logger:              logger.New("channel.weixin"),
	}
}

// SetBaseURL updates the API base URL (used after IDC redirect).
func (c *client) SetBaseURL(u string) {
	if u != "" {
		c.baseURL = u
	}
}

// SetBotToken updates the Bearer token used for API authentication.
func (c *client) SetBotToken(token string) {
	c.botToken = token
}

// SetRouteTag sets the SKRouteTag header value.
func (c *client) SetRouteTag(tag string) {
	c.routeTag = tag
}

// SetBotAgent sets the bot_agent field sent in base_info (v2.3.1+).
func (c *client) SetBotAgent(agent string) {
	c.botAgent = agent
}

// buildBaseInfo constructs the BaseInfo payload for every API request.
func (c *client) buildBaseInfo() BaseInfo {
	return BaseInfo{
		ChannelVersion: defaultChannelVersion,
		BotAgent:       c.botAgent,
	}
}

// --- Low-level helpers ---

// randomUint32 returns a random uint32 for X-WECHAT-UIN.
func randomUint32() uint32 {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fallback: use time-based value
		return uint32(time.Now().UnixNano())
	}
	return binary.LittleEndian.Uint32(b[:])
}

// makeXWechatUIN generates the X-WECHAT-UIN header value.
func makeXWechatUIN() string {
	val := randomUint32()
	s := strconv.FormatUint(uint64(val), 10)
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func (c *client) addCommonHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("AuthorizationType", "ilink_bot_token")
	if c.botToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.botToken)
	}
	req.Header.Set("X-WECHAT-UIN", makeXWechatUIN())
	req.Header.Set("iLink-App-Id", appID)
	req.Header.Set("iLink-App-ClientVersion", appClientVersion)
	if c.routeTag != "" {
		req.Header.Set("SKRouteTag", c.routeTag)
	}
}

// isGetUpdatesPath reports whether the given URL or path targets the
// long-polling getupdates endpoint, which is called very frequently
// and should not generate log noise.
func isGetUpdatesPath(s string) bool {
	return strings.Contains(s, "/ilink/bot/getupdates")
}

func (c *client) doWithTimeout(method, url string, body []byte, timeout time.Duration) (*http.Response, error) {
	hc := &http.Client{Timeout: timeout + 5*time.Second} // extra 5s for connection
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	c.addCommonHeaders(req)

	// getUpdates is long-polling — skip request log to avoid noise.
	if !isGetUpdatesPath(url) {
		c.logger.Info(context.Background(), "weixin-client: request", "method", method, "addr", url)
	}
	return hc.Do(req)
}

// apiPost performs a JSON POST request and unmarshals the response.
func apiPost[Resp any](c *client, path string, reqBody any, timeout time.Duration) (*Resp, error) {
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := c.baseURL + path
	resp, err := c.doWithTimeout("POST", url, bodyBytes, timeout)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response for %s: %w", path, err)
	}

	// getUpdates is long-polling — skip response log to avoid noise.
	if !isGetUpdatesPath(path) {
		c.logger.Info(context.Background(), "weixin-client: POST response", "path", path, "status", resp.StatusCode, "body", string(respBytes[:min(len(respBytes), 500)]))
	}

	var result Resp
	if err := json.Unmarshal(respBytes, &result); err != nil {
		return nil, fmt.Errorf("unmarshal response for %s (%s): %w", path, string(respBytes[:min(len(respBytes), 200)]), err)
	}
	return &result, nil
}

// apiGet performs a JSON GET request and unmarshals the response.
func apiGet[Resp any](c *client, path string, timeout time.Duration) (*Resp, error) {
	url := c.baseURL + path
	resp, err := c.doWithTimeout("GET", url, nil, timeout)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response for %s: %w", path, err)
	}

	c.logger.Info(context.Background(), "weixin-client: GET response", "path", path, "status", resp.StatusCode, "body", string(respBytes[:min(len(respBytes), 500)]))

	var result Resp
	if err := json.Unmarshal(respBytes, &result); err != nil {
		return nil, fmt.Errorf("unmarshal response for %s (%s): %w", path, string(respBytes[:min(len(respBytes), 200)]), err)
	}
	return &result, nil
}

// cdnUpload posts raw bytes to a CDN URL and returns the response headers.
func (c *client) cdnUpload(url string, data []byte) (encryptedParam string, err error) {
	hc := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("create CDN upload request: %w", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	c.logger.Info(context.Background(), "weixin-client: CDN POST", "addr", url, "bytes", len(data))
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("CDN upload: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("CDN upload client error %d: %s", resp.StatusCode, string(body))
	}

	encryptedParam = resp.Header.Get("x-encrypted-param")
	return encryptedParam, nil
}

// cdnDownload fetches encrypted media data from CDN.
func (c *client) cdnDownload(url string) ([]byte, error) {
	hc := &http.Client{Timeout: 60 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create CDN download request: %w", err)
	}

	c.logger.Info(context.Background(), "weixin-client: CDN GET", "addr", url)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("CDN download: %w", err)
	}
	defer resp.Body.Close()

	return io.ReadAll(resp.Body)
}

// --- High-level API methods ---

// getBotQRCode fetches the QR code for login (v2.3.1+: POST with local_token_list).
func (c *client) getBotQRCode(req *QRLoginRequest) (*QRCodeResponse, error) {
	if req == nil {
		req = &QRLoginRequest{}
	}
	return apiPost[QRCodeResponse](c, "/ilink/bot/get_bot_qrcode?bot_type=3", req, 30*time.Second)
}

// getQRCodeStatus polls QR scan status.
// verifyCode is an optional pair-code for the need_verifycode flow (v2.3.1+).
func (c *client) getQRCodeStatus(qrcode, verifyCode string) (*QRCodeStatusResponse, error) {
	path := "/ilink/bot/get_qrcode_status?qrcode=" + qrcode
	if verifyCode != "" {
		path += "&verify_code=" + verifyCode
	}
	return apiGet[QRCodeStatusResponse](c, path, c.qrStatusTimeout)
}

// getUpdates performs long-polling for new messages.
func (c *client) getUpdates(buf string) (*GetUpdatesResponse, error) {
	return apiPost[GetUpdatesResponse](c, "/ilink/bot/getupdates", GetUpdatesRequest{
		GetUpdatesBuf: buf,
		BaseInfo:      c.buildBaseInfo(),
	}, c.getUpdatesTimeout)
}

// sendMessage delivers a message to a WeChat user.
func (c *client) sendMessage(req *SendMessageRequest) (*SendMessageResponse, error) {
	return apiPost[SendMessageResponse](c, "/ilink/bot/sendmessage", req, c.sendMessageTimeout)
}

// getUploadURL obtains CDN upload credentials.
func (c *client) getUploadURL(req *GetUploadURLRequest) (*GetUploadURLResponse, error) {
	return apiPost[GetUploadURLResponse](c, "/ilink/bot/getuploadurl", req, c.getUploadURLTimeout)
}

// getConfig fetches bot configuration including typing_ticket.
func (c *client) getConfig(req *GetConfigRequest) (*GetConfigResponse, error) {
	return apiPost[GetConfigResponse](c, "/ilink/bot/getconfig", req, c.getConfigTimeout)
}

// sendTyping sends the typing indicator.
func (c *client) sendTyping(req *SendTypingRequest) error {
	_, err := apiPost[map[string]any](c, "/ilink/bot/sendtyping", req, c.sendTypingTimeout)
	return err
}

// notifyStart notifies the server that this channel client is starting (v2.1.10+).
func (c *client) notifyStart() (*NotifyStartResponse, error) {
	return apiPost[NotifyStartResponse](c, "/ilink/bot/msg/notifystart", NotifyStartRequest{
		BaseInfo: c.buildBaseInfo(),
	}, c.getConfigTimeout)
}

// notifyStop notifies the server that this channel client is stopping (v2.1.10+).
func (c *client) notifyStop() (*NotifyStopResponse, error) {
	return apiPost[NotifyStopResponse](c, "/ilink/bot/msg/notifystop", NotifyStopRequest{
		BaseInfo: c.buildBaseInfo(),
	}, c.getConfigTimeout)
}
