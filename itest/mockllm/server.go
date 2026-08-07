package mockllm

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
)

// Server is a scripted mock LLM server. Requests are matched against a
// pre-registered script (Step list) in arrival order; each step's Require
// precondition is evaluated against the FULL parsed request before its Reply
// is served. Any mismatch (precondition failure or script exhaustion) is
// recorded as the server's error and the request gets an HTTP 500 — scenarios
// assert mock.Error() == nil to fail fast on agent-loop regressions.
type Server struct {
	mu       sync.Mutex
	protocol Protocol
	steps    []Step
	next     int
	requests []*RecordedRequest
	err      error

	srv *httptest.Server
}

// Option configures a Server.
type Option func(*Server)

// WithProtocol selects the wire protocol (default: ProtocolOpenAI).
func WithProtocol(p Protocol) Option {
	return func(s *Server) { s.protocol = p }
}

// NewServer starts a mock LLM server bound to a random 127.0.0.1 port.
func NewServer(opts ...Option) *Server {
	s := &Server{protocol: ProtocolOpenAI}
	for _, o := range opts {
		o(s)
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// handle routes one request through the script.
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method != http.MethodPost {
		s.fail(fmt.Errorf("mock: unexpected method %s %s", r.Method, r.URL.Path))
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.pathAllowed(r.URL.Path) {
		s.fail(fmt.Errorf("mock: unexpected path %s (protocol %s)", r.URL.Path, s.protocol))
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.fail(fmt.Errorf("mock: read body: %w", err))
		http.Error(w, "read body failed", http.StatusInternalServerError)
		return
	}
	req, err := normalizeRequest(r.Method, r.URL.Path, r.Header, body)
	if err != nil {
		s.fail(fmt.Errorf("mock: parse request body: %w\nbody: %s", err, truncateForError(body)))
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Consume the next script step under the lock.
	s.mu.Lock()
	if s.err != nil {
		s.mu.Unlock()
		http.Error(w, "mock already failed", http.StatusInternalServerError)
		return
	}
	if s.next >= len(s.steps) {
		reason := fmt.Sprintf("mock: script exhausted (%d requests, %d steps); "+
			"agent loop called the LLM more times than scripted.\nlast request: %s",
			s.next+1, len(s.steps), summarizeRequest(req))
		s.err = fmt.Errorf("%s", reason)
		s.mu.Unlock()
		http.Error(w, reason, http.StatusInternalServerError)
		return
	}
	step := s.steps[s.next]
	s.next++
	s.requests = append(s.requests, req)
	s.mu.Unlock()

	// Evaluate the precondition against the full request, then serve.
	if step.Require != nil {
		if reason := step.Require(req); reason != "" {
			s.fail(fmt.Errorf("mock: step %d precondition failed: %s\nrequest: %s",
				s.next, reason, summarizeRequest(req)))
			http.Error(w, "precondition failed: "+reason, http.StatusPreconditionFailed)
			return
		}
	}
	if step.Reply != nil {
		step.Reply(ctx, w, s.protocol)
		return
	}
	// No reply — treat as a plain 200 empty body.
	w.WriteHeader(http.StatusOK)
}

// pathAllowed reports whether the request path belongs to this server's
// protocol. base_url carries the /v1 prefix in both providers, so OpenAI
// lands on /v1/chat/completions and Anthropic on /v1/messages.
func (s *Server) pathAllowed(path string) bool {
	if s.protocol == ProtocolAnthropic {
		return path == "/v1/messages"
	}
	return path == "/v1/chat/completions"
}

// fail records the first fatal error (script exhaustion / precondition
// failure / malformed request). Only the first error is kept.
func (s *Server) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = err
	}
}

// Script registers the interaction script. It must be called before any
// request arrives; requests after the script is exhausted fail the server.
func (s *Server) Script(steps ...Step) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steps = append(s.steps, steps...)
}

// URL returns the server base URL without the /v1 prefix (e.g.
// "http://127.0.0.1:54321").
func (s *Server) URL() string { return s.srv.URL }

// BaseURL returns the provider-facing base URL to put in the config
// fixture's base_url field. The two SDKs disagree on the /v1 prefix:
//   - go-openai concatenates baseURL + "/chat/completions", so OpenAI needs
//     the explicit "/v1" suffix.
//   - anthropic-sdk-go appends a trailing "/" to a non-root base path and
//     then resolves "v1/messages" against it, so a "/v1" suffix would
//     produce "/v1/v1/messages" — Anthropic needs the bare host.
func (s *Server) BaseURL() string {
	if s.protocol == ProtocolAnthropic {
		return s.srv.URL
	}
	return s.srv.URL + "/v1"
}

// Protocol returns the server's wire protocol.
func (s *Server) Protocol() Protocol { return s.protocol }

// Requests returns a copy of all recorded requests, in arrival order.
func (s *Server) Requests() []*RecordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*RecordedRequest, len(s.requests))
	copy(out, s.requests)
	return out
}

// RequestCount returns the number of requests received.
func (s *Server) RequestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// Error returns the first fatal error (nil when the script was consumed
// cleanly). Scenarios assert Error() == nil after the run completes.
func (s *Server) Error() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Close shuts the server down, unblocking any Hold() replies whose contexts
// were not already cancelled.
func (s *Server) Close() {
	if s.srv != nil {
		s.srv.Close()
	}
}

// summarizeRequest renders a one-line digest of a request for failure dumps.
func summarizeRequest(req *RecordedRequest) string {
	return fmt.Sprintf("%s %s (%d messages, %d tools)",
		req.Method, req.Path, len(req.Messages), len(req.Tools))
}

// truncateForError caps body dumps inside error messages.
func truncateForError(b []byte) string {
	const max = 2000
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "...(truncated)"
}
