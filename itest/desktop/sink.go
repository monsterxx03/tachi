package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// Result is what one driver reports back: the verdict the runner turns into an exit code
// (the driver also draws the same lines as a banner, so a human looking at the window sees
// them too).
type Result struct {
	Scenario string   `json:"scenario"`
	Lines    []Line   `json:"lines"`
	Error    string   `json:"error,omitempty"`   // a JS exception that ended the run early
	Done     bool     `json:"done"`              // false = the driver died mid-way (watchdog)
	MS       int      `json:"ms"`
	Missing  []string `json:"missing,omitempty"` // selectors the driver waited for and never saw
	// Console is everything the page logged, riding in the SAME body as the verdict.
	// It used to be a second POST to /console, and that POST races the runner: /result is
	// what unblocks it, and it reads the console right away — so a console error recorded
	// while the driver was still asserting was almost always lost, which is exactly the
	// line that explains a strange failure.
	Console []string `json:"console,omitempty"`
}

// Line is one assertion or one informational log line.
type Line struct {
	Label  string `json:"label"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// failed reports whether this line counts against the scenario.
func (l Line) failed() bool { return !l.OK }

// Failures returns the failing lines (empty when the driver passed).
func (r Result) Failures() []Line {
	var out []Line
	for _, l := range r.Lines {
		if l.failed() {
			out = append(out, l)
		}
	}
	return out
}

// OK reports whether the driver's own assertions all passed.
func (r Result) OK() bool {
	return len(r.Failures()) == 0 && r.Error == "" && r.Done
}

// sink is the loopback endpoint a driver POSTs its Result to. The webview can reach it
// (same machine), and it keeps the driver free of any Go-side knowledge.
//
// It also collects the driver's console output, which is where a WebKit-side error shows
// up when the page never gets far enough to report anything itself.
type sink struct {
	url     string
	mu      sync.Mutex
	result   *Result
	progress []Line
	console  []string
	dom      []byte
	done    chan struct{}
	srv     *http.Server
}

func newSink() (*sink, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("sink: listen: %w", err)
	}
	s := &sink{url: "http://" + ln.Addr().String(), done: make(chan struct{}, 1)}

	mux := http.NewServeMux()
	mux.HandleFunc("/result", func(w http.ResponseWriter, r *http.Request) {
		var res Result
		if err := json.NewDecoder(r.Body).Decode(&res); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		if s.result == nil {
			s.result = &res
			// The console rides in the result, so the runner can read both after the same
			// POST — see Result.Console. /console stays for a driver that dies before
			// reporting (it has nowhere else to put them).
			if len(res.Console) > 0 {
				s.console = append(s.console, res.Console...)
			}
			close(s.done)
		}
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	// /line receives each assertion as it is recorded. A driver that never finishes still
	// leaves its progress behind, which is the difference between "the app broke" and
	// "the driver broke" when a run times out.
	mux.HandleFunc("/line", func(w http.ResponseWriter, r *http.Request) {
		var l Line
		if err := json.NewDecoder(r.Body).Decode(&l); err == nil {
			s.mu.Lock()
			s.progress = append(s.progress, l)
			s.mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// /dom receives the page's HTML at verdict time: the rendered structure, the driver's
	// banner and every text the assertions looked at. A picture would be nicer, but the
	// window-capture APIs are closed on macOS 15+ and the DOM is right there in the page
	// that ran the asserts.
	mux.HandleFunc("/dom", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
		if err == nil && len(body) > 0 {
			s.mu.Lock()
			s.dom = body
			s.mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/console", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Lines []string `json:"lines"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			s.mu.Lock()
			s.console = append(s.console, body.Lines...)
			s.mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// Any other request is a driver bug worth seeing rather than a 404 in the dark.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.console = append(s.console, "unexpected request: "+r.Method+" "+r.URL.Path)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	})

	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = s.srv.Serve(ln) }()
	return s, nil
}

// wait blocks until the driver reports or the budget runs out. A second return of false
// means "no report" — the runner then reports a timeout with whatever console lines the
// driver managed to send.
func (s *sink) wait(timeout time.Duration) (Result, bool) {
	select {
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return *s.result, true
	case <-time.After(timeout):
		return Result{}, false
	}
}

// lines returns the assertions seen so far, in order.
func (s *sink) lines() []Line {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Line, len(s.progress))
	copy(out, s.progress)
	return out
}

func (s *sink) consoleLines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.console))
	copy(out, s.console)
	return out
}

// domSnapshot returns the page HTML the driver sent, if any.
func (s *sink) domSnapshot() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dom
}

func (s *sink) close() { _ = s.srv.Close() }
