package tools

import (
	"context"
	"encoding/json"
	"fmt"

	acp "github.com/coder/acp-go-sdk"

	"github.com/monsterxx03/tachi/agent/acpctx"
)

// QuestionOption represents a single option in a question
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
	Preview     string `json:"preview,omitempty"`
}

// Question represents a single question to ask the user
type Question struct {
	Question    string           `json:"question"`
	Header      string           `json:"header"`
	Options     []QuestionOption `json:"options"`
	MultiSelect bool             `json:"multi_select"`
}

// AskUserResult holds the user's answers to the questions
type AskUserResult struct {
	Answers     map[string]string
	Annotations map[string]string
}

// AskUserTool asks the user multiple choice questions
type AskUserTool struct{}

func (t AskUserTool) Name() string { return ToolNameAskUser }

func (t AskUserTool) Description() string {
	return "Ask the user questions during execution. " +
		"Use to gather preferences, clarify ambiguous instructions, " +
		"or get decisions on implementation choices. " +
		"When you have specific options, provide them in the options array (2-4 items). " +
		"For open-ended input, omit the options field for a free-text box."
}

func (t AskUserTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"questions": {
			Type:        "array",
			Description: "Questions to ask the user (1-4 questions). When options are provided, each option becomes a selectable choice. When options is empty or omitted, the user gets a free-text input box instead.",
			Items: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"question":    map[string]any{"type": "string", "description": "The question to ask the user"},
					"header":      map[string]any{"type": "string", "description": "Short label displayed as a chip/tag"},
					"multiSelect": map[string]any{"type": "boolean", "description": "Whether to allow multiple selections"},
					"options": map[string]any{
						"type":        "array",
						"description": "Pre-defined choices (omit this field entirely for a free-text input box instead)",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"label":       map[string]any{"type": "string", "description": "Display text for this option"},
								"description": map[string]any{"type": "string", "description": "Explanation of this option"},
							},
							"required": []string{"label", "description"},
						},
					},
				},
				"required": []string{"question", "header", "multiSelect"},
			},
		},
	}
}

func (t AskUserTool) Required() []string { return []string{"questions"} }

func (t AskUserTool) Parallel() bool { return false }

func (t AskUserTool) ExecuteContext(ctx context.Context, args string) (string, error) {
	// This tool is special - it doesn't execute immediately.
	// Instead, it returns an error that signals the agent to wait for user input.
	var input struct {
		Questions []Question `json:"questions"`
	}
	if err := json.Unmarshal([]byte(args), &input); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	// ACP mode: use elicitation to ask the user.
	// The ACP connection is attached to the context in streamToACP.
	if conn := acpctx.Conn(ctx); conn != nil {
		return t.executeACP(ctx, conn, input.Questions)
	}

	// Return a special error that contains the questions
	// The agent will catch this and wait for TUI to provide answers
	return "", &AskUserQuestionError{
		ToolName:  ToolNameAskUser,
		Args:      args,
		Questions: input.Questions,
	}
}

// executeACP handles AskUserQuestion in ACP mode by sending an elicitation
// request to the client (e.g., Zed), which renders a form for the user.
func (t AskUserTool) executeACP(ctx context.Context, conn *acp.AgentSideConnection, questions []Question) (string, error) {
	// Build the message from the first question (or a summary if multiple).
	message := questions[0].Question
	if len(questions) > 1 {
		message = fmt.Sprintf("Please answer %d questions", len(questions))
	}

	// Convert Tachi questions to ACP elicitation schema.
	schema := buildElicitationSchema(questions)

	// Send the elicitation request using the standard SDK API.
	// SessionId is set directly on the request struct (the local SDK
	// fork includes this field; upstream acp-go-sdk may add it later).
	sessionID := acpctx.SessionID(ctx)
	if sessionID == "" {
		return "", fmt.Errorf("elicitation: no session ID in context")
	}

	req := acp.UnstableCreateElicitationRequest{
		Form: &acp.UnstableCreateElicitationForm{
			SessionId:       sessionID,
			Message:         message,
			Mode:            "form",
			RequestedSchema: schema,
		},
	}
	resp, err := conn.UnstableCreateElicitation(ctx, req)
	if err != nil {
		return "", fmt.Errorf("elicitation failed: %w", err)
	}

	switch {
	case resp.Accept != nil:
		// User provided input — map ACP content back to the Tachi answer format.
		answers := make(map[string]string)
		if resp.Accept.Content != nil {
			for k, v := range resp.Accept.Content {
				switch val := v.(type) {
				case string:
					answers[k] = val
				default:
					b, _ := json.Marshal(v)
					answers[k] = string(b)
				}
			}
		}
		result, _ := json.Marshal(map[string]any{
			"questions":   questions,
			"answers":     answers,
			"annotations": map[string]string{},
		})
		return string(result), nil

	case resp.Decline != nil:
		return "", fmt.Errorf("user declined to answer")

	case resp.Cancel != nil:
		return "", fmt.Errorf("elicitation cancelled")

	default:
		return "", fmt.Errorf("unexpected elicitation response")
	}
}

// buildElicitationSchema converts Tachi's Question format to an ACP
// UnstableElicitationSchema (JSON Schema) for form-based elicitation.
func buildElicitationSchema(questions []Question) acp.UnstableElicitationSchema {
	properties := make(map[string]any)
	required := make([]string, 0, len(questions))

	for i, q := range questions {
		propName := fmt.Sprintf("question_%d", i)
		required = append(required, propName)

		if len(q.Options) > 0 {
			// Multiple choice: use oneOf for single-select, array for multi-select.
			if q.MultiSelect {
				// Multi-select: array of items with anyOf.
				items := make([]map[string]any, 0, len(q.Options))
				for _, opt := range q.Options {
					items = append(items, map[string]any{
						"const":       opt.Label,
						"title":       opt.Label,
						"description": opt.Description,
					})
				}
				properties[propName] = map[string]any{
					"type":        "array",
					"title":       q.Header,
					"description": q.Question,
					"items": map[string]any{
						"anyOf": items,
					},
				}
			} else {
				// Single-select: oneOf with const/title/description.
				oneOf := make([]map[string]any, 0, len(q.Options))
				for _, opt := range q.Options {
					oneOf = append(oneOf, map[string]any{
						"const":       opt.Label,
						"title":       opt.Label,
						"description": opt.Description,
					})
				}
				properties[propName] = map[string]any{
					"type":        "string",
					"title":       q.Header,
					"description": q.Question,
					"oneOf":       oneOf,
				}
			}
		} else {
			// Free-text input.
			properties[propName] = map[string]any{
				"type":        "string",
				"title":       q.Header,
				"description": q.Question,
			}
		}
	}

	description := ""
	if len(questions) > 0 {
		description = questions[0].Question
	}
	desc := &description

	return acp.UnstableElicitationSchema{
		Description: desc,
		Properties:  properties,
		Required:    required,
		Type:        "object",
	}
}
