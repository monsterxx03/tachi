package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/urfave/cli/v3"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/session"
	"github.com/monsterxx03/tachi/tui"

	_ "github.com/monsterxx03/tachi/channel/discord"
	_ "github.com/monsterxx03/tachi/channel/github"
	_ "github.com/monsterxx03/tachi/channel/weixin"
)

// Version is set via ldflags at build time:
//
//	go build -ldflags="-X main.Version=$(git describe --tags --always --dirty)" .
var Version = "dev"

func buildSystemPrompt(cfg *config.Config) string {
	return agent.BuildSystemPrompt(cfg.Language, "", "", cfg.ExtraSystemPrompt)
}

var commonFlags = []cli.Flag{
	&cli.StringFlag{
		Name:  "home",
		Usage: "Base directory for tachi state (default: ~/.tachi)",
	},
}

func main() {
	llm.Version = Version

	app := &cli.Command{
		Name:    "tachi",
		Usage:   "AI Agent CLI",
		Version: Version,
		Flags: append(commonFlags,
			&cli.BoolFlag{
				Name:    "resume",
				Aliases: []string{"r"},
				Usage:   "Resume the most recent session",
			},
			&cli.BoolFlag{
				Name:    "edit",
				Aliases: []string{"e"},
				Usage:   "Open config file in editor",
			},
			&cli.StringFlag{
				Name:    "prompt",
				Aliases: []string{"p"},
				Usage:   "User prompt — runs in non-interactive mode (stdin content appended when piped)",
			},
			&cli.StringFlag{
				Name:    "output-format",
				Aliases: []string{"o"},
				Usage:   "Output format: text (default) | json | json-stream",
			},
			&cli.BoolFlag{
				Name:    "quiet",
				Aliases: []string{"q"},
				Usage:   "Suppress progress output to stderr (auto-enabled when stdout is piped)",
			},
			&cli.StringFlag{
				Name:  "allowed-tools",
				Usage: "Comma-separated whitelist of tool names the agent may use",
			},
			&cli.StringFlag{
				Name:  "disallowed-tools",
				Usage: "Comma-separated blacklist of tool names the agent may NOT use",
			},
			&cli.DurationFlag{
				Name:  "timeout",
				Usage: "Maximum execution time (e.g. 5m, 30s, 1h)",
			},
			&cli.BoolFlag{
				Name:    "commit",
				Aliases: []string{"c"},
				Usage:   "Generate a git commit and commit changes (like /commit in TUI)",
			},
		),
		Before: func(ctx context.Context, cmd *cli.Command) (context.Context, error) {
			if cmd.IsSet("home") {
				config.SetBaseDir(cmd.String("home"))
			}
			return ctx, nil
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			// Commit mode: --commit / -c flag (like /commit in TUI).
			if cmd.IsSet("commit") {
				return runCommit(ctx, cmd)
			}
			// Detect whether stdin is being piped (non-terminal).
			isPiped := false
			stat, err := os.Stdin.Stat()
			if err == nil && (stat.Mode()&os.ModeCharDevice) == 0 {
				isPiped = true
			}
			// Non-interactive run mode: --prompt flag set or stdin is piped.
			if cmd.IsSet("prompt") || isPiped {
				return runAgent(ctx, cmd)
			}
			return runTUI(ctx, cmd)
		},
		Commands: []*cli.Command{
			{
				Name:  "init",
				Usage: "Initialize example config",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					path, err := config.Init()
					if err != nil {
						return err
					}
					fmt.Printf("Config created: %s\n", path)
					fmt.Println("Edit the file to set your API keys and provider settings.")
					return nil
				},
			},
			{
				Name:   "channel",
				Usage:  "Start all enabled channels from config (e.g., weixin)",
				Flags:  commonFlags,
				Action: runChannels,
			},
			{
				Name:  "acp",
				Usage: "Run as ACP agent (JSON-RPC 2.0 over stdio)",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return runACPAgent(ctx)
				},
			},
			{
				Name:   "usage",
				Usage:  "Show total cost from the usage ledger (all-time + per-day)",
				Action: runUsage,
			},
			{
				Name:  "transcript",
				Usage: "Visualize session transcripts",
				Commands: []*cli.Command{
					{
						Name:   "list",
						Usage:  "List all sessions with transcript data",
						Action: transcriptList,
					},
					{
						Name:  "show",
						Usage: "Generate HTML report for a session transcript",
						Flags: []cli.Flag{
							&cli.StringFlag{
								Name:    "session",
								Aliases: []string{"s"},
								Usage:   "Session ID to visualize",
							},
							&cli.BoolFlag{
								Name:    "latest",
								Aliases: []string{"l"},
								Usage:   "Show the most recent session",
							},
							&cli.BoolFlag{
								Name:    "no-open",
								Aliases: []string{"n"},
								Usage:   "Don't open browser, just print path",
							},
						},
						Action: transcriptShow,
					},
				},
			},
			{
				Name:  "web",
				Usage: "Start the local web console (sessions / usage / oneoffs)",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:  "addr",
						Usage: "Listen address (default: config.yaml web.addr or 127.0.0.1:8787)",
					},
				},
				Action: runWeb,
			},
		},
	}

	if err := app.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runTUI(ctx context.Context, cmd *cli.Command) error {
	// If -e/--edit flag is set, open the config file in the default editor and exit.
	if cmd.Bool("edit") {
		path, err := config.ConfigPath()
		if err != nil {
			return fmt.Errorf("config path: %w", err)
		}
		editor := os.Getenv("EDITOR")
		if editor == "" {
			for _, candidate := range []string{"nvim", "vim", "vi", "code"} {
				if p, err := exec.LookPath(candidate); err == nil {
					editor = p
					break
				}
			}
		}
		if editor == "" {
			return fmt.Errorf("no editor found; set $EDITOR or install one of: vi, nano, vim, code")
		}
		editCmd := exec.Command(editor, path)
		editCmd.Stdin = os.Stdin
		editCmd.Stdout = os.Stdout
		editCmd.Stderr = os.Stderr
		if err := editCmd.Run(); err != nil {
			return fmt.Errorf("editor failed: %w", err)
		}
		return nil
	}

	boot, err := agent.Bootstrap(ctx)
	if err != nil {
		return err
	}
	cfg := boot.Config

	// TUI is interactive — no iteration budget cap (0 = unlimited).
	// Resolved (main provider + resolved config) is built inside the
	// constructor from FullConfig's default provider.
	aiAgent, mcpMgr, err := agent.NewAIAgentWithConfig(ctx, agent.AgentConfig{
		Logger:         logger.New("tui"),
		PermissionMode: agent.PermissionModeTUI,
		FullConfig:     cfg,
		SystemConfig:   agent.SystemConfigFromConfig(cfg),
	})
	if err != nil {
		return err
	}
	if mcpMgr != nil {
		defer mcpMgr.Close()
	}
	defer aiAgent.Close()

	providerInfo := fmt.Sprintf("%s (%s)", aiAgent.Provider().Name(), aiAgent.Model())

	var initialSessionList []*session.Session

	if cmd.Bool("resume") {
		sm, err := session.NewManager(nil)
		if err != nil {
			return fmt.Errorf("session manager: %w", err)
		}
		sm.SetMaxKeep(cfg.SessionCleanupMaxCount)
		sm.CleanupOldSessions()
		aiAgent.SetSessionManager(sm)

		sessions, err := sm.List()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to list sessions: %v\n", err)
		}
		initialSessionList = sessions
	} else {
		sm, err := session.NewManager(nil)
		if err != nil {
			fmt.Printf("Warning: failed to init session manager: %v\n", err)
		} else {
			sm.SetMaxKeep(cfg.SessionCleanupMaxCount)
			sm.CleanupOldSessions()
			aiAgent.SetSessionManager(sm)
		}
	}

	return tui.Run(tui.ModelConfig{
		Agent:        aiAgent,
		SystemPrompt: buildSystemPrompt(cfg),
		ChatOpts: llm.ChatOptions{
			MaxTokens: cfg.MaxTokens,
		},
		ProviderInfo:       providerInfo,
		Config:             cfg,
		ContextWindow:      aiAgent.ContextWindow(),
		InitialSessionList: initialSessionList,
		MCPManager:         mcpMgr,
		MCPServers:         cfg.MCPServers,
	})
}

// exitCodeForReason maps agent exit reasons to Unix exit codes.
func exitCodeForReason(reason string) int {
	switch reason {
	case agent.ExitReasonStop:
		return 0
	case agent.ExitReasonBudgetExhausted, agent.ExitReasonLengthExhausted:
		return 2
	case agent.ExitReasonInterrupted:
		return 130 // standard SIGINT exit code
	default: // ExitReasonError, ExitReasonCancelled, etc.
		return 1
	}
}
