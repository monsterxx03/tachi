package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/sashabaranov/go-openai"
)

// stubProvider returns errors from a script; nil (or past-the-end) entries
// mean success. Call counts are recorded for assertions.
type stubProvider struct {
	chatErrs    []error
	streamErrs  []error
	chatCalls   int
	streamCalls int
}

func (s *stubProvider) Name() string  { return "stub" }
func (s *stubProvider) Model() string { return "stub-model" }

func (s *stubProvider) CreateChat(_ context.Context, _ []Message, _ []Tool, _ ChatOptions) (*Response, error) {
	i := s.chatCalls
	s.chatCalls++
	if i < len(s.chatErrs) && s.chatErrs[i] != nil {
		return nil, s.chatErrs[i]
	}
	return &Response{Content: "ok", FinishReason: "stop"}, nil
}

func (s *stubProvider) CreateChatStream(_ context.Context, _ []Message, _ []Tool, _ ChatOptions) (<-chan StreamEvent, error) {
	i := s.streamCalls
	s.streamCalls++
	if i < len(s.streamErrs) && s.streamErrs[i] != nil {
		return nil, s.streamErrs[i]
	}
	ch := make(chan StreamEvent, 1)
	ch <- StreamEvent{Type: StreamEventDone, FinishReason: "stop"}
	close(ch)
	return ch, nil
}

// fastConfig keeps tests quick: 1ms base delay.
var fastConfig = RetryConfig{MaxRetries: 2, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}

func netResetErr() error {
	return &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
}

func TestRetryProvider_CreateChat_SuccessFirstTry(t *testing.T) {
	stub := &stubProvider{}
	p := NewRetryProvider(stub, fastConfig)

	resp, err := p.CreateChat(context.Background(), nil, nil, ChatOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("unexpected content: %q", resp.Content)
	}
	if stub.chatCalls != 1 {
		t.Fatalf("expected 1 call, got %d", stub.chatCalls)
	}
}

func TestRetryProvider_CreateChat_RetryThenSuccess(t *testing.T) {
	stub := &stubProvider{chatErrs: []error{netResetErr()}}
	p := NewRetryProvider(stub, fastConfig)

	resp, err := p.CreateChat(context.Background(), nil, nil, ChatOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("unexpected content: %q", resp.Content)
	}
	if stub.chatCalls != 2 {
		t.Fatalf("expected 2 calls, got %d", stub.chatCalls)
	}
}

func TestRetryProvider_CreateChat_NonRetryableFailsFast(t *testing.T) {
	authErr := &openai.APIError{HTTPStatusCode: 401, Message: "unauthorized"}
	stub := &stubProvider{chatErrs: []error{authErr, authErr, authErr}}
	p := NewRetryProvider(stub, fastConfig)

	_, err := p.CreateChat(context.Background(), nil, nil, ChatOptions{})
	if !errors.Is(err, authErr) {
		t.Fatalf("expected authErr, got %v", err)
	}
	if stub.chatCalls != 1 {
		t.Fatalf("expected 1 call (no retry on 401), got %d", stub.chatCalls)
	}
}

func TestRetryProvider_CreateChat_ExhaustsRetries(t *testing.T) {
	stub := &stubProvider{chatErrs: []error{netResetErr(), netResetErr(), netResetErr(), netResetErr()}}
	p := NewRetryProvider(stub, fastConfig)

	_, err := p.CreateChat(context.Background(), nil, nil, ChatOptions{})
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	// 1 initial + MaxRetries retries.
	if stub.chatCalls != fastConfig.MaxRetries+1 {
		t.Fatalf("expected %d calls, got %d", fastConfig.MaxRetries+1, stub.chatCalls)
	}
}

func TestRetryProvider_CreateChat_ZeroMaxRetries(t *testing.T) {
	stub := &stubProvider{chatErrs: []error{netResetErr()}}
	p := NewRetryProvider(stub, RetryConfig{MaxRetries: 0, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond})

	_, err := p.CreateChat(context.Background(), nil, nil, ChatOptions{})
	if err == nil {
		t.Fatal("expected error with MaxRetries=0")
	}
	if stub.chatCalls != 1 {
		t.Fatalf("expected 1 call with MaxRetries=0, got %d", stub.chatCalls)
	}
}

