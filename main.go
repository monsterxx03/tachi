package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	acp "github.com/coder/acp-go-sdk"

	"github.com/monsterxx03/tachi/agent"
	acppkg "github.com/monsterxx03/tachi/agent/acp"
	"github.com/monsterxx03/tachi/agent/transcript/render"
	channelmgr "github.com/monsterxx03/tachi/channel/manager"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/channel"
	"github.com/monsterxx03/tachi/pkg/debuglog"
	"github.com/monsterxx03/tachi/session"
	"github.com/monsterxx03/tachi/tui"

	_ "github.com/monsterxx03/tachi/channel/weixin"
)

// Version is set via ldflags at build time:
//
//	go build -ldflags="-X main.Version=$(git describe --tags --always --dirty)" .
var Version = "dev"

func buildSystemPrompt(language string) string {
	return agent.BuildSystemPrompt(language, "")
}

var commonFlags = []cli.Flag{
	&cli.BoolFlag{
		Name:    "resume",
		Aliases: []string{"r"},
		Usage:   "Resume the most recent session",
	},
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
				Name:    "edit",
				Aliases: []string{"e"},
				Usage:   "Open config file in editor",
			},
		),
		Before: func(ctx context.Context, cmd *cli.Command) (context.Context, error) {
			if cmd.IsSet("home") {
				config.SetBaseDir(cmd.String("home"))
			}
			return ctx, nil
		},
		Action: runTUI,
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
				Name:  "run",
				Usage: "Run the AI agent (single-turn)",
				Flags: append(commonFlags,
					&cli.StringFlag{
						Name:    "prompt",
						Aliases: []string{"p"},
						Usage:   "User prompt to send",
					},
					&cli.BoolFlag{
						Name:  "json",
						Usage: "Output structured JSON instead of human-readable text",
					},
					&cli.DurationFlag{
						Name:  "timeout",
						Usage: "Maximum execution time (e.g. 5m, 30s, 1h)",
					},
				),
				Action: runAgent,
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
		},
	}

	if err := app.Run(context.Background(), os.Args); err != nil {
		log.Fatal(err)
	}
}

func resolveProviderFromConfig(cfg *config.Config) (llm.Provider, *config.ResolvedConfig, error) {
	resolved, err := config.Resolve(cfg)
	if err != nil {
		return nil, nil, err
	}

	provider, err := llm.NewProvider(
		resolved.Provider.Type,
		resolved.Provider.APIKey,
		resolved.Provider.BaseURL,
		resolved.Provider.Model,
	)
	if err != nil {
		return nil, nil, err
	}

	return provider, resolved, nil
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

	if err := debuglog.Init(config.LogsDir()); err != nil {
		fmt.Printf("Warning: failed to init debug log: %v\n", err)
	}
	defer debuglog.Close()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Load MCP server config from JSON files (project-level overrides global).
	if err := cfg.LoadMCPServers(config.FindProjectRoot()); err != nil {
		return fmt.Errorf("failed to load MCP servers: %w", err)
	}

	provider, resolved, err := resolveProviderFromConfig(cfg)
	if err != nil {
		return err
	}

	// TUI is interactive — no iteration budget cap (0 = unlimited).
	aiAgent := agent.NewAIAgent(provider, resolved.Provider.Model, 0)
	aiAgent.SetSkipEditConfirm(cfg.TUI.SkipEditConfirm)
	aiAgent.SetContextWindow(resolved.Provider.ContextWindow)
	aiAgent.SetupTitleProvider(cfg)
	aiAgent.SetupCommitProvider(cfg)

	mcpMgr, err := aiAgent.Configure(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: agent configuration error: %v\n", err)
	}
	if mcpMgr != nil {
		defer mcpMgr.Close()
	}
	defer aiAgent.Close()

	providerInfo := fmt.Sprintf("%s (%s)", resolved.Provider.Type, resolved.Provider.Model)

	var initialSessionList []*session.Session

	if cmd.Bool("resume") {
		sm, err := session.NewManager()
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
		sm, err := session.NewManager()
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
		SystemPrompt: buildSystemPrompt(cfg.Language),
		ChatOpts: llm.ChatOptions{
			MaxTokens: resolved.MaxTokens,
		},
		ProviderInfo:       providerInfo,
		Config:             cfg,
		ContextWindow:      resolved.Provider.ContextWindow,
		InitialSessionList: initialSessionList,
		MCPManager:         mcpMgr,
		MCPServers:         cfg.MCPServers,
	})
}

// runJSONResult is the structured JSON output for `tachi run --json`.
type runJSONResult struct {
	ExitReason     string     `json:"exit_reason"`
	IterationsUsed int        `json:"iterations_used"`
	Usage          *usageJSON `json:"usage"`
	Response       string     `json:"response"`
	Error          string     `json:"error,omitempty"`
}

