package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/debuglog"
)

const (
	maxPatchSize = 32 * 1024 // 32KB max patch output
)

// WorktreeManager manages git worktree creation and cleanup for sub-agent isolation.
type WorktreeManager struct {
	worktreeDir   string
	defaultBranch string // empty = detached HEAD
	cleanup       bool
	logger        *debuglog.Logger
}

// NewWorktreeManager creates a new WorktreeManager from config.
func NewWorktreeManager(cfg *config.Config, logger *debuglog.Logger) *WorktreeManager {
	wm := &WorktreeManager{
		worktreeDir:   cfg.Subagent.WorktreeDir,
		defaultBranch: cfg.Subagent.WorktreeBranch,
		cleanup:       true,
		logger:        logger,
	}
	if cfg.Subagent.WorktreeCleanup != nil {
		wm.cleanup = *cfg.Subagent.WorktreeCleanup
	}
	if wm.worktreeDir == "" {
		wm.worktreeDir = os.TempDir()
	}
	return wm
}

// Create establishes a git worktree, runs the callback inside it, and cleans up.
//
// When branch is empty, a detached HEAD worktree is created from HEAD.
// When branch is non-empty, the branch is fetched if needed, then checked out
// (always as detached HEAD to avoid "branch already checked out" conflicts).
//
// If worktree creation fails, the callback is executed in the original context
// (graceful degradation to shared-directory mode).
//
// After the callback completes (success or failure), if cleanup is enabled, the
// worktree is removed. Any file modifications in the worktree are collected as a
// unified diff patch and appended to the callback result.
func (wm *WorktreeManager) Create(
	ctx context.Context,
	branch string,
	fn func(ctx context.Context, worktreePath string) (string, error),
) (string, error) {
	// Use default branch if none specified
	if branch == "" {
		branch = wm.defaultBranch
	}

	worktreePath, err := wm.createWorktree(ctx, branch)
	if err != nil {
		wm.logger.Log("WorktreeManager: failed to create worktree: %v, falling back to shared dir", err)
		// Degrade: run callback in original context. Prepend a notice so
		// the caller is aware that file changes happened in shared directory.
		result, fnErr := fn(ctx, "")
		if result != "" {
			result = "[WARNING: worktree unavailable — ran in shared directory. File changes may affect the main working tree.]\n\n" + result
		}
		return result, fnErr
	}

	// Inject worktree path into context
	wtCtx := wdctx.WithDir(ctx, worktreePath)

	// Run the callback
	result, err := fn(wtCtx, worktreePath)

	// Collect patch before cleanup
	patch := wm.collectPatch(ctx, worktreePath)

	// Cleanup
	if wm.cleanup {
		wm.removeWorktree(worktreePath)
	}

	// Append patch to result
	if patch != "" {
		if result != "" {
			result += "\n\n---\n[WORKTREE_PATCH]\n" + patch + "\n[/WORKTREE_PATCH]"
		} else {
			result = "---\n[WORKTREE_PATCH]\n" + patch + "\n[/WORKTREE_PATCH]"
		}
	}

	return result, err
}

// createWorktree creates a git worktree at a temporary path.
// Returns the worktree path on success, or an error if creation fails.
func (wm *WorktreeManager) createWorktree(ctx context.Context, branch string) (string, error) {
	// Check if we're in a git repository
	if !isGitRepo() {
		return "", fmt.Errorf("not a git repository")
	}

	// Reject branch names that start with "origin/" — these look like remote
	// tracking refs and would create a confusing local branch named "origin/xxx".
	if trimmed := strings.TrimPrefix(branch, "origin/"); trimmed != branch {
		return "", fmt.Errorf("branch name %q looks like a remote tracking ref; use %q instead", branch, trimmed)
	}

	worktreeName := "tachi-subagent-" + uuid.New().String()[:8]
	worktreePath := filepath.Join(wm.worktreeDir, worktreeName)

	args := []string{"worktree", "add", "--detach", worktreePath}

	if branch != "" {
		// Ensure the branch exists locally; fetch if not
		if !branchExists(branch) {
			wm.logger.Log("WorktreeManager: branch %q not found locally, fetching from origin", branch)
			if err := fetchBranch(ctx, branch); err != nil {
				return "", fmt.Errorf("failed to fetch branch %q: %w", branch, err)
			}
			// After fetch, verify it exists
			if !branchExists(branch) {
				return "", fmt.Errorf("branch %q does not exist after fetch", branch)
			}
		}
		args = append(args, branch)
	} else {
		args = append(args, "HEAD")
	}

	cmd := exec.CommandContext(ctx, "git", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git worktree add failed: %w (stderr: %s)", err, stderr.String())
	}

	return worktreePath, nil
}

// removeWorktree removes a git worktree. Errors are logged but not returned —
// cleanup failure should not affect the result.
func (wm *WorktreeManager) removeWorktree(path string) {
	cmd := exec.Command("git", "worktree", "remove", "--force", path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		wm.logger.Log("WorktreeManager: failed to remove worktree %s: %v (stderr: %s)", path, err, stderr.String())
	}
}

// collectPatch detects file changes in the worktree and generates a unified diff.
// Returns empty string if there are no changes.
// Uses a fresh context with timeout because this runs after callback completion
// when the original ctx may already be cancelled.
const patchTimeout = 30 * time.Second

func (wm *WorktreeManager) collectPatch(_ context.Context, worktreePath string) string {
	if worktreePath == "" {
		return ""
	}

	bgCtx, cancel := context.WithTimeout(context.Background(), patchTimeout)
	defer cancel()

	// Stage all changes (including untracked files) so diff --cached captures them
	cmd := exec.CommandContext(bgCtx, "git", "-C", worktreePath, "add", "-A")
	if err := cmd.Run(); err != nil {
		wm.logger.Log("WorktreeManager: git add -A failed: %v", err)
		return ""
	}

	// Quick check: are there staged changes?
	cmd = exec.CommandContext(bgCtx, "git", "-C", worktreePath, "diff", "--cached", "--stat")
	statOut, err := cmd.Output()
	if err != nil || len(bytes.TrimSpace(statOut)) == 0 {
		return ""
	}

	// Generate full diff
	cmd = exec.CommandContext(bgCtx, "git", "-C", worktreePath, "diff", "--cached")
	diffOut, err := cmd.Output()
	if err != nil {
		wm.logger.Log("WorktreeManager: git diff failed: %v", err)
		return ""
	}

	patch := string(diffOut)

	// Truncate if too large, at the last complete line to avoid breaking diff format
	if len(patch) > maxPatchSize {
		truncated := patch[:maxPatchSize]
		if lastNL := strings.LastIndexByte(truncated, '\n'); lastNL > maxPatchSize/2 {
			truncated = truncated[:lastNL]
		}
		patch = truncated + fmt.Sprintf("\n... patch truncated (total: %d bytes). Use worktree_cleanup=false to inspect.\n", len(diffOut))
	}

	return patch
}

// isGitRepo checks if the current directory is inside a git repository.
func isGitRepo() bool {
	cmd := exec.Command("git", "rev-parse", "--git-dir")
	return cmd.Run() == nil
}

// branchExists checks if a local git branch exists.
func branchExists(branch string) bool {
	cmd := exec.Command("git", "rev-parse", "--verify", "refs/heads/"+branch)
	return cmd.Run() == nil
}

// fetchBranch fetches a branch from origin and creates a local tracking branch.
func fetchBranch(ctx context.Context, branch string) error {
	cmd := exec.CommandContext(ctx, "git", "fetch", "origin", branch+":"+branch)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w (stderr: %s)", err, stderr.String())
	}
	return nil
}