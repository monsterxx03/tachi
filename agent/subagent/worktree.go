package subagent

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
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
)

const maxPatchSize = 32 * 1024 // 32KB max patch output

// WorktreeManager manages git worktree creation and cleanup for sub-agent isolation.
type WorktreeManager struct {
	worktreeDir   string
	defaultBranch string // empty = detached HEAD
	cleanup       bool
	logger        *logger.Logger
}

// NewWorktreeManager creates a new WorktreeManager from subagent config.
func NewWorktreeManager(cfg config.SubagentConfig, logger *logger.Logger) *WorktreeManager {
	wm := &WorktreeManager{
		worktreeDir:   cfg.WorktreeDir,
		defaultBranch: cfg.WorktreeBranch,
		cleanup:       true,
		logger:        logger,
	}
	if cfg.WorktreeCleanup != nil {
		wm.cleanup = *cfg.WorktreeCleanup
	}
	if wm.worktreeDir == "" {
		wm.worktreeDir = os.TempDir()
	}
	return wm
}

// Create establishes a git worktree, runs the callback inside it, and cleans up.
func (wm *WorktreeManager) Create(
	ctx context.Context,
	branch string,
	fn func(ctx context.Context, worktreePath string) (string, *tools.SubagentResult, error),
) (string, *tools.SubagentResult, error) {
	if branch == "" {
		branch = wm.defaultBranch
	}

	worktreePath, err := wm.createWorktree(ctx, branch)
	if err != nil {
		wm.logger.Logf(ctx, "WorktreeManager: failed to create worktree: %v, falling back to shared dir", err)
		result, stats, fnErr := fn(ctx, "")
		if result != "" {
			result = "[WARNING: worktree unavailable — ran in shared directory. File changes may affect the main working tree.]\n\n" + result
		}
		return result, stats, fnErr
	}

	wtCtx := wdctx.WithDir(ctx, worktreePath)
	result, stats, err := fn(wtCtx, worktreePath)

	// Collect patch before cleanup (use fresh context; original may be cancelled)
	patch := wm.collectPatch(worktreePath)

	if wm.cleanup {
		wm.removeWorktree(worktreePath)
	}

	if patch != "" {
		if result != "" {
			result += "\n\n---\n[WORKTREE_PATCH]\n" + patch + "\n[/WORKTREE_PATCH]"
		} else {
			result = "---\n[WORKTREE_PATCH]\n" + patch + "\n[/WORKTREE_PATCH]"
		}
	}

	return result, stats, err
}

func (wm *WorktreeManager) createWorktree(ctx context.Context, branch string) (string, error) {
	if !isGitRepo() {
		return "", fmt.Errorf("not a git repository")
	}

	if trimmed, ok := strings.CutPrefix(branch, "origin/"); ok {
		return "", fmt.Errorf("branch name %q looks like a remote tracking ref; use %q instead", branch, trimmed)
	}

	worktreeName := "tachi-subagent-" + uuid.New().String()[:8]
	worktreePath := filepath.Join(wm.worktreeDir, worktreeName)

	args := []string{"worktree", "add", "--detach", worktreePath}

	if branch != "" {
		if !branchExists(branch) {
			wm.logger.Logf(ctx, "WorktreeManager: branch %q not found locally, fetching from origin", branch)
			if err := fetchBranch(ctx, branch); err != nil {
				return "", fmt.Errorf("failed to fetch branch %q: %w", branch, err)
			}
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

func (wm *WorktreeManager) removeWorktree(path string) {
	cmd := exec.Command("git", "worktree", "remove", "--force", path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		wm.logger.Logf(context.Background(), "WorktreeManager: failed to remove worktree %s: %v (stderr: %s)", path, err, stderr.String())
	}
}

const patchTimeout = 30 * time.Second

func (wm *WorktreeManager) collectPatch(worktreePath string) string {
	if worktreePath == "" {
		return ""
	}

	bgCtx, cancel := context.WithTimeout(context.Background(), patchTimeout)
	defer cancel()

	cmd := exec.CommandContext(bgCtx, "git", "-C", worktreePath, "add", "-A")
	if err := cmd.Run(); err != nil {
		wm.logger.Logf(context.Background(), "WorktreeManager: git add -A failed: %v", err)
		return ""
	}

	cmd = exec.CommandContext(bgCtx, "git", "-C", worktreePath, "diff", "--cached", "--stat")
	statOut, err := cmd.Output()
	if err != nil || len(bytes.TrimSpace(statOut)) == 0 {
		return ""
	}

	cmd = exec.CommandContext(bgCtx, "git", "-C", worktreePath, "diff", "--cached")
	diffOut, err := cmd.Output()
	if err != nil {
		wm.logger.Logf(context.Background(), "WorktreeManager: git diff failed: %v", err)
		return ""
	}

	patch := string(diffOut)

	if len(patch) > maxPatchSize {
		truncated := patch[:maxPatchSize]
		if lastNL := strings.LastIndexByte(truncated, '\n'); lastNL > maxPatchSize/2 {
			truncated = truncated[:lastNL]
		}
		patch = truncated + fmt.Sprintf("\n... patch truncated (total: %d bytes). Use worktree_cleanup=false to inspect.\n", len(diffOut))
	}

	return patch
}

func isGitRepo() bool {
	cmd := exec.Command("git", "rev-parse", "--git-dir")
	return cmd.Run() == nil
}

func branchExists(branch string) bool {
	cmd := exec.Command("git", "rev-parse", "--verify", "refs/heads/"+branch)
	return cmd.Run() == nil
}

func fetchBranch(ctx context.Context, branch string) error {
	cmd := exec.CommandContext(ctx, "git", "fetch", "origin", branch+":"+branch)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w (stderr: %s)", err, stderr.String())
	}
	return nil
}

// fallbackIfEmpty returns the fallback value when s is empty.
func fallbackIfEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
