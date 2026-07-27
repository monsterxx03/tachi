package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/urfave/cli/v3"

	acp "github.com/coder/acp-go-sdk"

	"github.com/monsterxx03/tachi/agent"
	acppkg "github.com/monsterxx03/tachi/agent/acp"
	cmds "github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/agent/transcript/render"
	channelmgr "github.com/monsterxx03/tachi/channel/manager"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/channel"
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

func buildSystemPrompt(language string, pprofCfg config.PprofConfig) string {
	return agent.BuildSystemPrompt(language, "", "", pprofCfg)
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
				Name:  "tools",
				Usage: "List available tools",
				Flags: append(commonFlags,
					&cli.BoolFlag{
						Name:  "mcp",
						Usage: "Include MCP tools (connects to MCP servers)",
					},
					&cli.BoolFlag{
						Name:  "json",
						Usage: "Output tool definitions as JSON",
					},
				),
				Action: runToolsCmd,
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

	boot, err := agent.Bootstrap(ctx)
	if err != nil {
		return err
	}
	cfg := boot.Config

	provider, resolved, err := resolveProviderFromConfig(cfg)
	if err != nil {
		return err
	}

	// TUI is interactive — no iteration budget cap (0 = unlimited).
	aiAgent := agent.NewAIAgent(provider, 0)
	aiAgent.SetLogger(logger.New("tui"))
	aiAgent.SetAutoApproveEdits(cfg.TUI.AutoApproveEdits)
	aiAgent.SetContextWindow(resolved.Provider.ContextWindow)
	aiAgent.SetupTitleProvider(cfg)
	aiAgent.SetupCommitProvider(cfg)
	aiAgent.SetupReviewProvider(cfg)

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
		SystemPrompt: buildSystemPrompt(cfg.Language, cfg.Debug.PPROF),
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

func runCommit(ctx context.Context, cmd *cli.Command) error {
	// Apply optional timeout.
	if timeout := cmd.Duration("timeout"); timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	boot, err := agent.Bootstrap(ctx)
	if err != nil {
		return err
	}
	cfg := boot.Config

	provider, resolved, err := resolveProviderFromConfig(cfg)
	if err != nil {
		return err
	}

	// Iteration budget for commit.
	maxIters := resolved.MaxIterations
	if maxIters <= 0 {
		maxIters = config.DefaultMaxIterations
	}

	aiAgent := agent.NewAIAgent(provider, maxIters)
	aiAgent.SetLogger(logger.New("run"))
	aiAgent.SetPermissionMode(agent.PermissionModeSkip) // non-interactive
	aiAgent.SetSkipMemoryRecall(true)
	aiAgent.SetContextWindow(resolved.Provider.ContextWindow)
	aiAgent.SetupTitleProvider(cfg)
	aiAgent.SetupCommitProvider(cfg)
	aiAgent.SetupReviewProvider(cfg)

	_, err = aiAgent.Configure(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: agent configuration error: %v\n", err)
	}
	defer aiAgent.Close()

	// Restrict to only the Bash tool (same as /commit in TUI).
	for _, name := range aiAgent.ToolNames() {
		if name != tools.ToolNameBash {
			aiAgent.UnregisterTool(name)
		}
	}

	commitProvider := aiAgent.CommitProvider()
	model := aiAgent.Model()

	// Build user prompt.
	userPrompt := cmds.CommitUserPrompt(model)

	// If -p/--prompt is also given, append as extra instructions.
	if extra := cmd.String("prompt"); extra != "" {
		userPrompt = userPrompt + "\n\n## Additional instructions\n\n" + extra
	}

	// If stdin is piped, append its content too.
	if pipeData := readStdinPipe(); pipeData != "" {
		userPrompt = userPrompt + "\n\n## Stdin input\n\n" + pipeData
	}

	// Disable thinking for commit (saves tokens/latency).
	thinkingDisabled := false
	opts := llm.ChatOptions{
		MaxTokens: resolved.MaxTokens,
		Thinking:  &thinkingDisabled,
	}

	outputFmt := parseOutputFormat(cmd)
	quiet := resolveQuiet(cmd)

	if !quiet {
		fmt.Fprintf(os.Stderr, "Provider: %s (%s)\n", resolved.Provider.Type, resolved.Provider.Model)
		fmt.Fprintf(os.Stderr, "Output format: %s\n\n", outputFmt)
	}

	ch := aiAgent.RunOneOffStream(ctx, commitProvider, buildSystemPrompt(cfg.Language, cfg.Debug.PPROF),
		userPrompt, opts, agent.OneOffMeta{Kind: "commit"})

	result := runOutputLoop(aiAgent, ch, outputFmt, quiet)
	if result == nil {
		result = &agent.RunResult{ExitReason: "error", Error: fmt.Errorf("no result received")}
	}

	if !quiet {
		fmt.Fprintf(os.Stderr, "\nExit: %s | Iterations: %d\n", result.ExitReason, result.IterationsUsed)
	}
	// Explicitly close the agent before os.Exit — deferred functions do not run
	// after os.Exit, so this ensures session_end is dispatched (e.g. Herdr
	// release_agent).
	aiAgent.Close()
	os.Exit(exitCodeForReason(result.ExitReason))
	return nil
}

// readStdinPipe reads data from stdin if it is being piped (not a terminal).
// Returns empty string if stdin is a terminal or no data is available.
func readStdinPipe() string {
	stat, err := os.Stdin.Stat()
	if err != nil || (stat.Mode()&os.ModeCharDevice) != 0 {
		return ""
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil || len(data) == 0 {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func runAgent(ctx context.Context, cmd *cli.Command) error {
	// Apply optional timeout.
	if timeout := cmd.Duration("timeout"); timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	boot, err := agent.Bootstrap(ctx)
	if err != nil {
		return err
	}
	cfg := boot.Config

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

	aiAgent := agent.NewAIAgent(provider, maxIters)
	aiAgent.SetLogger(logger.New("run"))
	aiAgent.SetPermissionMode(agent.PermissionModeSkip) // non-interactive
	aiAgent.SetSkipMemoryRecall(true)                   // "tachi run" is non-interactive — don't pollute prompt with memory recall
	aiAgent.SetContextWindow(resolved.Provider.ContextWindow)
	aiAgent.SetupTitleProvider(cfg)
	aiAgent.SetupCommitProvider(cfg)
	aiAgent.SetupReviewProvider(cfg)
	aiAgent.SetupRunProvider(cfg)

	// If a run provider is configured, switch to it for tachi -p mode.
	if rp := aiAgent.RunProvider(); rp != nil {
		aiAgent.SetProvider(rp)
		provider = rp
		// Update display info to reflect the run provider.
		resolved.Provider.Type = rp.Name()
		resolved.Provider.Model = rp.Model()
		// Re-resolve run provider config to get the correct context window.
		if rpCfg := cfg.FindProvider(cfg.RunProvider); rpCfg != nil {
			if rpResolved, err := config.ResolveProviderConfig(rpCfg); err == nil {
				aiAgent.SetContextWindow(rpResolved.ContextWindow)
				resolved.Provider.ContextWindow = rpResolved.ContextWindow
			}
		}
	}

	mcpMgr, err := aiAgent.Configure(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: agent configuration error: %v\n", err)
	}

	// Non-interactive mode: unregister AskUserQuestion so the LLM never
	// attempts to use it — there's no TUI form available in pipe mode.
	aiAgent.UnregisterTool(tools.ToolNameAskUser)

	// Wait briefly for MCP to connect so the first LLM call has tools available.
	mcpCtx, mcpCancel := context.WithTimeout(ctx, 5*time.Second)
	if err := aiAgent.WaitForMCP(mcpCtx); err != nil {
		fmt.Fprintf(os.Stderr, "MCP: background init still in progress (continuing)...\n")
	}
	mcpCancel()

	outputFmt := parseOutputFormat(cmd)
	quiet := resolveQuiet(cmd)

	prompt, err := resolvePrompt(cmd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		fmt.Fprintf(os.Stderr, "Usage: tachi -p \"<prompt>\" or pipe input via stdin\n")
		os.Exit(2)
		return nil
	}

	if !quiet {
		fmt.Fprintf(os.Stderr, "Provider: %s (%s)\n", resolved.Provider.Type, resolved.Provider.Model)
		fmt.Fprintf(os.Stderr, "Output format: %s\n\n", outputFmt)
	}

	var history []llm.Message

	applyToolRestrictions(aiAgent, cmd)

	thinkingDisabled := false
	ch := aiAgent.RunConversationStream(ctx, history, prompt, buildSystemPrompt(cfg.Language, cfg.Debug.PPROF), llm.ChatOptions{
		MaxTokens: resolved.MaxTokens,
		Thinking:  &thinkingDisabled,
	})

	result := runOutputLoop(aiAgent, ch, outputFmt, quiet)
	if result == nil {
		result = &agent.RunResult{ExitReason: "error", Error: fmt.Errorf("no result received")}
	}

	if !quiet {
		fmt.Fprintf(os.Stderr, "\nExit: %s | Iterations: %d\n", result.ExitReason, result.IterationsUsed)
	}
	// Explicit cleanup before os.Exit (defers won't run after os.Exit).
	if mcpMgr != nil {
		mcpMgr.Close()
	}
	aiAgent.Close()
	os.Exit(exitCodeForReason(result.ExitReason))
	return nil
}

// ── Output Format ──────────────────────────────────────────────────────────

// outputFormat represents the available output formats for `tachi run`.
type outputFormat int

const (
	outputText       outputFormat = iota // human-readable text (default)
	outputJSON                           // single JSON object
	outputJSONStream                     // NDJSON stream
)

func (f outputFormat) String() string {
	switch f {
	case outputJSON:
		return "json"
	case outputJSONStream:
		return "json-stream"
	default:
		return "text"
	}
}

// parseOutputFormat resolves the --output-format flag.
func parseOutputFormat(cmd *cli.Command) outputFormat {
	switch cmd.String("output-format") {
	case "json":
		return outputJSON
	case "json-stream":
		return outputJSONStream
	default:
		return outputText
	}
}

// resolveQuiet determines whether progress output should be suppressed.
// Automatically quiet when stdout is not a terminal (piped to another command).
func resolveQuiet(cmd *cli.Command) bool {
	if cmd.IsSet("quiet") {
		return cmd.Bool("quiet")
	}
	return !term.IsTerminal(int(os.Stdout.Fd()))
}

// resolvePrompt returns the user prompt from --prompt flag and/or stdin pipe.
// When both are provided, stdin content is appended after the --prompt.
// Returns an error if neither is provided.
func resolvePrompt(cmd *cli.Command) (string, error) {
	flagPrompt := cmd.String("prompt")
	var pipeData string

	// Check if stdin is being piped (not a terminal).
	stat, err := os.Stdin.Stat()
	if err == nil && (stat.Mode()&os.ModeCharDevice) == 0 {
		data, readErr := io.ReadAll(os.Stdin)
		if readErr == nil && len(data) > 0 {
			pipeData = strings.TrimSpace(string(data))
		}
	}

	if flagPrompt != "" && pipeData != "" {
		return flagPrompt + "\n" + pipeData, nil
	}
	if flagPrompt != "" {
		return flagPrompt, nil
	}
	if pipeData != "" {
		return pipeData, nil
	}
	return "", errors.New("no prompt provided")
}

// applyToolRestrictions parses --allowed-tools and --disallowed-tools flags
// and delegates to agent.RestrictTools.
func applyToolRestrictions(aiAgent *agent.AIAgent, cmd *cli.Command) {
	allowed := strings.TrimSpace(cmd.String("allowed-tools"))
	disallowed := strings.TrimSpace(cmd.String("disallowed-tools"))
	if allowed == "" && disallowed == "" {
		return
	}

	var allowedList, disallowedList []string
	if allowed != "" {
		for name := range strings.SplitSeq(allowed, ",") {
			if n := strings.TrimSpace(name); n != "" {
				allowedList = append(allowedList, n)
			}
		}
	}
	if disallowed != "" {
		for name := range strings.SplitSeq(disallowed, ",") {
			if n := strings.TrimSpace(name); n != "" {
				disallowedList = append(disallowedList, n)
			}
		}
	}
	aiAgent.RestrictTools(allowedList, disallowedList)
}

// runOutputLoop dispatches to the correct output handler based on format.
func runOutputLoop(aiAgent *agent.AIAgent, ch <-chan agent.AgentEvent, fmt outputFormat, quiet bool) *agent.RunResult {
	switch fmt {
	case outputJSON:
		return runOutputJSON(ch)
	case outputJSONStream:
		return runOutputJSONStream(aiAgent, ch)
	default:
		return runOutputText(aiAgent, ch, quiet)
	}
}

// runOutputText streams text delta events to stdout and progress to stderr.
func runOutputText(aiAgent *agent.AIAgent, ch <-chan agent.AgentEvent, quiet bool) *agent.RunResult {
	var result *agent.RunResult
	for event := range ch {
		switch event.Type {
		case agent.AgentEventTextDelta:
			fmt.Fprint(os.Stdout, event.TextDelta)
			os.Stdout.Sync()

		case agent.AgentEventThinkingDelta:
			if !quiet {
				fmt.Fprint(os.Stderr, event.ThinkingDelta)
			}

		case agent.AgentEventToolCallStart:
			if !quiet {
				fmt.Fprintf(os.Stderr, "\n🔧 %s(", event.ToolName)
			}

		case agent.AgentEventToolCallArgs:
			if !quiet {
				trunc := event.ToolArgs
				if len(trunc) > 60 {
					trunc = trunc[:60] + "..."
				}
				fmt.Fprintf(os.Stderr, "%s)\n", trunc)
			}

		case agent.AgentEventToolResult:
			if !quiet {
				icon := "✅"
				if event.ToolIsError {
					icon = "❌"
				}
				fmt.Fprintf(os.Stderr, " %s (%v)\n", icon, event.ToolDuration.Round(time.Millisecond))
			}

		case agent.AgentEventTurnComplete:
			result = event.Result

		case agent.AgentEventError:
			result = event.Result

		case agent.AgentEventToolConfirmation:
			aiAgent.ConfirmTool(agent.ConfirmAllowOnce)
		}
	}
	return result
}

// streamEvent is a single NDJSON event in json-stream output mode.
type streamEvent struct {
	Type       string     `json:"type"`
	Content    string     `json:"content,omitempty"`
	ToolName   string     `json:"tool_name,omitempty"`
	ToolArgs   string     `json:"tool_args,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolResult string     `json:"tool_result,omitempty"`
	DurationMS int64      `json:"duration_ms,omitempty"`
	IsError    bool       `json:"is_error,omitempty"`
	ExitReason string     `json:"exit_reason,omitempty"`
	Iterations int        `json:"iterations_used,omitempty"`
	Usage      *usageJSON `json:"usage,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// usageJSON is the serializable representation of token usage.
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

// runOutputJSON collects all events and outputs a single JSON object to stdout.
func runOutputJSON(ch <-chan agent.AgentEvent) *agent.RunResult {
	var result *agent.RunResult
	for event := range ch {
		switch event.Type {
		case agent.AgentEventTurnComplete:
			result = event.Result
		case agent.AgentEventError:
			result = event.Result
		case agent.AgentEventToolConfirmation:
		}
	}

	if result != nil {
		out := struct {
			ExitReason     string     `json:"exit_reason"`
			IterationsUsed int        `json:"iterations_used"`
			Response       string     `json:"response"`
			Usage          *usageJSON `json:"usage,omitempty"`
			Error          string     `json:"error,omitempty"`
		}{
			ExitReason:     result.ExitReason,
			IterationsUsed: result.IterationsUsed,
			Response:       result.Response,
			Usage:          usageToJSON(result.Usage),
		}
		if result.Error != nil {
			out.Error = result.Error.Error()
		}
		json.NewEncoder(os.Stdout).Encode(out)
	}
	return result
}

// runOutputJSONStream emits one JSON line per agent event to stdout.
func runOutputJSONStream(aiAgent *agent.AIAgent, ch <-chan agent.AgentEvent) *agent.RunResult {
	enc := json.NewEncoder(os.Stdout)
	var result *agent.RunResult

	for event := range ch {
		switch event.Type {
		case agent.AgentEventTextDelta:
			enc.Encode(streamEvent{Type: "text_delta", Content: event.TextDelta})

		case agent.AgentEventThinkingDelta:
			enc.Encode(streamEvent{Type: "thinking_delta", Content: event.ThinkingDelta})

		case agent.AgentEventToolCallStart:
			// Wait for args which come in the next event.
		case agent.AgentEventToolCallArgs:
			enc.Encode(streamEvent{
				Type:       "tool_call",
				ToolName:   event.ToolName,
				ToolArgs:   event.ToolArgs,
				ToolCallID: event.ToolID,
			})

		case agent.AgentEventToolResult:
			enc.Encode(streamEvent{
				Type:       "tool_result",
				ToolName:   event.ToolName,
				ToolResult: event.ToolResult,
				DurationMS: event.ToolDuration.Milliseconds(),
				IsError:    event.ToolIsError,
			})

		case agent.AgentEventTurnComplete:
			result = event.Result
			enc.Encode(streamEvent{
				Type:       "turn_complete",
				ExitReason: result.ExitReason,
				Iterations: result.IterationsUsed,
				Usage:      usageToJSON(result.Usage),
			})

		case agent.AgentEventError:
			result = event.Result
			errMsg := ""
			if result.Error != nil {
				errMsg = result.Error.Error()
			}
			enc.Encode(streamEvent{Type: "error", Error: errMsg})

		case agent.AgentEventToolConfirmation:
			aiAgent.ConfirmTool(agent.ConfirmAllowOnce)
		}
	}
	return result
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

// ── Tools listing ──────────────────────────────────────────────────────────────

func runToolsCmd(ctx context.Context, cmd *cli.Command) error {
	boot, err := agent.Bootstrap(ctx)
	if err != nil {
		return err
	}
	cfg := boot.Config

	// Create a minimal agent to register and list tools.
	// No LLM provider needed — we only use the tool registry.
	aiAgent := agent.NewAIAgent(nil, 0)
	defer aiAgent.Close()

	showMCP := cmd.Bool("mcp")

	mcpMgr, err := aiAgent.Configure(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: agent configuration error: %v\n", err)
	}

	if showMCP && mcpMgr != nil {
		waitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		if err := aiAgent.WaitForMCP(waitCtx); err != nil {
			fmt.Fprintf(os.Stderr, "MCP: some servers not ready yet (showing partial results)\n")
		}
		cancel()
	}
	if mcpMgr != nil {
		defer mcpMgr.Close()
	}

	schemas := aiAgent.ToolSchemas()
	outputJSON := cmd.Bool("json")

	// Extra tools that exist in the codebase but are only registered in
	// specific modes (e.g. channel mode for Cron / SendFile). We instantiate
	// them directly rather than hand-writing schemas.
	extraTools := []tools.Tool{
		tools.NewCronTool(nil, nil),
		&tools.SendFileTool{},
	}

	if outputJSON {
		// Collect all tool schemas (including deferred MCP tools and extra tools).
		var allSchemas []tools.Schema
		seen := make(map[string]bool)

		for _, s := range schemas {
			if !showMCP && strings.HasPrefix(s.Name, "mcp__") {
				continue
			}
			allSchemas = append(allSchemas, s)
			seen[s.Name] = true
		}

		for _, et := range extraTools {
			if !seen[et.Name()] {
				allSchemas = append(allSchemas, tools.ToSchema(et))
				seen[et.Name()] = true
			}
		}

		if showMCP {
			if pool := aiAgent.DeferredPool(); pool != nil {
				for _, dt := range pool.All() {
					if !seen[dt.Name] {
						allSchemas = append(allSchemas, dt.Schema)
						seen[dt.Name] = true
					}
				}
			}
		}

		sort.Slice(allSchemas, func(i, j int) bool {
			return allSchemas[i].Name < allSchemas[j].Name
		})

		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(allSchemas); err != nil {
			return fmt.Errorf("json encode: %w", err)
		}
		return nil
	}

	if showMCP {
		fmt.Println("Tools:")
	} else {
		fmt.Println("Built-in Tools:")
	}
	fmt.Println()

	// Collect all displayed tools as (name, description) pairs.
	type toolEntry struct {
		name string
		desc string
	}
	var entries []toolEntry

	// Built-in tools from registry (includes auto-loaded MCP tools).
	for _, s := range schemas {
		if !showMCP && strings.HasPrefix(s.Name, "mcp__") {
			continue
		}
		entries = append(entries, toolEntry{s.Name, firstLine(s.Description)})
	}

	// Extra tools not registered in the current mode.
	seen := make(map[string]bool)
	for _, e := range entries {
		seen[e.name] = true
	}
	for _, et := range extraTools {
		if !seen[et.Name()] {
			entries = append(entries, toolEntry{et.Name(), firstLine(et.Description())})
			seen[et.Name()] = true
		}
	}

	// Deferred MCP tools from pool — only shown with --mcp.
	if showMCP {
		if pool := aiAgent.DeferredPool(); pool != nil {
			for _, dt := range pool.All() {
				if !seen[dt.Name] {
					entries = append(entries, toolEntry{dt.Name, firstLine(dt.Description)})
					seen[dt.Name] = true
				}
			}
		}
	}

	for _, e := range entries {
		fmt.Printf("  %-30s  %s\n", e.name, e.desc)
	}

	return nil
}

// firstLine returns the first line of a multi-line string.
func firstLine(s string) string {
	if idx := strings.Index(s, "\n"); idx > 0 {
		return s[:idx]
	}
	return s
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
		date := s.CreatedAt.Format("2006-01-02 15:04")
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