type usageJSON struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

func usageToJSON(u *llm.Usage) *usageJSON {
	if u == nil {
		return nil
	}
	return &usageJSON{
		InputTokens:              u.InputTokens,
		OutputTokens:             u.OutputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens,
		CacheReadInputTokens:     u.CacheReadInputTokens,
	}
}

// exitCodeForReason maps agent exit reasons to Unix exit codes.
func exitCodeForReason(reason string) int {
	switch reason {
	case "stop":
		return 0
	case "budget_exhausted", "length_exhausted":
		return 2
	case "interrupted":
		return 130 // standard SIGINT exit code
	default: // "error", "cancelled", etc.
		return 1
	}
}

func runAgent(ctx context.Context, cmd *cli.Command) error {
	// Initialize debug logging.
	if err := debuglog.Init(config.LogsDir()); err != nil {
		fmt.Printf("Warning: failed to init debug log: %v\n", err)
	}
	defer debuglog.Close()

	// Apply optional timeout.
	if timeout := cmd.Duration("timeout"); timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Load MCP server config from JSON files.
	if err := cfg.LoadMCPServers(config.FindProjectRoot()); err != nil {
		return fmt.Errorf("failed to load MCP servers: %w", err)
	}

	provider, resolved, err := resolveProviderFromConfig(cfg)
	if err != nil {
		return err
	}

	// For single-shot run mode, 0 (unlimited) is capped to the default 50
	// to prevent runaway loops. Set max_iterations in config to set an explicit limit.
	maxIters := resolved.MaxIterations
	if maxIters <= 0 {
		maxIters = config.DefaultMaxIterations
	}

	aiAgent := agent.NewAIAgent(provider, resolved.Provider.Model, maxIters)
	aiAgent.SetSkipEditConfirm(cfg.TUI.SkipEditConfirm)
	aiAgent.SetSkipMemoryRecall(true) // "tachi run" is non-interactive — don't pollute prompt with memory recall
	aiAgent.SetContextWindow(resolved.Provider.ContextWindow)
	aiAgent.SetupTitleProvider(cfg)
	aiAgent.SetupCommitProvider(cfg)

	mcpMgr, err := aiAgent.Configure(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: agent configuration error: %v\n", err)
	}
	if mcpMgr != nil {
		defer mcpMgr.Close()
	}
	defer aiAgent.Close()

	// Wait briefly for MCP to connect so the first LLM call has tools available.
	mcpCtx, mcpCancel := context.WithTimeout(ctx, 5*time.Second)
	if err := aiAgent.WaitForMCP(mcpCtx); err != nil {
		// Timeout is not fatal — tools become available on subsequent iterations.
		fmt.Fprintf(os.Stderr, "MCP: background init still in progress (continuing)...\n")
	}
	mcpCancel()

	prompt := cmd.String("prompt")
	if prompt == "" {
		// Check if stdin is being piped (not a terminal).
		stat, err := os.Stdin.Stat()
		if err == nil && (stat.Mode()&os.ModeCharDevice) == 0 {
			pipeData, readErr := io.ReadAll(os.Stdin)
			if readErr == nil && len(pipeData) > 0 {
				prompt = strings.TrimSpace(string(pipeData))
			}
		}
	}
	if prompt == "" {
		prompt = "Write 'Hello, World!' to /tmp/test.txt and then read it back"
	}

	jsonOutput := cmd.Bool("json")

	// In JSON mode, progress info goes to stderr so stdout is pure JSON.
	if !jsonOutput {
		fmt.Printf("Provider: %s (%s)\n", resolved.Provider.Type, resolved.Provider.Model)
		fmt.Printf("User: %s\n\n", prompt)
	}

	var history []llm.Message

	if cmd.Bool("resume") {
		llmMsgs, _, latest, err := aiAgent.ResumeSession(resolved.Provider.Type, buildSystemPrompt(cfg.Language))
		if err != nil {
			return fmt.Errorf("resume failed: %w", err)
		}
		history = llmMsgs

		// Rebuild provider to match the session's original provider/model.
		if latest.Provider != resolved.Provider.Type || latest.Model != resolved.Provider.Model {
			sp, spErr := config.ResolveSessionProvider(cfg, latest.Provider, latest.Model)
			if spErr != nil {
				fmt.Fprintf(os.Stderr, "Warning: cannot restore session provider %q: %v\n", latest.Provider, spErr)
			} else {
				provider, provErr := llm.NewProvider(sp.Type, sp.APIKey, sp.BaseURL, sp.Model)
				if provErr != nil {
					fmt.Fprintf(os.Stderr, "Warning: cannot create session provider: %v\n", provErr)
				} else {
					aiAgent.SetProvider(provider, sp.Model)
					resolved.Provider = *sp
					restoreMsg := fmt.Sprintf("Provider (restored): %s (%s)\n", resolved.Provider.Type, resolved.Provider.Model)
					if jsonOutput {
						fmt.Fprintf(os.Stderr, "%s", restoreMsg)
					} else {
						fmt.Printf("%s", restoreMsg)
					}
				}
			}
		}
	}

	// Use streaming API to support history
	ch := aiAgent.RunConversationStream(ctx, history, prompt, buildSystemPrompt(cfg.Language), llm.ChatOptions{
		MaxTokens: resolved.MaxTokens,
	})

	var result *agent.RunResult
	for event := range ch {
		switch event.Type {
		case agent.AgentEventTurnComplete:
			result = event.Result
		case agent.AgentEventError:
			result = event.Result
		case agent.AgentEventToolConfirmation:
			aiAgent.ConfirmTool(true)
		}
	}
	if result == nil {
		result = &agent.RunResult{ExitReason: "error", Error: fmt.Errorf("no result received")}
	}

	if jsonOutput {
		// Emit structured JSON to stdout.
		jr := runJSONResult{
			ExitReason:     result.ExitReason,
			IterationsUsed: result.IterationsUsed,
			Usage:          usageToJSON(result.Usage),
			Response:       result.Response,
		}
		if result.Error != nil {
			jr.Error = result.Error.Error()
		}
		out, _ := json.Marshal(jr)
		fmt.Println(string(out))
	} else {
		fmt.Printf("Exit Reason: %s\n", result.ExitReason)
		fmt.Printf("Iterations Used: %d\n", result.IterationsUsed)
		fmt.Printf("\nResponse:\n%s\n", result.Response)
	}

	// Map exit reason to proper exit code.
	os.Exit(exitCodeForReason(result.ExitReason))
	return nil
}

