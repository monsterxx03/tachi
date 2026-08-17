// Package web implements the local web console (`tachi web`): a read-only
// dashboard over sessions, one-off transcripts and the usage ledger.
package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"io/fs"
	"log"
	"net/http"
	"path/filepath"
	"slices"
	"strings"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// Server hosts the web console HTTP endpoints.
type Server struct {
	Cfg   config.WebConfig
	Store *session.FileStore
	Usage *llm.UsageRecorder
}

// New creates a Server backed by the configured Tachi state dirs.
func New(cfg config.WebConfig) (*Server, error) {
	sessDir, err := config.SessionDir()
	if err != nil {
		return nil, err
	}
	store, err := session.NewFileStore(sessDir)
	if err != nil {
		return nil, err
	}
	return &Server{
		Cfg:   cfg,
		Store: store,
		Usage: llm.NewUsageRecorder(config.UsageDir()),
	}, nil
}

// Handler returns the full HTTP handler (API + embedded static assets)
// wrapped with auth and CORS middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// API routes (Go 1.22+ method+path patterns).
	mux.HandleFunc("GET /api/sessions", s.handleListSessions)
	mux.HandleFunc("GET /api/sessions/{id}", s.handleGetSession)
	mux.HandleFunc("GET /api/sessions/{id}/oneoff", s.handleListSessionOneOffs)
	mux.HandleFunc("GET /api/sessions/{id}/oneoff/{file}", s.handleGetOneOff)
	mux.HandleFunc("GET /api/oneoff", s.handleListGlobalOneOffs)
	mux.HandleFunc("GET /api/usage", s.handleUsage)
	mux.HandleFunc("GET /api/sessions/{id}/transcript", s.handleTranscript)

	// Frontend assets served as an SPA: unknown non-/api paths fall back to
	// index.html so client-side routes (/sessions/:id) work on refresh.
	mux.Handle("/", newSPAHandler(staticFS()))

	var h http.Handler = mux
	h = s.withCORS(h)
	h = s.withAuth(h)
	return h
}

// apiKeyMatches reports whether the request carries a valid X-Api-Key.
// When no key is configured, auth is disabled entirely.
func (s *Server) apiKeyMatches(header string) bool {
	if s.Cfg.APIKey == "" {
		return true
	}
	if header == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(header), []byte(s.Cfg.APIKey)) == 1
}

// withAuth guards /api/* with the configured API key.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) >= 4 && r.URL.Path[:4] == "/api" {
			if !s.apiKeyMatches(r.Header.Get("X-Api-Key")) {
				writeJSONError(w, http.StatusUnauthorized, "invalid or missing API key")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// withCORS allows the configured dev origins (Vite dev server) to call the
// API directly; same-origin requests are unaffected.
func (s *Server) withCORS(next http.Handler) http.Handler {
	if len(s.Cfg.AllowedOrigins) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if slices.Contains(s.Cfg.AllowedOrigins, origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Api-Key")
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ListenAndServe runs the server until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: s.Handler()}
	errCh := make(chan error, 1)
	go func() {
		log.Printf("tachi web: listening on http://%s", addr)
		errCh <- srv.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		return srv.Shutdown(context.Background())
	case err := <-errCh:
		return err
	}
}

// ── embedded static assets ────────────────────────────────────────────────

//go:embed dist
var distFS embed.FS

// distSub returns the embedded frontend build as an fs.FS, or an empty FS
// when the dist directory is absent (no frontend build happened yet).
func staticFS() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return emptyFS{}
	}
	return sub
}

// spaHandler serves static files and falls back to index.html for any
// path that has no file extension (assumed to be a client-side route), so
// BrowserRouter deep links work on refresh.
type spaHandler struct {
	fsys   fs.FS
	server http.Handler
	index  []byte
}

func newSPAHandler(fsys fs.FS) http.Handler {
	index, _ := fs.ReadFile(fsys, "index.html")
	fshr := http.FileServer(http.FS(fsys))
	return &spaHandler{fsys: fsys, server: fshr, index: index}
}

func (h *spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Root → serve as-is (FileServer maps "/" to index.html, but be explicit).
	if r.URL.Path == "/" || r.URL.Path == "" {
		h.server.ServeHTTP(w, r)
		return
	}
	// A path with an extension is a real static asset (js/css/fonts...);
	// serve it as-is.
	if ext := filepath.Ext(r.URL.Path); ext != "" {
		h.server.ServeHTTP(w, r)
		return
	}
	// Extension-less path: a client-side route. Check for a real file
	// (e.g. a directory entry or a dot-route), else fall back to index.html.
	if _, err := fs.Stat(h.fsys, strings.TrimPrefix(r.URL.Path, "/")); err == nil {
		h.server.ServeHTTP(w, r)
		return
	}
	// Fall back to the SPA shell.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(h.index)
}

// emptyFS serves nothing (404) — used when dist/ has no files.
type emptyFS struct{}

func (emptyFS) Open(string) (fs.File, error) { return nil, fs.ErrNotExist }
