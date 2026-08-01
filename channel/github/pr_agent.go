package github

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/google/go-github/v69/github"
	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/permission"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/pkg/set"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

// PRResult describes the outcome of a PR generation agent turn.
type PRResult struct {
	// Branch is the name of the branch that was pushed.
	Branch string

	// PRNumber is the number of the created PR, or 0 if not yet created.
	PRNumber int

	// PRURL is the URL of the created PR.
	PRURL string

	// Err is non-nil when the PR generation failed unrecoverably.
	Err error
}

// generateBranchName creates a git branch name from the issue number and title.
func generateBranchName(issueNum int, title string) string {
	// Sanitize title: lowercase, replace special chars with hyphens.
	sanitized := strings.ToLower(title)
	sanitized = strings.NewReplacer(
		" ", "-", "_", "-", ".", "-", ":", "", ",", "", "(", "", ")", "",
		"[", "", "]", "", "'", "", "\"", "", "`", "", "?", "", "!", "", "/", "-",
		"#", "", "@", "", "&", "", "*", "", "~", "", "^", "",
	).Replace(sanitized)
	sanitized = strings.Trim(sanitized, "-")

	// Use rune-aware truncation to avoid splitting multi-byte UTF-8 (CJK, emoji).
	sanitized = strutil.TruncatePlain(sanitized, 40)
	sanitized = strings.TrimRight(sanitized, "-")
	if sanitized == "" {
		sanitized = "fix"
	}

	// Append short UUID to prevent collisions on retry.
	suffix := strutil.ShortUUID(8)
	return fmt.Sprintf("tachi/issue-%d-%s-%s", issueNum, sanitized, suffix)
}

// PRGenerationConfig holds parameters for PR generation.
type PRGenerationConfig struct {
	Provider      llm.Provider
	Token         string
	Issue         *github.Issue
	Comments      []*github.IssueComment
	Labels        []*github.Label // issue labels for PR gate checking
	RepoName      string
	RepoLocalPath string
	BotLogin      string
	Owner         string
	Repo          string
	DefaultBranch string // base branch for PR (default: "main")
	Behavior      *BehaviorConfig
	Security      *SecurityConfig // needed for PRGate config and bash allow
	ToolNames     []string
	BashAllow     []string
	Logger        *logger.Logger
}

// WorktreeManager manages a single worktree for PR generation.
type prWorktree struct {
	basePath string // local clone path
	wtPath   string // worktree path
	name     string // worktree name
}

// createWorktree creates a git worktree from the local clone.
func (wt *prWorktree) create(ctx context.Context, branch, baseBranch string) error {
	wtName := "tachi-pr-" + strutil.ShortUUID(8)
	wt.wtPath = filepath.Join(os.TempDir(), wtName)
	wt.name = wtName

	args := []string{"worktree", "add", "-b", branch, wt.wtPath, "origin/" + baseBranch}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = wt.basePath
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git worktree add: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// cleanup removes the worktree.
func (wt *prWorktree) cleanup() {
	cmd := exec.Command("git", "worktree", "remove", "--force", wt.wtPath)
	cmd.Dir = wt.basePath
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		_ = err // best-effort cleanup, log would be nice but we're in a defer
	}
}

