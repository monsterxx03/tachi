package commands

import (
	"fmt"
	"strings"
)

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
2. Write the report to .tachi/reviews/[timestamp]-[summary]-review.md, where [timestamp] is obtained by running 'date +%Y-%m-%d-%H%M' via Bash (precise to the minute), and [summary] is a short 2-4 word kebab-case summary of the changes being reviewed (e.g. "fix-input-wrapping", "add-lsp-support"). Do NOT use the git branch name.
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

// ---------------------------------------------------------------------------
// Adversarial (multi-round) review — shared by TUI and ACP.
// See docs/2026-07-30-adversarial-review-design.md.
// ---------------------------------------------------------------------------

// ReviewRole is the role of one adversarial review round.
type ReviewRole int

const (
	RoleReviewer   ReviewRole = iota // 0 — full review of all changes
	RoleChallenger                   // 1 — challenge prior rounds, add findings
	RoleJudge                        // 2 — adjudicate disagreements, summarize
)

// RoundReport records the status of one prior round's report. The prompt
// builder and the orchestrators (TUI/ACP) share this type: Saved=false
// entries are flagged in the next round's prompt as "missing, skip".
type RoundReport struct {
	Round int
	Path  string // orchestrator-allocated expected path
	Saved bool   // whether the report was successfully written to disk
}

// ResolveRole returns the role for a given round; the final round is always
// the Judge.
func ResolveRole(round, totalRounds int) ReviewRole {
	if round == totalRounds {
		return RoleJudge
	}
	return ReviewRole((round - 1) % 3)
}

// RoleName returns the Chinese display name for a review role (used in the
// multi-round banner and the TUI statusbar). Switch form with an unknown
// fallback — a future ReviewRole value must not panic on a slice index.
func RoleName(r ReviewRole) string {
	switch r {
	case RoleReviewer:
		return "审查者"
	case RoleChallenger:
		return "挑战者"
	case RoleJudge:
		return "裁决者"
	default:
		return "审查者"
	}
}

// RoleEnName returns the English display name for a review role (used in the
// prompt header, e.g. "Round 1/3 — Reviewer"). Switch form with an unknown
// fallback, mirroring RoleName — a future ReviewRole value must not panic on
// a slice index.
func RoleEnName(r ReviewRole) string {
	switch r {
	case RoleReviewer:
		return "Reviewer"
	case RoleChallenger:
		return "Challenger"
	case RoleJudge:
		return "Judge"
	default:
		return "Reviewer"
	}
}

// roleFileSuffix returns the report filename role suffix.
func roleFileSuffix(r ReviewRole) string {
	switch r {
	case RoleReviewer:
		return "review"
	case RoleChallenger:
		return "challenge"
	case RoleJudge:
		return "judge"
	default:
		// Unknown role — fall back instead of panicking on slice index if a
		// future ReviewRole value is added without updating this mapping.
		return "review"
	}
}

// ReportPathFor returns the exact report path for a round:
// "<dir>/round-<N>-<role>-<model>.md". The orchestrator owns this path — it
// is allocated BEFORE the round starts, written verbatim into the prompt, and
// verified with os.Stat after the round ends. ReviewOrchestrator.Next (write
// instruction) and Complete (on-disk verification) both use this function so
// both refer to the same file.
func ReportPathFor(dir string, round int, role ReviewRole, model string) string {
	return fmt.Sprintf("%s/round-%d-%s-%s.md", dir, round, roleFileSuffix(role), sanitizeFileName(model))
}

// sanitizeFileName replaces path-illegal characters in a model ID with '-'
// (e.g. "qwen3:32b" → "qwen3-32b") so the model can appear in a filename.
func sanitizeFileName(s string) string {
	replacer := strings.NewReplacer(
		"/", "-", "\\", "-", ":", "-", " ", "-",
		"?", "-", "*", "-", `"`, "-", "<", "-", ">", "-", "|", "-",
	)
	return replacer.Replace(s)
}

