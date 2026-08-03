package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/urfave/cli/v3"

	acp "github.com/coder/acp-go-sdk"

	"github.com/monsterxx03/tachi/agent"
	acppkg "github.com/monsterxx03/tachi/agent/acp"
	"github.com/monsterxx03/tachi/agent/transcript/render"
	channelmgr "github.com/monsterxx03/tachi/channel/manager"
	"github.com/monsterxx03/tachi/pkg/channel"
	"github.com/monsterxx03/tachi/pkg/strutil"
	"github.com/monsterxx03/tachi/session"
)

func runChannels(ctx context.Context, cmd *cli.Command) error {
	boot, err := agent.Bootstrap(ctx)
	if err != nil {
		return err
	}
	cfg := boot.Config

	mgr := channelmgr.New(channelmgr.Config{
		Cfg: cfg,
	})

	active := cfg.Channel.ActiveChannels()
	if len(active) == 0 {
		return fmt.Errorf("no channels enabled in config; enable at least one channel")
	}

	// Instantiate channels from registry.
	registered := channel.ListRegistered()
	instantiated := 0
	for name, rawCfg := range active {
		factory, ok := registered[name]
		if !ok {
			fmt.Fprintf(os.Stderr, "[channel] WARNING: %q enabled in config but no factory registered (import its package?)\n", name)
			continue
		}

		ch, err := factory(rawCfg)
		if err != nil {
			return fmt.Errorf("channel %q: create: %w", name, err)
		}

		mgr.Add(ch)
		instantiated++
		fmt.Fprintf(os.Stderr, "[channel] %s registered\n", name)
	}

	// Verify at least one channel was instantiated.
	if instantiated == 0 {
		names := make([]string, 0, len(active))
		for name := range active {
			names = append(names, name)
		}
		return fmt.Errorf("no channel factories registered for any enabled channel: %v", names)
	}

	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("channel manager start: %w", err)
	}

	// Block until context is cancelled OR all channels have exited.
	// Channels like WeChat exit when stdin is closed or the connection drops;
	// waiting for ctx.Done() alone would leave zombie processes.
	select {
	case <-ctx.Done():
	case <-mgr.Done():
	}

	fmt.Fprintln(os.Stderr, "[channel] shutting down...")
	mgr.Close()
	return nil
}

// ── ACP Agent ────────────────────────────────────────────────────────────────

func runACPAgent(ctx context.Context) error {
	boot, err := agent.Bootstrap(ctx)
	if err != nil {
		return err
	}
	cfg := boot.Config

	fmt.Fprintf(os.Stderr, "tachi: ACP agent started (version %s)\n", Version)

	// Create TachiAgent (AIAgent instances are created per-session in NewSession)
	tachiAgent := acppkg.NewTachiAgent(cfg, Version)
	defer tachiAgent.CloseAll()

	// Start SDK connection (blocks until stdin EOF)
	conn := acp.NewAgentSideConnection(tachiAgent, os.Stdout, os.Stdin)
	tachiAgent.SetConnection(conn)

	// Wait for connection to end (editor closed, stdin EOF)
	<-conn.Done()
	fmt.Fprintf(os.Stderr, "tachi: ACP agent shutting down\n")
	return nil
}

// ── Transcript visualization commands ────────────────────────────────────────

func transcriptList(ctx context.Context, cmd *cli.Command) error {
	mgr, err := session.NewManager(nil)
	if err != nil {
		return fmt.Errorf("session manager: %w", err)
	}

	sessions, err := mgr.List()
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}

	fmt.Printf("%-40s  %-20s  %s\n", "SESSION ID", "DATE", "TITLE")
	fmt.Println(strings.Repeat("─", 100))
	for _, s := range sessions {
		date := s.CreatedAt.Format(strutil.TimeFormatDateTimeShort)
		fmt.Printf("%-40s  %-20s  %s\n", s.ID, date, s.Title)
	}
	fmt.Printf("\n%d sessions total.\n", len(sessions))
	fmt.Println("Use: tachi transcript show --session <id>    (or --latest)")
	return nil
}

func transcriptShow(ctx context.Context, cmd *cli.Command) error {
	mgr, err := session.NewManager(nil)
	if err != nil {
		return fmt.Errorf("session manager: %w", err)
	}

	var sess *session.Session

	if cmd.Bool("latest") {
		sessions, err := mgr.List()
		if err != nil {
			return fmt.Errorf("list sessions: %w", err)
		}
		if len(sessions) == 0 {
			return fmt.Errorf("no sessions found")
		}
		sess, err = mgr.Load(sessions[0].ID)
		if err != nil {
			return fmt.Errorf("load session: %w", err)
		}
	} else if id := cmd.String("session"); id != "" {
		sess, err = mgr.Load(id)
		if err != nil {
			return fmt.Errorf("load session %q: %w", id, err)
		}
	} else {
		return fmt.Errorf("specify --session <id> or --latest")
	}

	// Load messages for this session.
	msgs, err := mgr.LoadMessages()
	if err != nil {
		return fmt.Errorf("load messages: %w", err)
	}
	if len(msgs) == 0 {
		return fmt.Errorf("session %q has no messages yet; run a conversation first", sess.ID)
	}

	// Sub-agent sidecar messages are optional — a load failure is non-fatal.
	subagents, _ := mgr.LoadSubagentMessages(sess.ID)

	// Build report data from session messages (transcript is replaced by session).
	data := render.BuildReportDataFromMessages(sess, msgs, subagents)
	html, err := render.GenerateHTML(data)
	if err != nil {
		return fmt.Errorf("generate HTML: %w", err)
	}

	if cmd.Bool("no-open") {
		// Write to stdout-compatible path
		tmpDir := os.TempDir()
		filename := filepath.Join(tmpDir, fmt.Sprintf("tachi-transcript-%s.html", sess.ID[:8]))
		if err := os.WriteFile(filename, []byte(html), 0644); err != nil {
			return fmt.Errorf("write file: %w", err)
		}
		fmt.Println(filename)
		return nil
	}

	path, err := render.OpenInBrowser(html, sess.ID)
	if err != nil {
		return fmt.Errorf("open browser: %w\n\nHTML saved to: %s", err, path)
	}
	fmt.Printf("Transcript: %s\nOpened: %s\n", sess.Title, path)
	return nil
}