// RunPRGeneration runs the PR generation agent.
// It creates a worktree, runs the agent to implement changes, commits, pushes,
// and returns the branch name. The caller is responsible for creating the PR.
func RunPRGeneration(ctx context.Context, cfg *PRGenerationConfig) *PRResult {
	log := cfg.Logger
	log.Info(ctx, "github: starting PR generation",
		"repo", cfg.RepoName, "issue", cfg.Issue.GetNumber())

	// --- PR Gate: check if the issue author is authorized ---
	// Fetch labels for gate checking.
	// Labels are fetched inside RunPRGeneration rather than passed from the caller
	// because the caller (channel.go) doesn't always have them available.
	// We need a GitHub client to fetch labels. The config doesn't carry one,
	// so we pass allowedAssociations and gateLabel from config, and labels
	// are fetched by the caller.
	// For now, gate check without labels — caller must pass them.
	labels := cfg.Labels
	passed, gateMsg := checkPRGate(cfg.Issue, labels, cfg.Security.PRGate.AllowedAssociations, cfg.Security.PRGate.Label)
	if !passed {
		log.Info(ctx, "github: PR gate not passed", "repo", cfg.RepoName,
			"issue", cfg.Issue.GetNumber(), "reason", gateMsg)
		return &PRResult{
			Err: fmt.Errorf("PR gate: %s", gateMsg),
		}
	}

	baseBranch := cfg.DefaultBranch
	if baseBranch == "" {
		baseBranch = "main"
	}

	// --- Create worktree ---
	branch := generateBranchName(cfg.Issue.GetNumber(), cfg.Issue.GetTitle())
	wt := &prWorktree{basePath: cfg.RepoLocalPath}
	if err := wt.create(ctx, branch, baseBranch); err != nil {
		return &PRResult{Err: fmt.Errorf("create worktree: %w", err)}
	}
	defer wt.cleanup()

	// --- Build the implementation prompt ---
	systemPrompt, userMessage := buildImplementationPrompt(cfg.Issue, cfg.Comments, cfg.RepoName, branch)

	// --- Create agent ---
	maxIter := cfg.Behavior.MaxImplementationTurns
	if maxIter <= 0 {
		maxIter = 50
	}
	implAgent := agent.NewAIAgent(cfg.Provider, maxIter)
	implAgent.SetPermissionMode(agent.PermissionModeSkip)
	if log != nil {
		implAgent.SetLogger(log)
	}

	// Register implementation tools.
	registerImplementationTools(implAgent, cfg.ToolNames)

	// Set up Bash whitelist: allow + "*" ask fallback (fail-closed).
	if len(cfg.BashAllow) > 0 {
		policy := permission.NewPolicy(permission.Rules{
			Deny:  permission.BuiltinDenyRules,
			Allow: cfg.BashAllow,
			Ask:   []string{"*"}, // everything not allowed → ask → denied (Skip mode)
		}, permission.Rules{})
		implAgent.SetPermissionPolicy(policy)
	}

	// Restrict WriteFile to the worktree directory.
	ctx = tools.WithPathPolicy(ctx, &tools.PathPolicy{
		AllowedWriteDirs: []string{wt.wtPath},
	})
	ctx = wdctx.WithDir(ctx, wt.wtPath)

	// --- Run the agent ---
	eventCh := implAgent.RunOneOffStream(ctx, cfg.Provider, systemPrompt, userMessage, llm.ChatOptions{
		MaxTokens: agent.DefaultMaxTokens,
	}, agent.OneOffMeta{
		Kind: "github-pr",
		Extra: map[string]string{
			"repo":   cfg.RepoName,
			"issue":  fmt.Sprintf("%d", cfg.Issue.GetNumber()),
			"branch": branch,
		},
	})

	var result *agent.RunResult
	for ev := range eventCh {
		switch ev.Type {
		case agent.AgentEventTurnComplete:
			result = ev.Result
		case agent.AgentEventError:
			if ev.Result != nil && ev.Result.Error != nil {
				return &PRResult{Err: fmt.Errorf("implementation agent error: %w", ev.Result.Error)}
			}
		case agent.AgentEventToolConfirmation:
			implAgent.ConfirmTool(agent.ConfirmAllowOnce)
		}
	}

	if result == nil || result.Error != nil {
		err := fmt.Errorf("implementation agent returned no result")
		if result != nil && result.Error != nil {
			err = result.Error
		}
		return &PRResult{Err: err}
	}

	log.Info(ctx, "github: implementation completed", "repo", cfg.RepoName,
		"issue", cfg.Issue.GetNumber(), "iterations", result.IterationsUsed)

	// --- Git commit and push ---
	if err := gitCommitAndPush(ctx, wt.wtPath, branch, cfg.Token, cfg.Issue); err != nil {
		return &PRResult{Err: fmt.Errorf("git push failed: %w", err)}
	}

	log.Info(ctx, "github: PR generation completed",
		"repo", cfg.RepoName, "branch", branch)

	return &PRResult{
		Branch: branch,
	}
}

