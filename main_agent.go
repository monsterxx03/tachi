package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/urfave/cli/v3"
	"golang.org/x/term"

	"github.com/monsterxx03/tachi/agent"
	cmds "github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

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

	// Iteration budget for commit.
	maxIters := cfg.GetMaxIterations()
	if maxIters <= 0 {
		maxIters = config.DefaultMaxIterations
	}

	// Resolved (main provider + resolved config) is built inside the
	// constructor from FullConfig's default provider.
	aiAgent, _, err := agent.NewAIAgentWithConfig(ctx, agent.AgentConfig{
		MaxIterations:    maxIters,
		Logger:           logger.New("run"),
		PermissionMode:   agent.PermissionModeSkip,
		SkipMemoryRecall: true,
		FullConfig:       cfg,
		SystemConfig:     agent.SystemConfigFromConfig(cfg),
	})
	if err != nil {
		return err
	}
	defer aiAgent.Close()

	// Restrict to only the Bash tool (same as /commit in TUI).
	for _, name := range aiAgent.ToolNames() {
		if name != tools.ToolNameBash {
			aiAgent.UnregisterTool(name)
		}
	}

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

	outputFmt := parseOutputFormat(cmd)
	quiet := resolveQuiet(cmd)

	if !quiet {
		fmt.Fprintf(os.Stderr, "Provider: %s (%s)\n", aiAgent.Provider().Name(), aiAgent.Model())
		fmt.Fprintf(os.Stderr, "Output format: %s\n\n", outputFmt)
	}

	ch := aiAgent.RunCommitOneOff(ctx, buildSystemPrompt(cfg.Language), "", cfg.MaxTokens, userPrompt)

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

	// For single-shot run mode, 0 (unlimited) is capped to the default 50
	// to prevent runaway loops. Set max_iterations in config to set an explicit limit.
	maxIters := cfg.GetMaxIterations()
	if maxIters <= 0 {
		maxIters = config.DefaultMaxIterations
	}

	// Resolved (main provider + resolved config) is built inside the
	// constructor from FullConfig's default provider.
	aiAgent, mcpMgr, err := agent.NewAIAgentWithConfig(ctx, agent.AgentConfig{
		MaxIterations:          maxIters,
		Logger:                 logger.New("run"),
		PermissionMode:         agent.PermissionModeSkip,
		SkipMemoryRecall:       true,
		DisableSystemReminders: true, // non-interactive: no date/git/project/skills reminders
		FullConfig:             cfg,
		SystemConfig:           agent.SystemConfigFromConfig(cfg),
	})
	if err != nil {
		return err
	}

	// Display info: default provider, or the run provider when configured —
	// SetResolvedProvider resolves the name via the agent's FullConfig and
	// swaps the full ResolvedProvider (instance + context window + thinking
	// defaults) in one step.
	providerType, providerModel := aiAgent.Provider().Name(), aiAgent.Model()
	if cfg.RunProvider != "" {
		if rpResolved, err := aiAgent.SetResolvedProvider(cfg.RunProvider); err == nil {
			providerType, providerModel = rpResolved.Type, rpResolved.Model
		}
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
		fmt.Fprintf(os.Stderr, "Provider: %s (%s)\n", providerType, providerModel)
		fmt.Fprintf(os.Stderr, "Output format: %s\n\n", outputFmt)
	}

	var history []llm.Message

	applyToolRestrictions(aiAgent, cmd)

	thinkingDisabled := false
	ch := aiAgent.RunConversationStream(ctx, history, prompt, buildSystemPrompt(cfg.Language), llm.ChatOptions{
		MaxTokens: cfg.MaxTokens,
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
		allowedList = strutil.SplitBy(allowed, ",")
	}
	if disallowed != "" {
		disallowedList = strutil.SplitBy(disallowed, ",")
	}
	aiAgent.RestrictTools(allowedList, disallowedList)
}

// runOutputLoop dispatches to the correct output handler based on format.