func TestRetryProvider_CreateChatStream_RetryThenSuccess(t *testing.T) {
	stub := &stubProvider{streamErrs: []error{io.ErrUnexpectedEOF}}
	p := NewRetryProvider(stub, fastConfig)

	ch, err := p.CreateChatStream(context.Background(), nil, nil, ChatOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ev := <-ch
	if ev.Type != StreamEventDone {
		t.Fatalf("unexpected event type: %s", ev.Type)
	}
	if stub.streamCalls != 2 {
		t.Fatalf("expected 2 calls, got %d", stub.streamCalls)
	}
}

func TestRetryProvider_CreateChatStream_NonRetryableFailsFast(t *testing.T) {
	badReq := &openai.APIError{HTTPStatusCode: 400, Message: "bad request"}
	stub := &stubProvider{streamErrs: []error{badReq, badReq}}
	p := NewRetryProvider(stub, fastConfig)

	_, err := p.CreateChatStream(context.Background(), nil, nil, ChatOptions{})
	if !errors.Is(err, badReq) {
		t.Fatalf("expected badReq, got %v", err)
	}
	if stub.streamCalls != 1 {
		t.Fatalf("expected 1 call (no retry on 400), got %d", stub.streamCalls)
	}
}

func TestRetryProvider_ContextCanceledDuringBackoff(t *testing.T) {
	stub := &stubProvider{chatErrs: []error{netResetErr(), netResetErr(), netResetErr()}}
	// Slow backoff so the cancel lands mid-sleep.
	p := NewRetryProvider(stub, RetryConfig{MaxRetries: 2, BaseDelay: 500 * time.Millisecond, MaxDelay: time.Second})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	_, err := p.CreateChat(ctx, nil, nil, ChatOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if stub.chatCalls != 1 {
		t.Fatalf("expected 1 call (canceled during backoff), got %d", stub.chatCalls)
	}
}

func TestRetryProvider_Passthrough(t *testing.T) {
	stub := &stubProvider{}
	p := NewRetryProvider(stub, fastConfig)
	if p.Name() != "stub" || p.Model() != "stub-model" {
		t.Fatalf("Name/Model not passed through: %s/%s", p.Name(), p.Model())
	}
}

func TestIsRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context canceled", context.Canceled, false},
		{"context deadline", context.DeadlineExceeded, false},
		{"wrapped context canceled", fmt.Errorf("call: %w", context.Canceled), false},
		{"net op error (conn reset)", netResetErr(), true},
		{"dns error", &net.DNSError{IsTimeout: true}, true},
		{"io EOF", io.EOF, true},
		{"io unexpected EOF", io.ErrUnexpectedEOF, true},
		{"wrapped EOF", fmt.Errorf("stream: %w", io.EOF), true},
		{"openai 429", &openai.APIError{HTTPStatusCode: 429}, true},
		{"openai 500", &openai.APIError{HTTPStatusCode: 500}, true},
		{"openai 503", &openai.APIError{HTTPStatusCode: 503}, true},
		{"openai 408", &openai.APIError{HTTPStatusCode: 408}, true},
		{"openai 409", &openai.APIError{HTTPStatusCode: 409}, true},
		{"openai 400", &openai.APIError{HTTPStatusCode: 400}, false},
		{"openai 401", &openai.APIError{HTTPStatusCode: 401}, false},
		{"openai 403", &openai.APIError{HTTPStatusCode: 403}, false},
		{"openai 404", &openai.APIError{HTTPStatusCode: 404}, false},
		{"openai request error 429", &openai.RequestError{HTTPStatusCode: 429}, true},
		{"openai request error 400", &openai.RequestError{HTTPStatusCode: 400}, false},
		{"anthropic 429", &anthropic.Error{StatusCode: 429}, true},
		{"anthropic 500", &anthropic.Error{StatusCode: 500}, true},
		{"anthropic 403", &anthropic.Error{StatusCode: 403}, false},
		{"generic error", errors.New("boom"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryable(tt.err); got != tt.want {
				t.Errorf("isRetryable(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestRetryProvider_Backoff(t *testing.T) {
	p := NewRetryProvider(&stubProvider{}, RetryConfig{
		MaxRetries: 5,
		BaseDelay:  100 * time.Millisecond,
		MaxDelay:   250 * time.Millisecond,
	})
	want := []time.Duration{100, 200, 250, 250, 250} // ms, capped
	for i, w := range want {
		if got := p.backoff(i + 1); got != w*time.Millisecond {
			t.Errorf("backoff(%d) = %v, want %v", i+1, got, w*time.Millisecond)
		}
	}
}

func TestNewRetryProvider_Defaults(t *testing.T) {
	p := NewRetryProvider(&stubProvider{}, RetryConfig{MaxRetries: 2})
	if p.cfg.BaseDelay != 500*time.Millisecond {
		t.Errorf("default BaseDelay = %v, want 500ms", p.cfg.BaseDelay)
	}
	if p.cfg.MaxDelay != 8*time.Second {
		t.Errorf("default MaxDelay = %v, want 8s", p.cfg.MaxDelay)
	}
}
