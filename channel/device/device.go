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
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
type DeviceConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

func init() {
	channel.Register("device", func(rawCfg map[string]any) (channel.Channel, error) {
		b, err := yaml.Marshal(rawCfg)
		if err != nil {
			return nil, fmt.Errorf("device: marshal config: %w", err)
		}
		cfg := DeviceConfig{Host: "0.0.0.0", Port: 8080}
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
}

func NewChannel(cfg DeviceConfig) (*DeviceChannel, error) {
	return &DeviceChannel{cfg: cfg}, nil
}

func (d *DeviceChannel) Name() string { return "device" }

func (d *DeviceChannel) OnStart(ctx context.Context) error { return nil }

func (d *DeviceChannel) Run(ctx context.Context, handler channel.MessageHandler) error {
	d.handler = handler

	mux := http.NewServeMux()
	mux.HandleFunc("/", d.handleIndex)
	mux.HandleFunc("/api/chat", d.handleChat)
	mux.HandleFunc("/api/vision", d.handleVision)
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
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
	w.Write(data)
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

	resp := map[string]any{
		"reply": result.Reply.Content,
		"ok":    result.Err == nil,
	}
	if result.Err != nil {
		resp["error"] = result.Err.Error()
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

	resp := map[string]any{
		"reply": result.Reply.Content,
		"ok":    result.Err == nil,
	}
	if result.Err != nil {
		resp["error"] = result.Err.Error()
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
