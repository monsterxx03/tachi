package subagent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/debuglog"
)

// gitWorktreeAvailable returns true if git supports the worktree subcommand.
func gitWorktreeAvailable() bool {
	return exec.Command("git", "worktree").Run() == nil
}

func requireWorktreeSupport(t *testing.T) {
	t.Helper()
	if !isGitRepo() {
		t.Skip("not a git repository")
	}
	if !gitWorktreeAvailable() {
		t.Skip("git version too old (worktree needs 2.5+)")
	}
}

func TestWorktreeManager_Create_DetachedHEAD(t *testing.T) {
	requireWorktreeSupport(t)

	tmpDir := t.TempDir()
	cfg := config.SubagentConfig{
		Worktree:        true,
		WorktreeDir:     tmpDir,
		WorktreeCleanup: boolPtr(true),
	}
	wm := NewWorktreeManager(cfg, debuglog.DefaultLogger)

	result, err := wm.Create(t.Context(), "", func(ctx context.Context, wtPath string) (string, error) {
		assert.NotEmpty(t, wtPath)
		assert.DirExists(t, wtPath)

		// Verify git worktree is valid
		cmd := exec.Command("git", "-C", wtPath, "rev-parse", "--git-dir")
		out, err := cmd.Output()
		assert.NoError(t, err)
		assert.NotEmpty(t, out)

		// Write a test file to generate a patch
		f, err := os.Create(filepath.Join(wtPath, "test_worktree_patch.txt"))
		require.NoError(t, err)
		f.WriteString("hello from worktree\n")
		f.Close()

		return "task completed", nil
	})

	assert.NoError(t, err)
	assert.Contains(t, result, "task completed")
	assert.Contains(t, result, "[WORKTREE_PATCH]")
	assert.Contains(t, result, "test_worktree_patch.txt")

	// Worktree should be cleaned up
	entries, _ := os.ReadDir(tmpDir)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), "tachi-subagent-"),
			"worktree should be cleaned up, but found: %s", e.Name())
	}
}

func TestWorktreeManager_Create_NoCleanup(t *testing.T) {
	requireWorktreeSupport(t)

	tmpDir := t.TempDir()
	cfg := config.SubagentConfig{
		Worktree:        true,
		WorktreeDir:     tmpDir,
		WorktreeCleanup: boolPtr(false),
	}
	wm := NewWorktreeManager(cfg, debuglog.DefaultLogger)

	var worktreePath string
	result, err := wm.Create(t.Context(), "", func(ctx context.Context, wtPath string) (string, error) {
		worktreePath = wtPath
		return "done", nil
	})

	assert.NoError(t, err)
	assert.Contains(t, result, "done")

	// Worktree should still exist
	assert.DirExists(t, worktreePath, "worktree should not be cleaned up")

	// Clean up manually
	cmd := exec.Command("git", "worktree", "remove", "--force", worktreePath)
	cmd.Run()
}

func TestWorktreeManager_Create_CollectsPatchOnChange(t *testing.T) {
	requireWorktreeSupport(t)

	tmpDir := t.TempDir()
	cfg := config.SubagentConfig{
		Worktree:    true,
		WorktreeDir: tmpDir,
	}
	wm := NewWorktreeManager(cfg, debuglog.DefaultLogger)

	result, err := wm.Create(t.Context(), "", func(ctx context.Context, wtPath string) (string, error) {
		f, err := os.Create(filepath.Join(wtPath, "worktree_modify_test.txt"))
		require.NoError(t, err)
		f.WriteString("changed\n")
		f.Close()

		return "committed change", nil
	})

	assert.NoError(t, err)
	assert.Contains(t, result, "[WORKTREE_PATCH]")
}

func TestWorktreeManager_Create_WithBranch(t *testing.T) {
	requireWorktreeSupport(t)

	// Get current branch
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		t.Skip("cannot determine current branch")
	}
	currentBranch := strings.TrimSpace(string(out))
	if currentBranch == "HEAD" {
		t.Skip("detached HEAD, cannot test branch worktree")
	}

	tmpDir := t.TempDir()
	cfg := config.SubagentConfig{
		Worktree:    true,
		WorktreeDir: tmpDir,
	}
	wm := NewWorktreeManager(cfg, debuglog.DefaultLogger)

	result, err := wm.Create(t.Context(), currentBranch, func(ctx context.Context, wtPath string) (string, error) {
		// Verify we're on the specified branch
		cmd := exec.Command("git", "-C", wtPath, "rev-parse", "--abbrev-ref", "HEAD")
		out, err := cmd.Output()
		require.NoError(t, err)
		branchOn := strings.TrimSpace(string(out))
		assert.Equal(t, currentBranch, branchOn)
		return "branch verified", nil
	})

	assert.NoError(t, err)
	assert.Contains(t, result, "branch verified")
}

func TestWorktreeManager_Create_NonexistentBranch(t *testing.T) {
	requireWorktreeSupport(t)

	tmpDir := t.TempDir()
	cfg := config.SubagentConfig{
		Worktree:    true,
		WorktreeDir: tmpDir,
	}
	wm := NewWorktreeManager(cfg, debuglog.DefaultLogger)

	_, err := wm.Create(t.Context(), "nonexistent-branch-xyzzy", func(ctx context.Context, wtPath string) (string, error) {
		return "", nil
	})

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "branch")
}

