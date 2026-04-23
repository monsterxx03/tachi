package tools

import (
	"context"
	"encoding/json"
	"fmt"
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

func (t AskUserTool) Name() string { return "AskUserQuestion" }

func (t AskUserTool) Description() string {
	return "Use this tool when you need to ask the user questions during execution. " +
		"This allows you to:\n" +
		"1. Gather user preferences or requirements\n" +
		"2. Clarify ambiguous instructions\n" +
		"3. Get decisions on implementation choices as you work\n" +
		"4. Offer choices to the user about what direction to take.\n\n" +
		"Usage notes:\n" +
		"- Users will always be able to select \"Other\" to provide custom text input\n" +
		"- Use multiSelect: true to allow multiple answers to be selected for a question\n" +
		"- If you recommend a specific option, make that the first option in the list and add \"(Recommended)\" at the end of the label"
}

func (t AskUserTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"questions": {
			Type:        "array",
			Description: "Questions to ask the user (1-4 questions). Each question has: question (the question text), header (short label shown as chip), options (array of 2-4 options with label and description), and multiSelect (boolean to allow multiple selections).",
			Items: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"question":    map[string]any{"type": "string", "description": "The question to ask the user"},
					"header":      map[string]any{"type": "string", "description": "Short label displayed as a chip/tag"},
					"multiSelect": map[string]any{"type": "boolean", "description": "Whether to allow multiple selections"},
					"options": map[string]any{
						"type": "array",
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
				"required": []string{"question", "header", "options", "multiSelect"},
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

	// Return a special error that contains the questions
	// The agent will catch this and wait for TUI to provide answers
	return "", &AskUserQuestionError{
		ToolName:  "AskUserQuestion",
		Args:      args,
		Questions: input.Questions,
	}
}