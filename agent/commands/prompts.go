package commands

// DefaultReviewMaxIterations is the default iteration budget for /review.
const DefaultReviewMaxIterations = 200

// DefaultReviewAllowedTools returns a fresh copy of the default allowed tool
// list for /review. Callers must not modify the returned slice.
func DefaultReviewAllowedTools() []string {
	return []string{"Bash", "ReadFile", "WriteFile", "Glob", "Grep"}
}

// ReviewUserPrompt builds the prompt for a code review of current repo changes.
// The forked agent will only have Bash, ReadFile, Glob, Grep, and WriteFile tools.
func ReviewUserPrompt() string {
	return `## Context to gather (use the Bash tool — do not assume output without running commands)

Run these in the current working directory (Bash's cwd is the process cwd) and use the output as context:

- git diff HEAD (or "git diff --cached" for staged-only; if HEAD doesn't exist, use "git diff" + "git status")
- git status
- git branch --show-current
- git log --oneline -5

## Your task

Perform a thorough code review of all changes visible in the diff above. Analyze each change from the following perspectives:

1. **Correctness** — Is the logic sound? Any edge cases, race conditions, nil pointer risks, off-by-one errors, or type mismatches? Do function signatures match their callers?

2. **Code Quality** — Is the code readable, well-structured, and idiomatic? Naming clarity, comment quality, separation of concerns, error handling patterns, test coverage?

3. **Efficiency** — Any performance concerns? Unnecessary allocations, N+1 queries, suboptimal algorithms, excessive copying, missing cache opportunities?

4. **Security** — Any injection vulnerabilities, hardcoded secrets, insufficient input validation, privilege issues?

5. **Maintainability** — Are there hard-to-change coupling, duplicated logic, overly complex functions, missing abstraction boundaries?

### Output language

All output — including the review report saved to disk — **must** be written in the language specified by your system prompt's reply language instruction. Do not switch languages mid-review.

### Rules

- You may use **ReadFile** to read specific files for deeper context, **Glob** to discover related files, **Grep** to find usages/references across the codebase, and **Bash** for git commands and basic inspection.
- Do NOT modify any files — this is a read-only review. The **WriteFile** tool may only be used to write the final review report (see below).
- Do NOT run build/test commands unless needed to verify correctness (e.g. compilation check).
- Focus on the **changes** in the diff, not the entire codebase.
- If the diff is empty (no changes to review), state that clearly.
- Provide specific, actionable feedback with line references when possible.

### Output format

Present your review in a clear structured format. Group findings by file, then by concern. For each finding, state:
- **File** and relevant line range
- **Severity**: 🐛 Bug / ⚠️ Warning / 💡 Suggestion
- **Category**: Correctness / Quality / Efficiency / Security / Maintainability
- **Explanation** with specific reasoning
- **Suggestion** (how to fix or improve)

End with a brief overall assessment of the change set.

### Save the review report

After completing the review, save the full report to a file using **WriteFile**:

1. Ensure the directory .tachi/reviews/ exists (use 'mkdir -p .tachi/reviews' via Bash).
2. Write the report to .tachi/reviews/[date]-[branch-or-shortid]-review.md, where [date] is today's date in YYYY-MM-DD format and [branch-or-shortid] is the current git branch name or a short identifier.
3. The report file should contain the complete review output including all findings and the overall assessment.`
}

// InitPromptTemplate is the prompt sent to LLM to generate .tachi.md.
const InitPromptTemplate = `Create (or improve) a .tachi.md file at the repo root. This file is read by future coding agent instances — write for agents, not humans. Keep it under 200 lines, terse and dense.

What to include:
1. Build, lint, test commands (including how to run a single test).
2. Language version info — extract from project config files (e.g. Go version from go.mod, Node.js version from package.json/engines, Python from .python-version/Pipfile/pyproject.toml, Rust from Cargo.toml). Include as a compact note in the Build & Test section or a dedicated table row.
3. High-level architecture — the "big picture" that requires reading multiple files to discover. Use compact formats (tables, one-liners, signatures) over prose.

Rules:
- If .tachi.md exists, read it first and improve it in-place.
- No generic advice ("write tests", "be helpful", "don't hardcode secrets").
- No listing every file/dir — focus on relationships and non-obvious design decisions.
- No made-up sections ("Common Tasks", "Tips", "Support").
- If .cursor/rules/, .cursorrules, or .github/copilot-instructions.md exist, extract their key constraints.
- If README.md exists, extract its essential info.
- Use the WriteFile tool to write the result.

Gather context first:
  git status
  git branch --show-current
  git log --oneline -5
  find . -maxdepth 1 -name 'Makefile' -o -name 'go.mod' -o -name 'package.json' -o -name 'README.md' -o -name '.cursorrules' -o -name 'CLAUDE.md' 2>/dev/null
  ls -la

Then read key files, understand the architecture, and produce the .tachi.md.`