func TestWorktreeManager_Create_RemoteTrackingRef(t *testing.T) {
	requireWorktreeSupport(t)

	tmpDir := t.TempDir()
	cfg := config.SubagentConfig{
		Worktree:    true,
		WorktreeDir: tmpDir,
	}
	wm := NewWorktreeManager(cfg, debuglog.DefaultLogger)

	_, err := wm.Create(t.Context(), "origin/main", func(ctx context.Context, wtPath string) (string, error) {
		return "", nil
	})

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "remote tracking")
}

func TestWorktreeManager_Create_EmptyResultPatch(t *testing.T) {
	requireWorktreeSupport(t)

	tmpDir := t.TempDir()
	cfg := config.SubagentConfig{
		Worktree:    true,
		WorktreeDir: tmpDir,
	}
	wm := NewWorktreeManager(cfg, debuglog.DefaultLogger)

	// Don't make any changes → no patch
	result, err := wm.Create(t.Context(), "", func(ctx context.Context, wtPath string) (string, error) {
		return "", nil
	})

	assert.NoError(t, err)
	assert.NotContains(t, result, "[WORKTREE_PATCH]", "no patch when no changes made")
}

func TestWorktreeManager_Create_FnErrorStillReturnsPatch(t *testing.T) {
	requireWorktreeSupport(t)

	tmpDir := t.TempDir()
	cfg := config.SubagentConfig{
		Worktree:    true,
		WorktreeDir: tmpDir,
	}
	wm := NewWorktreeManager(cfg, debuglog.DefaultLogger)

	result, err := wm.Create(t.Context(), "", func(ctx context.Context, wtPath string) (string, error) {
		// Create a file then return error
		f, ferr := os.Create(filepath.Join(wtPath, "before_error.txt"))
		if ferr == nil {
			f.WriteString("partial work\n")
			f.Close()
		}
		return "", assert.AnError
	})

	// Error is propagated
	assert.Error(t, err)
	// Patch is still collected even on error
	if result != "" {
		assert.Contains(t, result, "[WORKTREE_PATCH]")
	}
}

func TestWorktreeManager_createWorktree_remoteTrackingRef(t *testing.T) {
	wm := &WorktreeManager{}
	_, err := wm.createWorktree(t.Context(), "origin/somebranch")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "remote tracking")
}

func TestWorktreeManager_createWorktree_emptyBranch(t *testing.T) {
	requireWorktreeSupport(t)

	wm := &WorktreeManager{worktreeDir: t.TempDir(), logger: debuglog.DefaultLogger}

	path, err := wm.createWorktree(t.Context(), "")
	require.NoError(t, err)
	assert.DirExists(t, path)

	// Verify it's a detached HEAD worktree
	cmd := exec.Command("git", "-C", path, "rev-parse", "--abbrev-ref", "HEAD")
	out, _ := cmd.Output()
	assert.Equal(t, "HEAD\n", string(out), "should be detached HEAD")

	// Clean up
	exec.Command("git", "worktree", "remove", "--force", path).Run()
}

func TestWorktreeManager_removeWorktree(t *testing.T) {
	requireWorktreeSupport(t)

	wm := &WorktreeManager{worktreeDir: t.TempDir(), logger: debuglog.DefaultLogger}

	path, err := wm.createWorktree(t.Context(), "")
	require.NoError(t, err)
	assert.DirExists(t, path)

	wm.removeWorktree(path)
	assert.NoDirExists(t, path)
}

func TestWorktreeManager_removeWorktree_nonexistent(t *testing.T) {
	wm := &WorktreeManager{logger: debuglog.DefaultLogger}
	// Should not panic on nonexistent path
	wm.removeWorktree("/tmp/tachi-nonexistent-worktree-xyz")
}

func TestWorktreeManager_Create_FallbackToSharedDir_NoGitWorktree(t *testing.T) {
	if !isGitRepo() {
		t.Skip("not a git repository")
	}

	tmpDir := t.TempDir()
	// Use a file as WorktreeDir so that git worktree add always fails
	// (it can't create a directory inside a regular file).
	notADir := filepath.Join(tmpDir, "not-a-dir")
	require.NoError(t, os.WriteFile(notADir, []byte("block"), 0644))

	cfg := config.SubagentConfig{
		Worktree:    true,
		WorktreeDir: notADir,
	}
	wm := NewWorktreeManager(cfg, debuglog.DefaultLogger)

	// createWorktree will fail, fallback gives empty path to fn
	var wtPath string
	result, err := wm.Create(t.Context(), "", func(ctx context.Context, path string) (string, error) {
		wtPath = path
		return "fallback result", nil
	})

	assert.NoError(t, err)
	assert.Equal(t, "", wtPath, "fallback should pass empty path")
	assert.Contains(t, result, "fallback result")
	assert.Contains(t, result, "[WARNING: worktree unavailable")
}

func TestCollectPatch_EmptyPath(t *testing.T) {
	wm := &WorktreeManager{}
	patch := wm.collectPatch("")
	assert.Empty(t, patch)
}

func TestIsGitRepo(t *testing.T) {
	// This is a git repo, so it should return true
	assert.True(t, isGitRepo())
}

func TestBranchExists_Existing(t *testing.T) {
	if !isGitRepo() {
		t.Skip("not a git repository")
	}
	// main should exist
	assert.True(t, branchExists("main"))
}

func TestBranchExists_Nonexistent(t *testing.T) {
	assert.False(t, branchExists("nonexistent-branch-xyzzy-12345"))
}