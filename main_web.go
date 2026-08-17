package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"

	"github.com/urfave/cli/v3"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/web"
)

// runWeb implements `tachi web`: start the local web console server and
// (unless --no-open) open the browser.
func runWeb(ctx context.Context, cmd *cli.Command) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	addr := cmd.String("addr")
	if addr == "" {
		addr = cfg.Web.Addr
	}
	// Config default may be empty; fall back to a sane localhost port.
	if addr == "" {
		addr = "127.0.0.1:8787"
	}

	srv, err := web.New(cfg.Web)
	if err != nil {
		return fmt.Errorf("init web server: %w", err)
	}

	if !cmd.Bool("no-open") {
		if host, port, err := net.SplitHostPort(addr); err == nil {
			if host == "" || host == "0.0.0.0" || host == "::" {
				host = "127.0.0.1"
			}
			url := fmt.Sprintf("http://%s:%s/", host, port)
			if err := openBrowser(url); err != nil {
				fmt.Fprintf(os.Stderr, "open browser: %v (server still running on %s)\n", err, addr)
			}
		}
	}

	fmt.Printf("Tachi web console: http://%s\n", addr)
	if cfg.Web.APIKey != "" {
		fmt.Println("API key auth enabled (set in config.yaml web.api_key)")
	} else {
		fmt.Println("API key auth disabled (set config.yaml web.api_key to enable)")
	}

	return srv.ListenAndServe(ctx, addr)
}

// openBrowser opens url in the system default browser (same behavior as the
// transcript report's OpenInBrowser helper).
func openBrowser(url string) error {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
		args = []string{url}
	case "linux":
		cmd = "xdg-open"
		args = []string{url}
	case "windows":
		cmd = "rundll32"
		args = []string{"url.dll,FileProtocolHandler", url}
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
	return exec.Command(cmd, args...).Start()
}
