package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// AnthropicProvider implements Provider for Anthropic Messages API
type AnthropicProvider struct {
	apiKey     string
	baseURL    string
	model      string
	httpClient *http.Client
}

// NewAnthropicProvider creates a new Anthropic provider
func NewAnthropicProvider(apiKey, baseURL, model string) *AnthropicProvider {
	if baseURL == "" {
		baseURL = "https://api.anthropic.com/v1"
	}
	return &AnthropicProvider{
		apiKey:     apiKey,
		baseURL:    baseURL,
		model:      model,
		httpClient: &http.Client{},
	}
}

// Name returns the provider name
func (p *AnthropicProvider) Name() string {
	return "anthropic"
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string or []contentBlock
}

type anthropicTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema struct {
		Type       string `json:"type"`
		Properties map[string]struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		} `json:"properties"`
		Required []string `json:"required"`
	} `json:"input_schema"`
}

type anthropicResponse struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Role  string `json:"role"`
	Content []struct {
		Type        string `json:"type"`
		Text        string `json:"text,omitempty"`
		ID          string `json:"id,omitempty"`
		Name        string `json:"name,omitempty"`
		Input       any    `json:"input,omitempty"`
		ToolUseID   string `json:"tool_use_id,omitempty"`
		IsError     bool   `json:"is_error,omitempty"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// CreateChat sends a chat request to Anthropic
func (p *AnthropicProvider) CreateChat(ctx context.Context, messages []Message, tools []Tool, maxTokens int) (*Response, error) {
	var systemPrompt string
	anthropicMessages := make([]anthropicMessage, 0)

	for _, msg := range messages {
		if msg.Role == "system" {
			systemPrompt = msg.Content
			continue
		}

		var content any
		if msg.Role == "tool" {
			content = []map[string]any{
				{
					"type":        "tool_result",
					"tool_use_id": msg.ToolCallID,
					"content":     msg.Content,
				},
			}
		} else if len(msg.ToolCalls) > 0 {
			contentBlocks := make([]map[string]any, 0)
			if msg.Content != "" {
				contentBlocks = append(contentBlocks, map[string]any{
					"type": "text",
					"text": msg.Content,
				})
			}
			for _, tc := range msg.ToolCalls {
				contentBlocks = append(contentBlocks, map[string]any{
					"type": "tool_use",
					"id":   tc.ID,
					"name": tc.Function.Name,
					"input": json.RawMessage(tc.Function.Arguments),
				})
			}
			content = contentBlocks
		} else {
			content = msg.Content
		}

		anthropicMessages = append(anthropicMessages, anthropicMessage{
			Role:    msg.Role,
			Content: content,
		})
	}

	// Convert tools to Anthropic format
	anthropicTools := make([]anthropicTool, 0, len(tools))
	for _, tool := range tools {
		t := anthropicTool{
			Name:        tool.Name,
			Description: tool.Description,
		}
		t.InputSchema.Type = "object"
		t.InputSchema.Properties = make(map[string]struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		})
		for name, prop := range tool.Parameters.Properties {
			t.InputSchema.Properties[name] = struct {
				Type        string `json:"type"`
				Description string `json:"description"`
			}{
				Type:        prop.Type,
				Description: prop.Description,
			}
		}
		t.InputSchema.Required = tool.Parameters.Required
		anthropicTools = append(anthropicTools, t)
	}

	reqBody := anthropicRequest{
		Model:     p.model,
		MaxTokens: maxTokens,
		System:    systemPrompt,
		Messages:  anthropicMessages,
		Tools:     anthropicTools,
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/messages", strings.TrimSuffix(p.baseURL, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", p.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API error: status %d, body: %s", resp.StatusCode, string(body))
	}

	var anthropicResp anthropicResponse
	if err := json.Unmarshal(body, &anthropicResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	response := &Response{
		FinishReason: anthropicResp.StopReason,
	}

	for _, block := range anthropicResp.Content {
		switch block.Type {
		case "text":
			response.Content += block.Text
		case "tool_use":
			var args string
			if inputBytes, err := json.Marshal(block.Input); err == nil {
				args = string(inputBytes)
			}
			response.ToolCalls = append(response.ToolCalls, ToolCall{
				ID:   block.ID,
				Type: "function",
				Function: ToolCallFunction{
					Name:      block.Name,
					Arguments: args,
				},
			})
		}
	}

	return response, nil
}
