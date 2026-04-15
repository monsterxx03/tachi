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

// OpenAIProvider implements Provider for OpenAI Chat Completions API
type OpenAIProvider struct {
	apiKey     string
	baseURL    string
	model      string
	httpClient *http.Client
}

// NewOpenAIProvider creates a new OpenAI provider
func NewOpenAIProvider(apiKey, baseURL, model string) *OpenAIProvider {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &OpenAIProvider{
		apiKey:     apiKey,
		baseURL:    baseURL,
		model:      model,
		httpClient: &http.Client{},
	}
}

// Name returns the provider name
func (p *OpenAIProvider) Name() string {
	return "openai"
}

type openaiMessage struct {
	Role         string `json:"role"`
	Content      string `json:"content"`
	Name         string `json:"name,omitempty"`
	ToolCalls    []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
}

type openaiRequest struct {
	Model       string          `json:"model"`
	Messages    []openaiMessage `json:"messages"`
	Tools       []any           `json:"tools,omitempty"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Temperature float64         `json:"temperature,omitempty"`
}

type openaiResponse struct {
	ID      string `json:"id"`
	Choices []struct {
		Message struct {
			Role         string `json:"role"`
			Content      string `json:"content"`
			ToolCalls    []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// CreateChat sends a chat request to OpenAI
func (p *OpenAIProvider) CreateChat(ctx context.Context, messages []Message, tools []Tool, maxTokens int) (*Response, error) {
	reqMessages := make([]openaiMessage, 0, len(messages))
	for _, msg := range messages {
		om := openaiMessage{
			Role:    msg.Role,
			Content: msg.Content,
			Name:    msg.Name,
		}
		if msg.ToolCallID != "" {
			om.ToolCallID = msg.ToolCallID
		}
		for _, tc := range msg.ToolCalls {
			om.ToolCalls = append(om.ToolCalls, struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			}{
				ID:   tc.ID,
				Type: tc.Type,
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				},
			})
		}
		reqMessages = append(reqMessages, om)
	}

	// Convert tools to OpenAI format
	var reqTools []any
	for _, tool := range tools {
		reqTools = append(reqTools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        tool.Name,
				"description": tool.Description,
				"parameters":  tool.Parameters,
			},
		})
	}

	reqBody := openaiRequest{
		Model:       p.model,
		Messages:    reqMessages,
		Tools:       reqTools,
		MaxTokens:   maxTokens,
		Temperature: 0.7,
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/chat/completions", strings.TrimSuffix(p.baseURL, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

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

	var openaiResp openaiResponse
	if err := json.Unmarshal(body, &openaiResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	if len(openaiResp.Choices) == 0 {
		return &Response{
			Content:      "",
			FinishReason: "stop",
		}, nil
	}

	choice := openaiResp.Choices[0]
	response := &Response{
		Content:      choice.Message.Content,
		FinishReason: choice.FinishReason,
	}

	for _, tc := range choice.Message.ToolCalls {
		response.ToolCalls = append(response.ToolCalls, ToolCall{
			ID:   tc.ID,
			Type: tc.Type,
			Function: ToolCallFunction{
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			},
		})
	}

	return response, nil
}
