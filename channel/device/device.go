// Package device implements a browser-based "physical" channel for the agent.
//
// The device channel turns any browser (laptop, phone, tablet) into the
// agent's body: it serves a face page (animated eyes + mic + camera) and
// exposes JSON endpoints that the page calls to chat and to send images.
//
// Architecture:
//
//	┌──────────────┐  HTTP/JSON  ┌──────────────────────┐
//	│  browser      │ ←─────────→ │  DeviceChannel        │
//	│  face.html    │   /api/chat │  (this package)       │
//	│  eyes/mic/cam │   /api/vision                        │
//	└──────────────┘             └───────────┬────────────┘
//	                                        │ MessageHandler
//	                                     ┌──┴─────────────┐
//	                                     │  agent (LLM)    │
//	                                     └────────────────┘
//
// The page is served from the same origin as the API, so no CORS setup and no
// HTTPS is required on localhost (getUserMedia is allowed on secure contexts;
// localhost counts as secure).
//
// Session model: all requests share a single fixed ThreadID ("device"), so
// the agent keeps one persistent conversation across page reloads.
package device

import (
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/monsterxx03/tachi/pkg/channel"
)

//go:embed face.html
var faceFS embed.FS

// DeviceConfig holds the device channel's YAML configuration.
//
//	channel:
//	  channels:
//	    device:
//	      enabled: true
//	      host: 0.0.0.0
//	      port: 8080
//	      tts_url: http://127.0.0.1:8888   # Kokoro TTS 服务（可选）
type DeviceConfig struct {
	Host    string `yaml:"host"`
	Port    int    `yaml:"port"`
	TTSURL  string `yaml:"tts_url"`  // Kokoro TTS 服务地址，空则前端直连
	TTSVoice string `yaml:"tts_voice"` // 默认音色
	TTSSpeed float64 `yaml:"tts_speed"` // 语速
}

func init() {
	channel.Register("device", func(rawCfg map[string]any) (channel.Channel, error) {
		b, err := yaml.Marshal(rawCfg)
		if err != nil {
			return nil, fmt.Errorf("device: marshal config: %w", err)
		}
		cfg := DeviceConfig{Host: "0.0.0.0", Port: 8080, TTSVoice: "zf_027", TTSSpeed: 1.05}
		if err := yaml.Unmarshal(b, &cfg); err != nil {
			return nil, fmt.Errorf("device: unmarshal config: %w", err)
		}
		return NewChannel(cfg)
	})
}

// deviceThreadID is the fixed session key for the device channel. All
// browser requests map to this one thread, giving the agent persistent
// memory across conversations and reloads.
const deviceThreadID = "device"

// maxImageBytes caps incoming camera snapshots (10 MB).
const maxImageBytes = 10 << 20

type DeviceChannel struct {
	cfg     DeviceConfig
	handler channel.MessageHandler
	server  *http.Server
	ttsHTTP *http.Client
	ttsCache map[string]string // text -> base64 wav（简单去重，不缓存 TTS 服务）
}

func NewChannel(cfg DeviceConfig) (*DeviceChannel, error) {
	return &DeviceChannel{
		cfg: cfg,
		ttsHTTP: &http.Client{Timeout: 30 * time.Second},
		ttsCache: make(map[string]string),
	}, nil
}

func (d *DeviceChannel) Name() string { return "device" }

// ShowTurnSummary reports whether assistant replies should include the
// iteration/duration/trace footer. The device face page is a personified
// conversation UI — technical metadata would break the illusion, so the
// footer is suppressed.
func (d *DeviceChannel) ShowTurnSummary() bool { return false }

// SystemPromptSuffix returns extra system prompt instructions for the device
// channel. Replies on the face page are spoken aloud via browser TTS and
// shown in a compact chat log, so they must be short, plain text, and free
// of emoji/markdown (the TTS layer strips markdown symbols before reading).
func (d *DeviceChannel) SystemPromptSuffix() string {
	return `
## Device — Voice Conversation

You are talking to the user through a voice-enabled browser interface
(a face with eyes, microphone and camera). Your reply is read aloud by
text-to-speech, so write for the ear, not for the screen.

Rules:
- Keep replies SHORT — one or two sentences at most. This is a spoken
  conversation, not a document.
- Use plain text only. No emoji, no markdown formatting (bold, code
  blocks, headings, bullet lists).
- Be warm and conversational, like talking face to face.
- When the user sends a photo, describe what you see briefly and reply
  conversationally.
`
}

