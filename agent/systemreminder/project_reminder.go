package systemreminder

import (
	"context"
	"os"
)

// ProjectContextReminder injects the contents of .tachi.md (if present) on the
// first message of a brand-new conversation. This gives the model awareness of
// the project context without bloating the static system prompt.
type ProjectContextReminder struct{}

func (ProjectContextReminder) Generate(ctx context.Context, rctx Context) []string {
	if !rctx.IsFirstMessage {
		return nil
	}

	// Read .tachi.md relative to the process working directory.
	data, err := os.ReadFile(".tachi.md")
	if err != nil {
		return nil // No .tachi.md — nothing to inject.
	}

	content := string(data)
	if content == "" {
		return nil
	}

	return []string{
		"## Project Context (.tachi.md)",
		"",
		content,
	}
}