// runChannels starts all channels declared in config.
//
// Channels are discovered via the registry (channel.Register). Each entry in
// cfg.Channel.ActiveChannels() is matched to a registered factory by name.
// For backward compatibility, the legacy cfg.Channel.Weixin.Enabled flag
// is converted by ActiveChannels() into a "weixin" entry if not already
// present in the new-style channels map.
//
// To add private channels, create a file like:
//
//	package main
//	import _ "private-repo/tachi-channel-mybots"
//
// and configure them in config.yaml:
//
//	channel:
//	  channels:
//	    mybots:
//	      enabled: true
//	      token: "xxx"
func runChannels(ctx context.Context, cmd *cli.Command) error {
	if err := debuglog.Init(config.LogsDir()); err != nil {
		fmt.Printf("Warning: failed to init debug log: %v\n", err)
	}
	defer debuglog.Close()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Load MCP server config from JSON files.
	if err := cfg.LoadMCPServers(config.FindProjectRoot()); err != nil {
		return fmt.Errorf("failed to load MCP servers: %w", err)
	}

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
			fmt.Printf("[channel] WARNING: %q enabled in config but no factory registered (import its package?)\n", name)
			continue
		}

		ch, err := factory(rawCfg)
		if err != nil {
			return fmt.Errorf("channel %q: create: %w", name, err)
		}

		mgr.Add(ch)
		instantiated++
		fmt.Printf("[channel] %s registered\n", name)
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

	// Block until context is cancelled.
	<-ctx.Done()
	fmt.Println("[channel] shutting down...")
	mgr.Close()
	return nil
}

// ── ACP Agent ────────────────────────────────────────────────────────────────

func runACPAgent(ctx context.Context) error {
	// Initialize debug logging (stdout is reserved for JSON-RPC, use file logging).
	if err := debuglog.Init(config.LogsDir()); err != nil {
		fmt.Fprintf(os.Stderr, "tachi: warning: failed to init debug log: %v\n", err)
	}
	defer debuglog.Close()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Load MCP server config from JSON files.
	if err := cfg.LoadMCPServers(config.FindProjectRoot()); err != nil {
		return fmt.Errorf("failed to load MCP servers: %w", err)
	}

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
	mgr, err := session.NewManager()
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
		date := s.CreatedAt.Format("2006-01-02 15:04")
		fmt.Printf("%-40s  %-20s  %s\n", s.ID, date, s.Title)
	}
	fmt.Printf("\n%d sessions total.\n", len(sessions))
	fmt.Println("Use: tachi transcript show --session <id>    (or --latest)")
	return nil
}

func transcriptShow(ctx context.Context, cmd *cli.Command) error {
	mgr, err := session.NewManager()
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

	// Build report data from session messages (transcript is replaced by session).
	data := render.BuildReportDataFromMessages(sess, msgs)
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
