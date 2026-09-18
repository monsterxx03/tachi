package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/config"
)

// TestApplyOptions verifies config.ProviderOption funcs mutate the expected fields
// and that unset fields stay at their zero values.
func TestApplyOptions(t *testing.T) {
	got := applyOptions([]config.ProviderOption{
		config.WithMaxRetries(5),
		config.WithTimeout(30 * time.Second),
		config.WithProxy("http://127.0.0.1:7890"),
	})
	if got.MaxRetries == nil || *got.MaxRetries != 5 {
		t.Fatalf("config.WithMaxRetries(5): got %v, want 5", got.MaxRetries)
	}
	if got.Timeout != 30*time.Second {
		t.Fatalf("config.WithTimeout(30s): got %v, want 30s", got.Timeout)
	}
	if got.Proxy != "http://127.0.0.1:7890" {
		t.Fatalf("config.WithProxy: got %q, want http://127.0.0.1:7890", got.Proxy)
	}

	// No options → zero values (MaxRetries nil = use default).
	empty := applyOptions(nil)
	if empty.MaxRetries != nil {
		t.Fatalf("applyOptions(nil).MaxRetries = %v, want nil", empty.MaxRetries)
	}
	if empty.Timeout != 0 {
		t.Fatalf("applyOptions(nil).Timeout = %v, want 0", empty.Timeout)
	}
	if empty.Proxy != "" {
		t.Fatalf("applyOptions(nil).Proxy = %q, want empty (no explicit proxy)", empty.Proxy)
	}

	// Explicit 0 is distinguishable from unset.
	zero := applyOptions([]config.ProviderOption{config.WithMaxRetries(0)})
	if zero.MaxRetries == nil || *zero.MaxRetries != 0 {
		t.Fatalf("config.WithMaxRetries(0): got %v, want 0", zero.MaxRetries)
	}

	// nil options are skipped safely.
	safe := applyOptions([]config.ProviderOption{nil, config.WithTimeout(time.Second)})
	if safe.Timeout != time.Second {
		t.Fatalf("nil option should be skipped; got Timeout %v", safe.Timeout)
	}
}

// TestNewOpenAIProvider_Timeout verifies the config.WithTimeout option lands on the
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
	withTimeout := NewOpenAIProvider("sk", srv.URL, "m", config.WithTimeout(10*time.Millisecond))
	_, err := withTimeout.CreateChat(context.Background(), msg, nil, ChatOptions{})
	if err == nil {
		t.Fatal("provider with 10ms timeout should fail on a 80ms server, got nil error")
	}
	// Assert the timeout semantically rather than by message text. net/http
	// surfaces an http.Client timeout either bare ("context deadline exceeded")
	// or annotated ("... (Client.Timeout exceeded while awaiting headers)"),
	// depending on an internal scheduling race when a custom RoundTripper
	// (tachiTransport) is installed — see net/http client.go setRequestCancel.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error should be timeout-related, got: %v", err)
	}
}

// proxyTestUpstream is what the proxied providers below point at: port 1 on
// loopback refuses connections immediately, so a request that does NOT go
// through the proxy fails instead of hanging.
const proxyTestUpstream = "http://127.0.0.1:1"

