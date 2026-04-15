package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/llm"
)

func main() {
	// Get API key from environment
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		log.Fatal("OPENAI_API_KEY environment variable is required")
	}

	// Create LLM provider - use OpenAI
	provider := llm.NewOpenAIProvider(apiKey, "", "gpt-4o")

	// Create agent
	aiAgent := agent.NewAIAgent(provider, "gpt-4o", 10)
	aiAgent.RegisterTools()

	// Define system prompt
	systemPrompt := `You are a helpful AI assistant. You have access to tools:
- Read: Read the contents of a file
- Write: Write content to a file

Use tools when needed to fulfill user requests.`

	// Run conversation
	ctx := context.Background()

	fmt.Println("=== Go Agent Test ===")
	fmt.Println()

	// Test: Ask to read a file (which we'll create first)
	fmt.Println("User: Write 'Hello, World!' to /tmp/test.txt and then read it back")
	fmt.Println()

	result := aiAgent.RunConversation(ctx, "Write 'Hello, World!' to /tmp/test.txt and then read it back", systemPrompt, 4096)

	fmt.Printf("Exit Reason: %s\n", result.ExitReason)
	fmt.Printf("Iterations Used: %d\n", result.IterationsUsed)
	fmt.Println()
	fmt.Printf("Response:\n%s\n", result.Response)

	if result.Error != nil {
		fmt.Printf("Error: %v\n", result.Error)
	}
}
