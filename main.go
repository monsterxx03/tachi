package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/debuglog"
	"github.com/monsterxx03/tachi/tools"
	"github.com/monsterxx03/tachi/tui"
	"github.com/urfave/cli/v3"
)

const defaultSystemPrompt = `You are a helpful AI assistant. You have access to tools:
- Read: Read the contents of a file
- Write: Write content to a file

Use tools when needed to fulfill user requests.`

var commonFlags = []cli.Flag{
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
	registerTools(aiAgent, cfg)

	providerInfo := fmt.Sprintf("%s (%s)", resolved.Provider.Type, resolved.Provider.Model)

	return tui.Run(tui.ModelConfig{
		Agent:        aiAgent,
		SystemPrompt: defaultSystemPrompt,
		ChatOpts: llm.ChatOptions{
			MaxTokens: resolved.MaxTokens,
		},
		ProviderInfo: providerInfo,
		Config:       cfg,
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
	registerTools(aiAgent, cfg)

	prompt := cmd.String("prompt")
	if prompt == "" {
		prompt = "Write 'Hello, World!' to /tmp/test.txt and then read it back"
	}
	fmt.Printf("Provider: %s (%s)\n", resolved.Provider.Type, resolved.Provider.Model)
	fmt.Printf("User: %s\n\n", prompt)

	result := aiAgent.RunConversation(ctx, prompt, defaultSystemPrompt, llm.ChatOptions{
		MaxTokens: resolved.MaxTokens,
	})

	fmt.Printf("Exit Reason: %s\n", result.ExitReason)
	fmt.Printf("Iterations Used: %d\n", result.IterationsUsed)
	fmt.Printf("\nResponse:\n%s\n", result.Response)

	if result.Error != nil {
		return fmt.Errorf("error: %v", result.Error)
	}
	return nil
}
