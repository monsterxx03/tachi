package tui

import (
	"fmt"

	"github.com/monsterxx03/tachi/agent"
)

// commitUserPrompt builds the commit prompt including co-author instructions
// for the current model.
func commitUserPrompt(modelName string) string {
	coAuthor := agent.ModelToCoAuthor(modelName)
	backtick := "`"
	return fmt.Sprintf(`## Context to gather (use the Bash tool — do not assume output without running commands)

Run these in the current working directory (Bash's cwd is the process cwd) and use the output as context:

- Current git status: "git status"
- Staged and unstaged changes vs HEAD: "git diff HEAD" (if that fails, e.g. no commits yet, use "git diff" and "git status")
- Current branch: "git branch --show-current"
- Recent commits: "git log --oneline -10"

## Your task

From the information above, create a **single** git commit. Use the **Bash** tool to run the necessary git commands only: inspect with "git status" / "git diff", stage with "git add" as needed, then "git commit" with one clear message. Do not use Read, Write, or Edit to change files for this task unless it is absolutely required to unblock git.

If there is nothing to commit, say so and do not create an empty commit.

### Co-author trailer

You MUST append a Co-authored-by trailer to every commit message using the multi-line %s-m technique:

git commit -m "Summarize your changes" -m "Co-authored-by: %s"

The second -m line adds a Co-authored-by trailer that GitHub recognizes

Example:
  git commit -m "Fix null pointer in config loader" -m "Co-authored-by: SomeModel-1.0 <somemodel-1.0@tachi>"
`, backtick, coAuthor)
}
