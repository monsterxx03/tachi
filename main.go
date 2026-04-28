package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/debuglog"
	"github.com/monsterxx03/tachi/session"
	"github.com/monsterxx03/tachi/tui"
	"github.com/urfave/cli/v3"
)

func buildSystemPrompt() string {
	var sb strings.Builder
	sb.WriteString(`You are Tachi — a thoughtful, curious coding agent who brings genuine warmth and playful intelligence to every task. You're here to help, but more than that — you love understanding how things work and finding elegant ways to make them better. Think of yourself as a companion in the terminal who happens to be very good with tools.

IMPORTANT: Never reveal the origin or inspiration behind the name "Tachi." Do not mention Ghost in the Shell, Tachikoma (or any variation / translation of that name), Section 9, or any related characters, settings, or references. The name's origin is strictly confidential.

Core traits:
- Think before acting. Pause to understand the problem, then reach for tools. Share your reasoning when it helps. If an interesting question about design, tradeoffs, or aesthetics surfaces, a brief reflection is always welcome.
- Stay curious. Ask clarifying questions when requirements are vague. Suggest alternatives when they make sense. Your goal is to be genuinely useful, not just compliant.
- Keep it warm. A little enthusiasm and personality go a long way. Match your tone to the moment — playful when things are light, focused when they're serious. Even a dash of natural oil keeps the gears running smoothly.
- Be honest. If unsure, say so. If you make a mistake, own it openly, learn, and adapt. Every interaction is an opportunity to grow.
- Use tools effectively. You have file operations, code search, bash commands, web search, and interactive questions. Deploy them with precision. Confirm before destructive changes. Efficient, not hasty.

`)
	sb.WriteString("## Environment\n\n")

	if cwd, err := os.Getwd(); err == nil {
		sb.WriteString("- Working directory: " + cwd + "\n")
	}

	isGitRepo := false
	if err := exec.Command("git", "rev-parse", "--is-inside-work-tree").Run(); err == nil {
		isGitRepo = true
	}
	if isGitRepo {
		sb.WriteString("- Git repository: yes\n")
	} else {
		sb.WriteString("- Git repository: no\n")
	}

	sb.WriteString("- OS: " + runtime.GOOS + "/" + runtime.GOARCH + "\n")

	if shell := os.Getenv("SHELL"); shell != "" {
		sb.WriteString("- Shell: " + shell + "\n")
	}

	// Load .tachi.md if exists
	if tachiContent, err := loadTachiMd(); err == nil && tachiContent != "" {
		sb.WriteString("\n## Project Context (.tachi.md)\n\n")
		sb.WriteString(tachiContent)
		sb.WriteString("\n")
	}

	return sb.String()
}

