package subagent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/pkg/shutil"
	"github.com/monsterxx03/tachi/pkg/strutil"
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
		wm.logger.Error(ctx, "WorktreeManager: failed to create worktree, falling back to shared dir", err)
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

	worktreeName := "tachi-subagent-" + strutil.ShortUUID(8)
	worktreePath := filepath.Join(wm.worktreeDir, worktreeName)

	args := []string{"worktree", "add", "--detach", worktreePath}

	if branch != "" {
		if !branchExists(branch) {
			wm.logger.Info(ctx, "WorktreeManager: branch not found locally, fetching", "branch", branch)
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

	if err := shutil.Run(ctx, "", "git", args...); err != nil {
		return "", fmt.Errorf("git worktree add failed: %w", err)
	}

	return worktreePath, nil
}

func (wm *WorktreeManager) removeWorktree(path string) {
	if err := shutil.Run(context.Background(), "", "git", "worktree", "remove", "--force", path); err != nil {
		wm.logger.Error(context.Background(), "WorktreeManager: failed to remove worktree", err, "path", path)
	}
}

const patchTimeout = 30 * time.Second

func (wm *WorktreeManager) collectPatch(worktreePath string) string {
	if worktreePath == "" {
		return ""
	}

	bgCtx, cancel := context.WithTimeout(context.Background(), patchTimeout)
	defer cancel()

	if err := shutil.Run(bgCtx, worktreePath, "git", "add", "-A"); err != nil {
		wm.logger.Error(context.Background(), "WorktreeManager: git add -A failed", err)
		return ""
	}

	statOut, err := shutil.Output(bgCtx, worktreePath, "git", "diff", "--cached", "--stat")
	if err != nil || statOut == "" {
		return ""
	}

	diffOut, err := shutil.Output(bgCtx, worktreePath, "git", "diff", "--cached")
	if err != nil {
		wm.logger.Error(context.Background(), "WorktreeManager: git diff failed", err)
		return ""
	}

	patch := diffOut

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
	return shutil.Success(context.Background(), "", "git", "rev-parse", "--git-dir")
}

func branchExists(branch string) bool {
	return shutil.Success(context.Background(), "", "git", "rev-parse", "--verify", "refs/heads/"+branch)
}

func fetchBranch(ctx context.Context, branch string) error {
	return shutil.Run(ctx, "", "git", "fetch", "origin", branch+":"+branch)
}

// fallbackIfEmpty returns the fallback value when s is empty.
func fallbackIfEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
