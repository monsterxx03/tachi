package device

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/pkg/channel"
)

// echoHandler returns a fixed reply for any incoming message.
func echoHandler(ctx context.Context, msg channel.IncomingMessage) channel.HandlerResult {
	return channel.HandlerResult{
		Reply: channel.OutgoingMessage{
			ThreadID: msg.ThreadID,
			Content:  "收到: " + msg.Content,
		},
	}
}

func startTestServer(t *testing.T) string {
	t.Helper()
	ch, err := NewChannel(DeviceConfig{Host: "127.0.0.1", Port: 18080})
	if err != nil {
		t.Fatalf("NewChannel: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ch.Run(ctx, echoHandler) }()
	t.Cleanup(func() { cancel(); <-done })

	base := "http://127.0.0.1:18080"
	// Wait for the server to come up.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/api/health")
		if err == nil {
			resp.Body.Close()
			return base
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("server did not start in time")
	return ""
}

func TestServeFacePage(t *testing.T) {
	base := startTestServer(t)
	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !bytes.Contains(body, []byte("face")) {
		t.Errorf("face page does not contain canvas markup")
	}
}

func TestChatEndpoint(t *testing.T) {
	base := startTestServer(t)
	body, _ := json.Marshal(map[string]string{"message": "你好"})
	resp, err := http.Post(base+"/api/chat", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/chat: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Reply string `json:"reply"`
		OK    bool   `json:"ok"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.OK || out.Reply != "收到: 你好" {
		t.Errorf("unexpected reply: %+v", out)
	}
}

func TestVisionEndpoint(t *testing.T) {
	base := startTestServer(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("image", "camera.jpg")
	fw.Write([]byte("fake-jpeg-bytes"))
	mw.WriteField("message", "看看我")
	mw.Close()

	resp, err := http.Post(base+"/api/vision", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatalf("POST /api/vision: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Reply string `json:"reply"`
		OK    bool   `json:"ok"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.OK || out.Reply != "收到: 看看我" {
		t.Errorf("unexpected reply: %+v", out)
	}
}
