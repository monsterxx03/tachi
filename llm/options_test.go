package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestApplyOptions verifies ProviderOption funcs mutate the expected fields
// and that unset fields stay at their zero values.
func TestApplyOptions(t *testing.T) {
	got := applyOptions([]ProviderOption{
		WithMaxRetries(5),
		WithTimeout(30 * time.Second),
	})
	if got.MaxRetries == nil || *got.MaxRetries != 5 {
		t.Fatalf("WithMaxRetries(5): got %v, want 5", got.MaxRetries)
	}
	if got.Timeout != 30*time.Second {
		t.Fatalf("WithTimeout(30s): got %v, want 30s", got.Timeout)
	}

	// No options → zero values (MaxRetries nil = use default).
	empty := applyOptions(nil)
	if empty.MaxRetries != nil {
		t.Fatalf("applyOptions(nil).MaxRetries = %v, want nil", empty.MaxRetries)
	}
	if empty.Timeout != 0 {
		t.Fatalf("applyOptions(nil).Timeout = %v, want 0", empty.Timeout)
	}

	// Explicit 0 is distinguishable from unset.
	zero := applyOptions([]ProviderOption{WithMaxRetries(0)})
	if zero.MaxRetries == nil || *zero.MaxRetries != 0 {
		t.Fatalf("WithMaxRetries(0): got %v, want 0", zero.MaxRetries)
	}

	// nil options are skipped safely.
	safe := applyOptions([]ProviderOption{nil, WithTimeout(time.Second)})
	if safe.Timeout != time.Second {
		t.Fatalf("nil option should be skipped; got Timeout %v", safe.Timeout)
	}
}

// TestNewOpenAIProvider_Timeout verifies the WithTimeout option lands on the
// HTTP client: a slow upstream that would otherwise hang fails fast with a
// timeout error, while a client without the option completes.
func TestNewOpenAIProvider_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(80 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0}}`))
	}))
	defer srv.Close()

	msg := []Message{{Role: "user", Content: "hi"}}

	// Without a timeout the slow server succeeds.
	noTimeout := NewOpenAIProvider("sk", srv.URL, "m")
	if _, err := noTimeout.CreateChat(context.Background(), msg, nil, ChatOptions{}); err != nil {
		t.Fatalf("provider without timeout should succeed, got: %v", err)
	}

	// With a 10ms timeout the same call fails fast.
	withTimeout := NewOpenAIProvider("sk", srv.URL, "m", WithTimeout(10*time.Millisecond))
	_, err := withTimeout.CreateChat(context.Background(), msg, nil, ChatOptions{})
	if err == nil {
		t.Fatal("provider with 10ms timeout should fail on a 80ms server, got nil error")
	}
	// go-openai surfaces the HTTP client timeout as
	// "request canceled (Client.Timeout exceeded while awaiting headers)".
	if !strings.Contains(err.Error(), "Client.Timeout exceeded") {
		t.Fatalf("error should be timeout-related, got: %v", err)
	}
}

// TestNewOpenAIProvider_RetryMax verifies the retry count consumed by
// NewNamedProvider: WithMaxRetries overrides the legacy default of 2, and
// explicit 0 disables retrying.
func TestNewOpenAIProvider_RetryMax(t *testing.T) {
	cases := []struct {
		name string
		opts []ProviderOption
		want int
	}{
		{name: "default", opts: nil, want: 2},
		{name: "override", opts: []ProviderOption{WithMaxRetries(5)}, want: 5},
		{name: "disabled", opts: []ProviderOption{WithMaxRetries(0)}, want: 0},
		{name: "negative-clamped", opts: []ProviderOption{WithMaxRetries(-3)}, want: 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := NewNamedProvider(ProviderTypeOpenAI, "p", "sk", "", "m", c.opts...)
			if err != nil {
				t.Fatalf("NewNamedProvider: %v", err)
			}
			rp, ok := p.(*RetryProvider)
			if !ok {
				t.Fatalf("openai provider should be wrapped in RetryProvider, got %T", p)
			}
			if got := rp.Cfg().MaxRetries; got != c.want {
				t.Errorf("MaxRetries = %d, want %d", got, c.want)
			}
		})
	}
}