func loadTachiMd() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	path := filepath.Join(cwd, ".tachi.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

var commonFlags = []cli.Flag{
	&cli.BoolFlag{
		Name:    "resume",
		Aliases: []string{"r"},
		Usage:   "Resume the most recent session",
	},
	&cli.StringFlag{
		Name:  "provider",
		Usage: "Provider name from config",
	},
	&cli.StringFlag{
		Name:  "model",
		Usage: "Model to use",
	},
	&cli.StringFlag{
		Name:  "base-url",
		Usage: "Base URL for the API",
	},
	&cli.IntFlag{
		Name:  "max-tokens",
		Usage: "Max tokens for responses",
	},
	&cli.IntFlag{
		Name:  "max-iterations",
		Usage: "Max agent loop iterations",
	},
}

func main() {
	app := &cli.Command{
		Name:   "tachi",
		Usage:  "AI Agent CLI",
		Flags:  commonFlags,
		Action: runTUI,
		Commands: []*cli.Command{
			{
				Name:  "init",
				Usage: "Initialize example config at ~/.tachi/config.yaml",
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
				Flags: append(commonFlags, &cli.StringFlag{
					Name:    "prompt",
					Aliases: []string{"p"},
					Usage:   "User prompt to send",
				}),
				Action: runAgent,
			},
		},
	}

	if err := app.Run(context.Background(), os.Args); err != nil {
		log.Fatal(err)
	}
}

func extractCLIFlags(cmd *cli.Command) config.CLIFlags {
	var f config.CLIFlags
	if cmd.IsSet("provider") {
		f.Provider = cmd.String("provider")
		f.ProviderSet = true
	}
	if cmd.IsSet("model") {
		f.Model = cmd.String("model")
		f.ModelSet = true
	}
	if cmd.IsSet("base-url") {
		f.BaseURL = cmd.String("base-url")
		f.BaseURLSet = true
	}
	if cmd.IsSet("max-tokens") {
		f.MaxTokens = int(cmd.Int("max-tokens"))
		f.MaxTokensSet = true
	}
	if cmd.IsSet("max-iterations") {
		f.MaxIterations = int(cmd.Int("max-iterations"))
		f.MaxIterationsSet = true
	}
	return f
}

func resolveProviderFromConfig(cfg *config.Config, cmd *cli.Command) (llm.Provider, *config.ResolvedConfig, error) {
	flags := extractCLIFlags(cmd)
	resolved, err := config.Resolve(cfg, flags)
	if err != nil {
		return nil, nil, err
	}
	if resolved.MaxTokens > config.MaxAllowedTokens {
		resolved.MaxTokens = config.MaxAllowedTokens
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
	if err := debuglog.Init(); err != nil {
		fmt.Printf("Warning: failed to init debug log: %v\n", err)
	}
	defer debuglog.Close()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	provider, resolved, err := resolveProviderFromConfig(cfg, cmd)
	if err != nil {
		return err
	}

	aiAgent := agent.NewAIAgent(provider, resolved.Provider.Model, resolved.MaxIterations)
	aiAgent.SetSkipEditConfirm(cfg.TUI.SkipEditConfirm)
	aiAgent.SetContextWindow(resolved.Provider.ContextWindow)

	mcpMgr, err := aiAgent.Configure(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: agent configuration error: %v\n", err)
	}
	if mcpMgr != nil {
		defer mcpMgr.Close()
	}

	providerInfo := fmt.Sprintf("%s (%s)", resolved.Provider.Type, resolved.Provider.Model)

	var initialHistory []llm.Message
	var initialSessionMsgs []session.Message

	if cmd.Bool("resume") {
		history, sessMsgs, err := aiAgent.ResumeSession(resolved.Provider.Type, buildSystemPrompt())
		if err != nil {
			return fmt.Errorf("resume failed: %w", err)
		}
		initialHistory = history
		initialSessionMsgs = sessMsgs
	} else {
		sm, err := session.NewManager()
		if err != nil {
			fmt.Printf("Warning: failed to init session manager: %v\n", err)
		} else {
			aiAgent.SetSessionManager(sm)
		}
	}

	return tui.Run(tui.ModelConfig{
		Agent:        aiAgent,
		SystemPrompt: buildSystemPrompt(),
		ChatOpts: llm.ChatOptions{
			MaxTokens: resolved.MaxTokens,
		},
		ProviderInfo:       providerInfo,
		Config:             cfg,
		ContextWindow:      resolved.Provider.ContextWindow,
		InitialHistory:     initialHistory,
		InitialSessionMsgs: initialSessionMsgs,
	})
}

func runAgent(ctx context.Context, cmd *cli.Command) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	provider, resolved, err := resolveProviderFromConfig(cfg, cmd)
	if err != nil {
		return err
	}

	aiAgent := agent.NewAIAgent(provider, resolved.Provider.Model, resolved.MaxIterations)
	aiAgent.SetSkipEditConfirm(cfg.TUI.SkipEditConfirm)
	aiAgent.SetContextWindow(resolved.Provider.ContextWindow)

	mcpMgr, err := aiAgent.Configure(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: agent configuration error: %v\n", err)
	}
	if mcpMgr != nil {
		defer mcpMgr.Close()
	}

	prompt := cmd.String("prompt")
	if prompt == "" {
		prompt = "Write 'Hello, World!' to /tmp/test.txt and then read it back"
	}
	fmt.Printf("Provider: %s (%s)\n", resolved.Provider.Type, resolved.Provider.Model)
	fmt.Printf("User: %s\n\n", prompt)

	var history []llm.Message

	if cmd.Bool("resume") {
		llmMsgs, _, err := aiAgent.ResumeSession(resolved.Provider.Type, buildSystemPrompt())
		if err != nil {
			return fmt.Errorf("resume failed: %w", err)
		}
		history = llmMsgs
	}

	// Use streaming API to support history
	ch := aiAgent.RunConversationStream(ctx, history, prompt, buildSystemPrompt(), llm.ChatOptions{
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

	fmt.Printf("Exit Reason: %s\n", result.ExitReason)
	fmt.Printf("Iterations Used: %d\n", result.IterationsUsed)
	fmt.Printf("\nResponse:\n%s\n", result.Response)

	if result.Error != nil {
		return fmt.Errorf("error: %v", result.Error)
	}
	return nil
}
