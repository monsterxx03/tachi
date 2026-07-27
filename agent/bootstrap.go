package agent

import (
	"context"
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// BootstrapResult holds the initialized resources after Bootstrap completes.
type BootstrapResult struct {
	// Config is the fully loaded and resolved configuration.
	Config *config.Config
}

// Bootstrap performs common initialization shared by all entry points:
// load config, init structured logger, start pprof (if enabled), and load
// MCP server configs from JSON files. Returns the loaded config.
//
// Call this at the top of every entry-point function so the boilerplate
// (config.Load + logger.Init + startPprof + LoadMCPServers) is written
// once and kept consistent across TUI, ACP, channel, and run modes.
func Bootstrap(ctx context.Context) (*BootstrapResult, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}
	if err := logger.Init(config.LogsDir(), cfg.Logs); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to init logger: %v\n", err)
	}
	cfg.Debug.PPROF.Port = startPprof(cfg.Debug.PPROF)
	if err := cfg.LoadMCPServers(config.FindProjectRoot()); err != nil {
		return nil, fmt.Errorf("failed to load MCP servers: %w", err)
	}
	return &BootstrapResult{Config: cfg}, nil
}

// startPprof starts a pprof HTTP server on 127.0.0.1:<port> if enabled in config.
// It tries cfg.Port first; if the port is taken (e.g., another Tachi instance),
// it auto-increments up to cfg.Port+100. Returns the actual port bound,
// or 0 if disabled or no port could be bound.
// The server runs in a background goroutine.
func startPprof(cfg config.PprofConfig) int {
	if !cfg.Enabled {
		return 0
	}
	log := logger.New("pprof")
	for port := cfg.Port; port <= cfg.Port+100; port++ {
		addr := fmt.Sprintf("127.0.0.1:%d", port)
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			go func() {
				log.Info(context.Background(), "pprof server started", "addr", addr)
				if err := http.Serve(ln, nil); err != nil {
					log.Error(context.Background(), "pprof server error", err)
				}
			}()
			return port
		}
	}
	log.Warn(context.Background(), "pprof failed to bind any port",
		"start", cfg.Port, "end", cfg.Port+100)
	return 0
}
