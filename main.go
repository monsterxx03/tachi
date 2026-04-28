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
	"github.com/monsterxx03/tachi/tools"
	"github.com/monsterxx03/tachi/tui"
	"github.com/urfave/cli/v3"
)

func buildSystemPrompt() string {
	var sb strings.Builder
	sb.WriteString("You are a helpful AI assistant.\nUse tools when needed to fulfill user requests.\n\n")
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

func registerTools(aiAgent *agent.AIAgent, cfg *config.Config) {
	aiAgent.RegisterTools()
	ws := tools.WebSearchTool{
		ProviderType: cfg.WebSearch.Type,
		APIKey:       cfg.WebSearch.Key,
		Timeout:      cfg.WebSearch.Timeout,
		MaxResults:   cfg.WebSearch.MaxResults,
	}
	if _, key := ws.ResolveProvider(); key != "" {
		aiAgent.RegisterTool(ws)
	}
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

func resolveProvider(cmd *cli.Command) (llm.Provider, *config.ResolvedConfig, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load config: %w", err)
	}
	return resolveProviderFromConfig(cfg, cmd)
}

// loadSessionHistory loads the most recent session and returns its messages
// converted to LLM format. The session is set as current on the manager.
func loadSessionHistory(sm *session.Manager, providerType string) ([]llm.Message, []session.Message, error) {
	sessions, err := sm.List()
	if err != nil {
		return nil, nil, fmt.Errorf("list sessions: %w", err)
	}
	if len(sessions) == 0 {
		return nil, nil, fmt.Errorf("no sessions to resume")
	}

	latest := sessions[0] // Sorted by CreatedAt descending
	if _, err := sm.Load(latest.ID); err != nil {
		return nil, nil, fmt.Errorf("load session %s: %w", latest.ID, err)
	}

	sessionMsgs, err := sm.LoadMessages()
	if err != nil {
		return nil, nil, fmt.Errorf("load messages: %w", err)
	}

	llmMsgs, err := agent.ConvertSessionToLLMMessages(sessionMsgs, providerType)
	if err != nil {
		return nil, nil, fmt.Errorf("convert session messages: %w", err)
	}

	return llmMsgs, sessionMsgs, nil
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
	registerTools(aiAgent, cfg)

	providerInfo := fmt.Sprintf("%s (%s)", resolved.Provider.Type, resolved.Provider.Model)

	// Initialize session manager and attach to agent
	sessionManager, err := session.NewManager()
	if err != nil {
		fmt.Printf("Warning: failed to init session manager: %v\n", err)
	} else {
		aiAgent.SetSessionManager(sessionManager)
	}

	var initialHistory []llm.Message
	var initialSessionMsgs []session.Message

	if cmd.Bool("resume") {
		if sessionManager == nil {
			return fmt.Errorf("cannot resume: session manager unavailable")
		}
		history, sessMsgs, err := loadSessionHistory(sessionManager, resolved.Provider.Type)
		if err != nil {
			return fmt.Errorf("resume failed: %w", err)
		}
		initialHistory = history
		initialSessionMsgs = sessMsgs
		// Prepend current system prompt to history
		sysPrompt := buildSystemPrompt()
		if sysPrompt != "" {
			initialHistory = append([]llm.Message{{Role: "system", Content: sysPrompt}}, initialHistory...)
		}
	}

	return tui.Run(tui.ModelConfig{
		Agent:        aiAgent,
		SystemPrompt: buildSystemPrompt(),
		ChatOpts: llm.ChatOptions{
			MaxTokens: resolved.MaxTokens,
		},
		ProviderInfo:        providerInfo,
		Config:              cfg,
		InitialHistory:      initialHistory,
		InitialSessionMsgs:  initialSessionMsgs,
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
	registerTools(aiAgent, cfg)

	prompt := cmd.String("prompt")
	if prompt == "" {
		prompt = "Write 'Hello, World!' to /tmp/test.txt and then read it back"
	}
	fmt.Printf("Provider: %s (%s)\n", resolved.Provider.Type, resolved.Provider.Model)
	fmt.Printf("User: %s\n\n", prompt)

	var history []llm.Message

	if cmd.Bool("resume") {
		sessionManager, err := session.NewManager()
		if err != nil {
			return fmt.Errorf("session manager: %w", err)
		}
		llmMsgs, _, err := loadSessionHistory(sessionManager, resolved.Provider.Type)
		if err != nil {
			return fmt.Errorf("resume failed: %w", err)
		}
		history = llmMsgs
		sysPrompt := buildSystemPrompt()
		if sysPrompt != "" {
			history = append([]llm.Message{{Role: "system", Content: sysPrompt}}, history...)
		}
		aiAgent.SetSessionManager(sessionManager)
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
