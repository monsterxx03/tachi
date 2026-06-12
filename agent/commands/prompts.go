package commands

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