// checkPRGate checks if the issue author is authorized to trigger PR generation.
// allowedAssociations specifies which author_association values are permitted
// (e.g., ["OWNER", "MEMBER", "COLLABORATOR"]). labels are the issue's labels
// — if a label matching gateLabel is found, the gate is also passed.
func checkPRGate(issue *github.Issue, labels []*github.Label, allowedAssociations []string, gateLabel string) (bool, string) {
	// Check author association.
	assoc := issue.GetAuthorAssociation()
	if set.New(allowedAssociations...).Has(assoc) {
		return true, ""
	}

	// Check for gate label.
	if gateLabel != "" && issue.GetState() == "open" {
		for _, l := range labels {
			if l.GetName() == gateLabel {
				return true, ""
			}
		}
	}

	return false, fmt.Sprintf("author association %q not in allowed list", assoc)
}

// buildImplementationPrompt builds the system and user prompts for the implementation agent.
func buildImplementationPrompt(issue *github.Issue, comments []*github.IssueComment, repoName, branch string) (systemPrompt, userMessage string) {
	systemPrompt = `You are an open-source maintainer assistant, implementing a solution for a GitHub issue.

You have full access to the repository codebase. Your task is to implement the solution
described in the issue. You should:

1. First, explore the codebase to understand the relevant code
2. Plan your implementation approach
3. Write the code changes
4. Run tests to verify your changes
5. Commit your changes

Tools available:
- ReadFile: Read file contents
- WriteFile: Write new files
- EditFile: Edit existing files (preferred for modifications)
- Bash: Run shell commands (git, go, npm, rg, etc.)
- Glob: Find files by name patterns
- Grep: Search for text patterns
- SubAgent: Delegate complex sub-tasks to child agents

IMPORTANT: You are working in a git worktree at the repository root.
- The worktree is a clean copy of the main branch
- Create a new branch for your changes (the branch is already created)
- After making changes, commit them with a descriptive message
- Run tests before committing

Git workflow:
1. git add -A && git commit -m "feat: description of changes"
2. The branch ` + "`" + branch + "`" + ` is already created and checked out

Repository: ` + "`" + repoName + "`" + `
Issue: #` + fmt.Sprintf("%d", issue.GetNumber()) + ` — ` + issue.GetTitle()

	// Build user message.
	var b strings.Builder
	b.WriteString("## Issue\n\n")
	b.WriteString(issue.GetTitle())
	b.WriteString("\n\n")
	b.WriteString(issue.GetBody())
	b.WriteString("\n\n")

	if len(comments) > 0 {
		b.WriteString("## Discussion\n\n")
		// Include last 5 human comments for context.
		count := 0
		for i := len(comments) - 1; i >= 0 && count < 5; i-- {
			c := comments[i]
			if c.User != nil && c.User.GetType() == "Bot" {
				continue
			}
			fmt.Fprintf(&b, "**%s**: %s\n\n", c.User.GetLogin(), c.GetBody())
			count++
		}
	}

	b.WriteString("## Instructions\n\n")
	b.WriteString("Implement the solution for this issue. Make sure to:\n")
	b.WriteString("1. Write clean, well-documented code\n")
	b.WriteString("2. Follow the project's existing code style and patterns\n")
	b.WriteString("3. Add or update tests as needed\n")
	b.WriteString("4. Run tests to verify your changes compile and pass\n")
	b.WriteString("5. Commit your changes with a descriptive message\n")

	return systemPrompt, b.String()
}