// TestProviderProxyRouting verifies config.WithProxy actually routes API calls
// through the proxy — on all three paths, because each installs the HTTP client
// its own way (go-openai's ClientConfig, the two SDKs' option.WithHTTPClient),
// so a path that dropped the option would still pass a test written for another.
//
// The proxy is an httptest server speaking HTTP-proxy: it records the
// absolute-form request URI, whose Host names the UPSTREAM — the proof the
// request came through it, given that the upstream itself is a dead port. Each
// path is served a valid response for its own protocol, so a 200 also proves the
// answer travelled back.
func TestProviderProxyRouting(t *testing.T) {
	paths := []struct {
		name string
		body string
		make func(opts ...config.ProviderOption) Provider
	}{
		{
			name: "openai",
			body: `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
			make: func(opts ...config.ProviderOption) Provider {
				return NewOpenAIProvider("sk", proxyTestUpstream, "m", opts...)
			},
		},
		{
			name: "anthropic",
			body: `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
			make: func(opts ...config.ProviderOption) Provider {
				return NewAnthropicProvider("sk", proxyTestUpstream, "m", opts...)
			},
		},
		{
			name: "openai-res",
			body: completedResponseJSON(
				`{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ok","annotations":[]}]}`,
				10, 5),
			make: func(opts ...config.ProviderOption) Provider {
				return NewOpenAIResponsesProvider("sk", proxyTestUpstream, "m", opts...)
			},
		},
	}

	for _, path := range paths {
		t.Run(path.name, func(t *testing.T) {
			proxy, hits := newRecordingProxy(t, path.body)
			msgs := []Message{{Role: "user", Content: "hi"}}

			// Sanity first: with no proxy option the call reaches the dead
			// upstream and fails, so a success below cannot be the upstream.
			// Retries are off for it: connection-refused fails on the first
			// attempt, and the SDK paths' default backoff would only make this
			// subtest slow.
			if _, err := path.make(config.WithMaxRetries(0)).CreateChat(t.Context(), msgs, nil, ChatOptions{}); err == nil {
				t.Fatal("a call without a proxy should fail against the dead upstream")
			}
			if got := hits(); len(got) != 0 {
				t.Fatalf("a call without a proxy reached the proxy: %v", got)
			}

			resp, err := path.make(config.WithProxy(proxy.URL)).CreateChat(t.Context(), msgs, nil, ChatOptions{})
			if err != nil {
				t.Fatalf("call through the proxy: %v", err)
			}
			if resp.Content != "ok" {
				t.Errorf("content = %q, want the proxy's answer (ok)", resp.Content)
			}
			got := hits()
			if len(got) != 1 {
				t.Fatalf("proxy hits = %v, want exactly 1", got)
			}
			if !strings.HasPrefix(got[0], "127.0.0.1:1/") {
				t.Errorf("proxied request target = %q, want the upstream host 127.0.0.1:1", got[0])
			}
		})
	}

	// The same wiring one level up: a provider resolved from a config whose
	// spec carries the proxy. spec → config.WithProxy is the mapping the option
	// tests above cannot see (providerOptionsFromSpec), and it is what a user's
	// config.yaml actually goes through.
	t.Run("spec-proxy", func(t *testing.T) {
		proxy, hits := newRecordingProxy(t, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)

		cfg := testConfig(t)
		cfg.Providers[0] = config.ProviderConfig{
			Name:    "p",
			Type:    "openai",
			Model:   "m",
			APIKey:  "sk-test",
			BaseURL: proxyTestUpstream,
			Spec:    config.ModelSpec{Proxy: proxy.URL},
		}
		resolved, err := BuildProvider(cfg, "p")
		if err != nil {
			t.Fatalf("BuildProvider: %v", err)
		}
		if _, err := resolved.Provider.CreateChat(t.Context(),
			[]Message{{Role: "user", Content: "hi"}}, nil, ChatOptions{}); err != nil {
			t.Fatalf("call through the spec's proxy: %v", err)
		}
		if got := hits(); len(got) != 1 {
			t.Fatalf("proxy hits = %v, want exactly 1", got)
		}
	})
}

// newRecordingProxy starts an HTTP-proxy test server that answers every request
// with body and records "<upstream host><path>" for each one it carried. The
// returned function reads the recording under the lock the handler writes it
// with (the handler runs on the server's goroutine).
func newRecordingProxy(t *testing.T, body string) (*httptest.Server, func() []string) {
	t.Helper()
	var (
		mu   sync.Mutex
		hits []string
	)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.URL.Host+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(proxy.Close)
	return proxy, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), hits...)
	}
}

// TestNewOpenAIProvider_RetryMax verifies the retry count consumed by
// NewNamedProvider: config.WithMaxRetries overrides the legacy default of 2, and
// explicit 0 disables retrying.
func TestNewOpenAIProvider_RetryMax(t *testing.T) {
	cases := []struct {
		name string
		opts []config.ProviderOption
		want int
	}{
		{name: "default", opts: nil, want: 2},
		{name: "override", opts: []config.ProviderOption{config.WithMaxRetries(5)}, want: 5},
		{name: "disabled", opts: []config.ProviderOption{config.WithMaxRetries(0)}, want: 0},
		{name: "negative-clamped", opts: []config.ProviderOption{config.WithMaxRetries(-3)}, want: 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := NewNamedProvider(config.ProviderTypeOpenAI, "p", "sk", "", "m", c.opts...)
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
