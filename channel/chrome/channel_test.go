package chrome

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/pkg/channel"
)

// writeNM writes a ChromeRequest (Native Messaging format) to a writer.
func writeNM(w io.Writer, req ChromeRequest) error {
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	header := make([]byte, 4)
	binary.LittleEndian.PutUint32(header, uint32(len(data)))
	if _, err := w.Write(header); err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// readNM reads a ChromeResponse (Native Messaging format) from a reader.
func readNM(r io.Reader) (ChromeResponse, error) {
	var length uint32
	if err := binary.Read(r, binary.LittleEndian, &length); err != nil {
		return ChromeResponse{}, err
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return ChromeResponse{}, err
	}
	var resp ChromeResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return ChromeResponse{}, err
	}
	return resp, nil
}

// encodeNM encodes a ChromeRequest into a byte slice (Native Messaging format).
func encodeNM(req ChromeRequest) []byte {
	data, _ := json.Marshal(req)
	header := make([]byte, 4)
	binary.LittleEndian.PutUint32(header, uint32(len(data)))
	return append(header, data...)
}

// bufferedPipe is a thread-safe buffered IO pair.
// Write sends data to a channel; Read receives from it.
// Multiple writes are queued; reads are served from an internal buffer.
type bufferedPipe struct {
	ch   chan []byte
	mu   sync.Mutex
	buf  []byte
}

func newBufferedPipe(capacity int) *bufferedPipe {
	return &bufferedPipe{
		ch: make(chan []byte, capacity),
	}
}

func (bp *bufferedPipe) Write(p []byte) (int, error) {
	chunk := make([]byte, len(p))
	copy(chunk, p)
	bp.ch <- chunk
	return len(p), nil
}

func (bp *bufferedPipe) Read(p []byte) (int, error) {
	bp.mu.Lock()
	if len(bp.buf) == 0 {
		bp.mu.Unlock()
		chunk, ok := <-bp.ch
		if !ok {
			return 0, io.EOF
		}
		bp.mu.Lock()
		bp.buf = chunk
	}
	n := copy(p, bp.buf)
	bp.buf = bp.buf[n:]
	bp.mu.Unlock()
	return n, nil
}

func TestReadWriteMessage(t *testing.T) {
	var buf bytes.Buffer
	ch := NewChromeChannelWithIO("chrome", &buf, io.Discard)

	req := ChromeRequest{
		ID:       "test-1",
		Action:   "explain",
		ThreadID: "tab_123",
		Selection: struct {
			Text  string `json:"text"`
			URL   string `json:"url,omitempty"`
			Title string `json:"title,omitempty"`
		}{
			Text:  "ReAct",
			URL:   "https://example.com",
			Title: "Example Page",
		},
	}

	if err := writeNM(&buf, req); err != nil {
		t.Fatalf("writeNM: %v", err)
	}

	got, err := ch.readMessage()
	if err != nil {
		t.Fatalf("readMessage: %v", err)
	}

	if got.ID != req.ID {
		t.Errorf("ID = %q, want %q", got.ID, req.ID)
	}
	if got.Action != req.Action {
		t.Errorf("Action = %q, want %q", got.Action, req.Action)
	}
	if got.ThreadID != req.ThreadID {
		t.Errorf("ThreadID = %q, want %q", got.ThreadID, req.ThreadID)
	}
	if got.Selection.Text != req.Selection.Text {
		t.Errorf("Selection.Text = %q, want %q", got.Selection.Text, req.Selection.Text)
	}
}