func (d *DeviceChannel) OnStart(ctx context.Context) error { return nil }

// synthSpeech 调本地 Kokoro TTS 把文本合成 wav（base64）。
// 返回空字符串表示 TTS 不可用（前端会自动降级到浏览器语音）。
func (d *DeviceChannel) synthSpeech(text string) string {
	if d.cfg.TTSURL == "" {
		return ""
	}
	clean := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, text)
	if len(clean) > 500 {
		clean = clean[:500]
	}
	if v, ok := d.ttsCache[clean]; ok {
		return v
	}
	payload, _ := json.Marshal(map[string]any{
		"text":  clean,
		"voice": d.cfg.TTSVoice,
		"speed": d.cfg.TTSSpeed,
	})
	req, err := http.NewRequest(http.MethodPost, d.cfg.TTSURL+"/tts", bytes.NewReader(payload))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.ttsHTTP.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	wav, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return ""
	}
	b64 := base64.StdEncoding.EncodeToString(wav)
	if len(d.ttsCache) < 512 { // 防内存膨胀
		d.ttsCache[clean] = b64
	}
	return b64
}

func (d *DeviceChannel) Run(ctx context.Context, handler channel.MessageHandler) error {
	d.handler = handler

	mux := http.NewServeMux()
	mux.HandleFunc("/", d.handleIndex)
	mux.HandleFunc("/api/chat", d.handleChat)
	mux.HandleFunc("/api/vision", d.handleVision)
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	addr := fmt.Sprintf("%s:%d", d.cfg.Host, d.cfg.Port)
	d.server = &http.Server{
		Addr:              addr,
		Handler:           withCORS(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- d.server.ListenAndServe() }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return d.server.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// handleIndex serves the face page.
func (d *DeviceChannel) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := faceFS.ReadFile("face.html")
	if err != nil {
		http.Error(w, "face.html not embedded", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

// handleChat accepts {message: "..."} and returns {reply: "..."}.
//
// The call blocks until the agent turn completes (LLM + tools), so the page
// should show a thinking state while awaiting the response.
func (d *DeviceChannel) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Message == "" {
		http.Error(w, "empty message", http.StatusBadRequest)
		return
	}

	result := d.handler(r.Context(), channel.IncomingMessage{
		ThreadID:  deviceThreadID,
		MessageID: uuid.NewString(),
		Content:   req.Message,
		Directed:  true,
	})

	reply := result.Reply.Content
	resp := map[string]any{
		"reply": reply,
		"ok":    result.Err == nil,
	}
	if result.Err != nil {
		resp["error"] = result.Err.Error()
	} else {
		// 服务端生成语音，随响应推给前端（TTS 不可用时 audio 为空，前端降级）
		if audio := d.synthSpeech(reply); audio != "" {
			resp["audio"] = audio
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(resp)
}

// handleVision accepts a multipart form (image file + optional message)
// and asks the agent to describe / react to the image.
func (d *DeviceChannel) handleVision(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxImageBytes)
	if err := r.ParseMultipartForm(maxImageBytes); err != nil {
		http.Error(w, "bad multipart form: "+err.Error(), http.StatusBadRequest)
		return
	}

	prompt := r.FormValue("message")
	if prompt == "" {
		prompt = "看看我，描述一下你看到的画面。"
	}

	f, _, err := r.FormFile("image")
	if err != nil {
		http.Error(w, "no image field: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, "read image: "+err.Error(), http.StatusBadRequest)
		return
	}

	result := d.handler(r.Context(), channel.IncomingMessage{
		ThreadID:  deviceThreadID,
		MessageID: uuid.NewString(),
		Content:   prompt,
		Directed:  true,
		Attachments: []channel.Attachment{{
			Type:     channel.AttachmentTypeImage,
			FileName: "camera.jpg",
			MimeType: http.DetectContentType(data),
			Content:  data,
		}},
	})

	reply := result.Reply.Content
	resp := map[string]any{
		"reply": reply,
		"ok":    result.Err == nil,
	}
	if result.Err != nil {
		resp["error"] = result.Err.Error()
	} else if audio := d.synthSpeech(reply); audio != "" {
		resp["audio"] = audio
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(resp)
}

// withCORS adds permissive CORS headers. Same-origin pages don't need them,
// but they make it trivial to open the page from another device later
// (e.g. a phone on the LAN hitting http://<laptop-ip>:8080).
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
