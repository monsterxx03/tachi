package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/llm"
	"github.com/urfave/cli/v3"
)

const defaultSystemPrompt = `You are a helpful AI assistant. You have access to tools:
- Read: Read the contents of a file
- Write: Write content to a file

Use tools when needed to fulfill user requests.`

func main() {
	app := &cli.Command{
		Name:  "tachi",
		Usage: "AI Agent CLI",
		Commands: []*cli.Command{
			{
				Name:  "test-openai",
				Usage: "Test OpenAI provider with tool calling",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:  "model",
						Usage: "Model to use",
					},
					&cli.StringFlag{
						Name:  "base-url",
						Usage: "Base URL for OpenAI API",
					},
					&cli.StringFlag{
						Name:    "prompt",
						Aliases: []string{"p"},
						Usage:   "User prompt to send",
					},
				},
				Action: runTestOpenAI,
			},
			{
				Name:  "test-anthropic",
				Usage: "Test Anthropic/MiniMax provider with tool calling",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:  "model",
						Usage: "Model to use",
					},
					&cli.StringFlag{
						Name:  "base-url",
						Usage: "Base URL for Anthropic API",
					},
					&cli.StringFlag{
						Name:    "prompt",
						Aliases: []string{"p"},
						Usage:   "User prompt to send",
					},
				},
				Action: runTestAnthropic,
			},
		},
	}

	if err := app.Run(context.Background(), os.Args); err != nil {
		log.Fatal(err)
	}
}

func runTestOpenAI(ctx context.Context, c *cli.Command) error {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("OPENAI_API_KEY environment variable is required")
	}

	model := c.String("model")
	if model == "" {
		model = "MiniMax-M2.7"
	}
	baseURL := c.String("base-url")
	if baseURL == "" {
		baseURL = "https://api.minimaxi.com/v1"
	}
	provider := llm.NewOpenAIProvider(apiKey, baseURL, model)
	aiAgent := agent.NewAIAgent(provider, model, 10)
	aiAgent.RegisterTools()

	fmt.Println("=== OpenAI Test ===")
	fmt.Printf("Model: %s\n", model)
	fmt.Printf("Base URL: %s\n\n", baseURL)

	return runTest(ctx, aiAgent, defaultSystemPrompt, c.String("prompt"))
}

func runTestAnthropic(ctx context.Context, c *cli.Command) error {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("ANTHROPIC_API_KEY environment variable is required")
	}

	model := c.String("model")
	if model == "" {
		model = "MiniMax-M2.7"
	}
	baseURL := c.String("base-url")
	if baseURL == "" {
		baseURL = "https://api.minimaxi.com/anthropic"
	}
	provider := llm.NewAnthropicProvider(apiKey, baseURL, model)
	aiAgent := agent.NewAIAgent(provider, model, 10)
	aiAgent.RegisterTools()

	fmt.Println("=== Anthropic/MiniMax Test ===")
	fmt.Printf("Model: %s\n", model)
	fmt.Printf("Base URL: %s\n\n", baseURL)

	return runTest(ctx, aiAgent, defaultSystemPrompt, c.String("prompt"))
}

func runTest(ctx context.Context, aiAgent *agent.AIAgent, systemPrompt string, userPrompt string) error {
	if userPrompt == "" {
		userPrompt = "Write 'Hello, World!' to /tmp/test.txt and then read it back"
	}
	fmt.Printf("User: %s\n\n", userPrompt)

	result := aiAgent.RunConversation(ctx, userPrompt, systemPrompt, llm.ChatOptions{MaxTokens: 4096})

	fmt.Printf("Exit Reason: %s\n", result.ExitReason)
	fmt.Printf("Iterations Used: %d\n", result.IterationsUsed)
	fmt.Println()
	fmt.Printf("Response:\n%s\n", result.Response)

	if result.Error != nil {
		return fmt.Errorf("error: %v", result.Error)
	}
	return nil
}