func TestPingPong(t *testing.T) {
	// Pre-encode the ping request into a bytes.Reader.
	input := bytes.NewReader(encodeNM(ChromeRequest{
		ID:       "ping-1",
		Action:   "ping",
		ThreadID: "global",
	}))

	tachiToExt := newBufferedPipe(10)
	ch := NewChromeChannelWithIO("chrome", input, tachiToExt)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- ch.Run(ctx, func(_ context.Context, msg channel.IncomingMessage) channel.HandlerResult {
			t.Errorf("handler should not be called for ping")
			return channel.HandlerResult{
				Reply: channel.OutgoingMessage{
					ThreadID: msg.ThreadID,
					Content:  "unexpected call",
				},
			}
		})
	}()

	// Read pong response from tachiToExt.
	resp, err := readNM(tachiToExt)
	if err != nil {
		t.Fatalf("read pong: %v", err)
	}
	if resp.ID != "ping-1" {
		t.Errorf("resp.ID = %q, want %q", resp.ID, "ping-1")
	}
	if resp.Content != "pong" {
		t.Errorf("resp.Content = %q, want %q", resp.Content, "pong")
	}

	// Let Run finish (after ping, the input reader has EOF, so Run exits cleanly).
	runErr := <-errCh
	if runErr != nil {
		t.Errorf("Run returned error: %v", runErr)
	}
}

func TestHandlerInvocation(t *testing.T) {
	input := bytes.NewReader(encodeNM(ChromeRequest{
		ID:       "req-1",
		Action:   "explain",
		ThreadID: "tab_456",
		Selection: struct {
			Text  string `json:"text"`
			URL   string `json:"url,omitempty"`
			Title string `json:"title,omitempty"`
		}{
			Text:  "ReAct",
			URL:   "https://react.dev",
			Title: "ReAct Framework",
		},
	}))

	tachiToExt := newBufferedPipe(10)
	ch := NewChromeChannelWithIO("chrome", input, tachiToExt)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handlerCalled := make(chan struct{}, 1)
	var capturedMsg channel.IncomingMessage

	errCh := make(chan error, 1)
	go func() {
		errCh <- ch.Run(ctx, func(_ context.Context, msg channel.IncomingMessage) channel.HandlerResult {
			capturedMsg = msg
			handlerCalled <- struct{}{}
			return channel.HandlerResult{
				Reply: channel.OutgoingMessage{
					ThreadID: msg.ThreadID,
					Content:  "Here's the explanation of ReAct...",
				},
			}
		})
	}()

	<-handlerCalled

	if capturedMsg.ThreadID != "tab_456" {
		t.Errorf("ThreadID = %q, want %q", capturedMsg.ThreadID, "tab_456")
	}
	if capturedMsg.MessageID != "req-1" {
		t.Errorf("MessageID = %q, want %q", capturedMsg.MessageID, "req-1")
	}
	if capturedMsg.Content == "" {
		t.Errorf("Content should not be empty")
	}

	resp, err := readNM(tachiToExt)
	if err != nil {
		t.Fatalf("readNM: %v", err)
	}
	if resp.ID != "req-1" {
		t.Errorf("resp.ID = %q, want %q", resp.ID, "req-1")
	}
	if resp.Type != "result" {
		t.Errorf("resp.Type = %q, want %q", resp.Type, "result")
	}
	if resp.Content != "Here's the explanation of ReAct..." {
		t.Errorf("resp.Content = %q, want %q", resp.Content, "Here's the explanation of ReAct...")
	}

	cancel()
	<-errCh
}

func TestBuildPrompt(t *testing.T) {
	ch := NewChromeChannel("chrome")

	t.Run("summarize returns content", func(t *testing.T) {
		req := ChromeRequest{
			Action:  "summarize",
			Content: "请总结此页面内容...\n\n---\n\n页面正文...",
		}
		req.Selection.Title = "Example Page"
		req.Selection.URL = "https://example.com"

		prompt := ch.buildPrompt(req)
		if prompt != req.Content {
			t.Errorf("summarize action should return Content as-is\nGot: %q\nWant: %q", prompt, req.Content)
		}
	})

	t.Run("default with content", func(t *testing.T) {
		req := ChromeRequest{
			Action:  "unknown_action",
			Content: "some custom content",
		}
		req.Selection.Text = "fallback text"

		prompt := ch.buildPrompt(req)
		if prompt != "some custom content" {
			t.Errorf("default with content should return Content\nGot: %q", prompt)
		}
	})

	t.Run("default without content returns selection text", func(t *testing.T) {
		req := ChromeRequest{
			Action: "unknown_action",
		}
		req.Selection.Text = "selected text"

		prompt := ch.buildPrompt(req)
		if prompt != "selected text" {
			t.Errorf("default without content should return Selection.Text\nGot: %q", prompt)
		}
	})

	t.Run("default with empty content and selection returns empty", func(t *testing.T) {
		req := ChromeRequest{
			Action: "unknown_action",
		}

		prompt := ch.buildPrompt(req)
		if prompt != "" {
			t.Errorf("default with everything empty should return empty\nGot: %q", prompt)
		}
	})
}