// buildPRTitle creates a PR title from the issue.
func buildPRTitle(issue *github.Issue) string {
	title := issue.GetTitle()
	if strings.HasPrefix(strings.ToLower(title), "feat:") ||
		strings.HasPrefix(strings.ToLower(title), "fix:") ||
		strings.HasPrefix(strings.ToLower(title), "chore:") ||
		strings.HasPrefix(strings.ToLower(title), "docs:") ||
		strings.HasPrefix(strings.ToLower(title), "refactor:") {
		return title
	}
	return "feat: " + title
}

// buildPRBody creates a PR body from the issue and implementation notes.
func buildPRBody(issue *github.Issue, implNotes string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Closes #%d\n\n", issue.GetNumber())
	b.WriteString("## Summary\n\n")
	b.WriteString(issue.GetBody())
	b.WriteString("\n\n")
	if implNotes != "" {
		b.WriteString("## Implementation Notes\n\n")
		b.WriteString(implNotes)
	}
	return b.String()
}

// gitCommitAndPush commits changes and pushes to the remote.
// Token is passed via per-command extraheader, never written to .git/config.
//
// Security note: the token is visible in the process list (ps aux) on Linux/macOS
// because it's passed as a git -c argument. Alternatives considered:
//   - Credential store: would persist token to disk, worse.
//   - SSH key: would require deploying a bot-specific SSH key, which is
//     equivalent to the PAT approach but with different management overhead.
//   - GIT_ASKPASS: still passed via env, equally visible in process list.
//
// The extraheader approach is the best balance: the token is ephemeral (per push),
// never persisted to .git/config, and the push command is short-lived.
func gitCommitAndPush(ctx context.Context, worktreePath, branch, token string, issue *github.Issue) error {
	log := logger.New("github.pr")

	// Check if there are any changes.
	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain")
	cmd.Dir = worktreePath
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("git status: %w", err)
	}
	if len(bytes.TrimSpace(out)) == 0 {
		log.Warn(ctx, "github: no changes to commit", "branch", branch)
		return nil
	}

	// git add -A
	cmd = exec.CommandContext(ctx, "git", "add", "-A")
	cmd.Dir = worktreePath
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git add: %w (output: %s)", err, string(out))
	}

	// git commit
	commitMsg := fmt.Sprintf("feat: %s\n\nCloses #%d", issue.GetTitle(), issue.GetNumber())
	cmd = exec.CommandContext(ctx, "git", "commit", "-m", commitMsg)
	cmd.Dir = worktreePath
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git commit: %w (output: %s)", err, string(out))
	}

	// git push with token via extraheader (never stored in .git/config)
	pushArgs := []string{
		"-c", fmt.Sprintf("http.extraheader=AUTHORIZATION: bearer %s", token),
		"push", "origin", branch,
	}
	cmd = exec.CommandContext(ctx, "git", pushArgs...)
	cmd.Dir = worktreePath
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git push: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}

	log.Info(ctx, "github: pushed branch", "branch", branch)
	return nil
}

// registerImplementationTools registers the allowed tools for the implementation phase.
// registerImplementationTools registers the allowed tools for the implementation phase.
// SubAgent is excluded because it requires a SubagentRunner that isn't wired in
// the current PR generation setup — the LLM will get a "tool not found" error and adapt.
func registerImplementationTools(a *agent.AIAgent, toolNames []string) {
	constructors := map[string]func() tools.Tool{
		"ReadFile":  func() tools.Tool { return tools.NewReadTool() },
		"WriteFile": func() tools.Tool { return tools.WriteTool{} },
		"EditFile":  func() tools.Tool { return tools.NewEditTool() },
		"Bash":      func() tools.Tool { return tools.NewBashTool(nil) },
		"Glob":      func() tools.Tool { return tools.GlobTool{} },
		"Grep":      func() tools.Tool { return tools.GrepTool{} },
	}

	if len(toolNames) == 0 {
		for _, constructor := range constructors {
			a.RegisterTool(constructor())
		}
		return
	}

	for _, name := range toolNames {
		if constructor, ok := constructors[name]; ok {
			a.RegisterTool(constructor())
		}
	}
}
