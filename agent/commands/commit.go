package commands

import (
	"fmt"
	"strings"
)

// ModelToCoAuthor converts a model name to a valid Co-authored-by name + email pair.
// The email local part is the model name lowercased with non-alphanumeric chars replaced by hyphens.
func ModelToCoAuthor(modelName string) string {
	if modelName == "" {
		return "AI <ai@tachi>"
	}
	emailLocal := SanitizeModelName(modelName)
	return modelName + " <" + emailLocal + "@tachi>"
}

// SanitizeModelName lowercases and replaces non-alphanumeric sequences with a single hyphen.
func SanitizeModelName(name string) string {
	var sb strings.Builder
	sb.Grow(len(name))
	prevDash := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
			prevDash = false
		} else {
			if !prevDash {
				sb.WriteRune('-')
				prevDash = true
			}
		}
	}
	return sb.String()
}

// CommitUserPrompt builds the commit prompt including co-author instructions
// for the given model name.
func CommitUserPrompt(modelName string) string {
	coAuthor := ModelToCoAuthor(modelName)
	backtick := "`"
	return fmt.Sprintf(`## Context to gather (use the Bash tool — do not assume output without running commands)

Run these in the current working directory (the Bash tool's cwd) and use the output as context:

- Current git status: "git status"
- Staged and unstaged changes vs HEAD: "git diff HEAD" (if that fails, e.g. no commits yet, use "git diff" and "git status")
- Current branch: "git branch --show-current"
- Recent commits: "git log --oneline -10"

## Your task

From the information above, create a **single** git commit. Use the **Bash** tool to run the necessary git commands only: inspect with "git status" / "git diff", stage with "git add" as needed, then "git commit" with one clear message. Do not use Read, Write, or Edit to change files for this task unless it is absolutely required to unblock git.

If there is nothing to commit, say so and do not create an empty commit.

### Commit message style

- **Use Conventional Commits**: start the subject with a type prefix describing the nature of the change, then a concise summary of this commit's theme. Use the most fitting type:
  - feat — new feature
  - fix — bug fix
  - refactor — code restructure without behavior change
  - docs — documentation only
  - style — formatting / whitespace (non-functional)
  - test — tests only
  - chore — maintenance, deps, tooling
  - perf — performance improvement
  - Optionally add a scope in parentheses, e.g. feat(websearch): ...
- The subject line must stay **under 72 characters** (ideally 50-60) so it reads well in the terminal.
- If a body is needed, **wrap every body line at 72 characters** — git log indents the body by 4 spaces in an 80-column terminal, so 72 keeps lines from wrapping.
- Write a high-level summary of what changed and why. Do NOT include implementation details such as function names, variable names, or line-level changes.
- Prefer a single concise subject line; only add a body via an extra -m flag if the change genuinely needs more context.

Example:
  git commit -m "feat(websearch): add exa provider with quota fallback" -m "Brave rate limits now fall back to exa automatically, and credit-exhausted providers pause until the next billing cycle." -m "Co-authored-by: SomeModel-1.0 <somemodel-1.0@tachi>"

### Co-author trailer

You MUST append a Co-authored-by trailer to every commit message using the multi-line %s-m technique:

git commit -m "Summarize your changes" -m "Co-authored-by: %s"

The second -m line adds a Co-authored-by trailer that GitHub recognizes

Example:
  git commit -m "Fix null pointer in config loader" -m "Co-authored-by: SomeModel-1.0 <somemodel-1.0@tachi>"
`, backtick, coAuthor)
}