func TestSend(t *testing.T) {
	tachiToExt := newBufferedPipe(10)
	ch := NewChromeChannelWithIO("chrome", nil, tachiToExt)

	if err := ch.Send(context.Background(), channel.OutgoingMessage{
		ThreadID: "tab_123",
		Content:  "proactive notification",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	resp, err := readNM(tachiToExt)
	if err != nil {
		t.Fatalf("readNM: %v", err)
	}
	if resp.Type != "result" {
		t.Errorf("resp.Type = %q, want %q", resp.Type, "result")
	}
	if resp.Content != "proactive notification" {
		t.Errorf("resp.Content = %q, want %q", resp.Content, "proactive notification")
	}
}

func TestHandlerError(t *testing.T) {
	input := bytes.NewReader(encodeNM(ChromeRequest{
		ID:       "err-1",
		Action:   "explain",
		ThreadID: "tab_789",
		Selection: struct {
			Text  string `json:"text"`
			URL   string `json:"url,omitempty"`
			Title string `json:"title,omitempty"`
		}{Text: "test"},
	}))

	tachiToExt := newBufferedPipe(10)
	ch := NewChromeChannelWithIO("chrome", input, tachiToExt)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- ch.Run(ctx, func(_ context.Context, msg channel.IncomingMessage) channel.HandlerResult {
			return channel.HandlerResult{
				Err: fmt.Errorf("something went wrong"),
			}
		})
	}()

	resp, err := readNM(tachiToExt)
	if err != nil {
		t.Fatalf("readNM: %v", err)
	}
	if resp.Type != "error" {
		t.Errorf("resp.Type = %q, want %q", resp.Type, "error")
	}
	if resp.ID != "err-1" {
		t.Errorf("resp.ID = %q, want %q", resp.ID, "err-1")
	}
	if !contains(resp.Content, "something went wrong") {
		t.Errorf("resp.Content should contain error, got: %s", resp.Content)
	}

	cancel()
	<-errCh
}

func TestEOF(t *testing.T) {
	ch := NewChromeChannelWithIO("chrome", new(bytes.Buffer), io.Discard)
	err := ch.Run(context.Background(), nil)
	if err != nil {
		t.Errorf("Run returned error on EOF: %v", err)
	}
}

func TestSteeredResult(t *testing.T) {
	input := bytes.NewReader(encodeNM(ChromeRequest{
		ID:       "steer-1",
		Action:   "ask_tachi",
		ThreadID: "tab_999",
		Selection: struct {
			Text  string `json:"text"`
			URL   string `json:"url,omitempty"`
			Title string `json:"title,omitempty"`
		}{Text: "follow up"},
		Content: "also add tests",
	}))

	tachiToExt := newBufferedPipe(10)
	ch := NewChromeChannelWithIO("chrome", input, tachiToExt)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- ch.Run(ctx, func(_ context.Context, msg channel.IncomingMessage) channel.HandlerResult {
			return channel.HandlerResult{
				Steered: true,
			}
		})
	}()

	// Let Run consume the steered message (no output → loop → EOF from bytes.Reader).
	time.Sleep(10 * time.Millisecond)
	cancel()
	<-errCh
}

func contains(s, substr string) bool {
	return len(substr) == 0 || (len(s) >= len(substr) && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
