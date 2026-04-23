package tui

func commitUserPrompt() string {
	return `## Context to gather (use the Bash tool — do not assume output without running commands)

Run these in the current working directory (Bash’s cwd is the process cwd) and use the output as context:

- Current git status: "git status"
- Staged and unstaged changes vs HEAD: "git diff HEAD" (if that fails, e.g. no commits yet, use "git diff" and "git status")
- Current branch: "git branch --show-current"
- Recent commits: "git log --oneline -10"

## Your task

From the information above, create a **single** git commit. Use the **Bash** tool to run the necessary git commands only: inspect with "git status" / "git diff", stage with "git add" as needed, then "git commit" with one clear message. Do not use Read, Write, or Edit to change files for this task unless it is absolutely required to unblock git.

If there is nothing to commit, say so and do not create an empty commit.
`
}
