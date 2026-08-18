package main

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/web"
)

// runWeb implements `tachi web`: start the local web console server.
// The browser is never opened automatically — users open the printed URL
// themselves.
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

	fmt.Printf("Tachi web console: http://%s\n", addr)
	if cfg.Web.APIKey != "" {
		fmt.Println("API key auth enabled (set in config.yaml web.api_key)")
	} else {
		fmt.Println("API key auth disabled (set config.yaml web.api_key to enable)")
	}

	return srv.ListenAndServe(ctx, addr)
}