// BuildReviewPrompt constructs the complete user message for one adversarial
// review round. role/round/totalRounds identify the round; outPath is the
// exact report path the orchestrator has allocated (no placeholders — the LLM
// must not invent its own filename); prev carries the status of prior rounds'
// reports (Saved=false entries are flagged as missing and skipped).
func BuildReviewPrompt(role ReviewRole, round, totalRounds int, outPath string, prev []RoundReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "你是代码审查的第 %d 轮%s (Round %d/%d — %s)。\n\n",
		round, RoleName(role), round, totalRounds, RoleEnName(role))

	// Context gathering — identical to the single-round review prompt. Every
	// round re-runs it: rounds run in isolated forks with no shared context,
	// so each round must gather the diff itself.
	b.WriteString(`## Context to gather (use the Bash tool — do not assume output without running commands)

Run these in the session's working directory (Bash's cwd) and use the output as context:

- git diff HEAD (or "git diff --cached" for staged-only; if HEAD doesn't exist, use "git diff" + "git status")
- git status
- git branch --show-current
- git log --oneline -5

`)

	// Prior report status — real paths, truthfully reported.
	if len(prev) > 0 {
		b.WriteString("## 前序报告\n\n")
		for _, r := range prev {
			if r.Saved {
				fmt.Fprintf(&b, "  - Round %d: %s — 用 ReadFile 阅读\n", r.Round, r.Path)
			} else {
				fmt.Fprintf(&b, "  - Round %d: ⚠️ 该轮未能成功保存报告，跳过\n", r.Round)
			}
		}
		b.WriteString("\n")
	}

	// Role-specific task.
	switch role {
	case RoleReviewer:
		b.WriteString(`## Your task

Perform a thorough code review of ALL changes visible in the diff above. Analyze each change from the following perspectives:

1. **Correctness** — Is the logic sound? Any edge cases, race conditions, nil pointer risks, off-by-one errors, or type mismatches? Do function signatures match their callers?

2. **Code Quality** — Is the code readable, well-structured, and idiomatic? Naming clarity, comment quality, separation of concerns, error handling patterns, test coverage?

3. **Efficiency** — Any performance concerns? Unnecessary allocations, N+1 queries, suboptimal algorithms, excessive copying, missing cache opportunities?

4. **Security** — Any injection vulnerabilities, hardcoded secrets, insufficient input validation, privilege issues?

5. **Maintainability** — Are there hard-to-change coupling, duplicated logic, overly complex functions, missing abstraction boundaries?

注意：本轮审查范围是**全部变更**。你的报告将被下一轮的挑战者审查，请确保足够详细。

`)
	case RoleChallenger:
		b.WriteString(`## Your task

你的队友 (Reviewer/Challenger) 已完成前序轮次。请用 ReadFile 阅读上面的前序报告，然后：

1. 阅读所有前序报告
2. 对每条发现标注立场：
   - ✅ Agree — 同意并补充理由
   - ❌ Disagree — 反驳并说明理由
   - ➕ Addition — 全新的发现
3. 特别注意前序轮次可能遗漏的 edge case、安全面、性能瓶颈

`)
	case RoleJudge:
		// Defensive: the judge prompt must never read "前 0 轮" (only reachable
		// if BuildReviewPrompt is called with round==1, which the callers never
		// do — single-round /review takes a separate path).
		preceding := round - 1
		if preceding <= 0 {
			b.WriteString(`## Your task

请综合当前变更直接给出裁决，并按严重性排序输出统一的审查报告：

1. 对每一条发现给出裁决（Confirmed / Disputed / Rejected）
2. 生成统一的最终报告，按严重性排序
3. 给出整体评估与建议

`)
		} else {
			fmt.Fprintf(&b, `## Your task

你的队友已完成前 %d 轮审查。请用 ReadFile 阅读上面的所有前序报告，然后：

1. 对每一条分歧做出最终裁决（Confirmed / Disputed / Rejected）
2. 生成统一的最终报告，按严重性排序
3. 给出整体评估与建议

`, preceding)
		}
	}

	// Shared output format.
	b.WriteString(`### 输出格式

每个发现标注：
- **File** 和行号
- **Severity**: 🐛 Bug / ⚠️ Warning / 💡 Suggestion
- **Category**: Correctness / Quality / Efficiency / Security / Maintainability
- 具体的理由和修复建议

### Output language

All output — including the review report saved to disk — **must** be written in the language specified by your system prompt's reply language instruction. Do not switch languages mid-review.

### Rules

- You may use **ReadFile** to read specific files for deeper context, **Glob** to discover related files, **Grep** to find usages/references across the codebase, and **Bash** for git commands and basic inspection.
- Do NOT modify any files — this is a read-only review. The **WriteFile** tool may only be used to write the final review report (see below).
- Focus on the **changes** in the diff, not the entire codebase.
- If the diff is empty (no changes to review), state that clearly.
- Provide specific, actionable feedback with line references when possible.

`)

	// Round focus: intermediate vs final.
	if round == totalRounds {
		b.WriteString("这是**最终轮**，你需要做出最终裁决并生成完整的执行摘要。\n")
	} else {
		b.WriteString("这是中间裁决，请标记仍有争议的区域供后续轮次聚焦。\n")
	}

	// Save instruction — orchestrator-owned path, written verbatim.
	fmt.Fprintf(&b, "\n### 保存报告\n\n完成后用 WriteFile 保存报告到：%s（编排器给出的确切路径，目录已创建，无需 mkdir）\n", outPath)

	return b.String()
}
