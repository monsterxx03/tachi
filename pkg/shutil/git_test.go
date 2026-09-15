package shutil

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRepo makes a real repository with one commit and returns its path: the branch a checkout
// reports is a fact about the repository, so the probe is tested against one rather than a stub.
func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Skipf("git %s: %v (%s)", strings.Join(args, " "), err, out)
		}
	}
	return dir
}

// TestGitBranch pins the three answers the two callers depend on: a branch name, the short commit
// when HEAD is detached (with the flag that says so — a bare hash presented as a branch is a lie the
// workspace panel would tell), and "not a repository" as ok=false rather than an empty branch.
func TestGitBranch(t *testing.T) {
	ctx := context.Background()

	if name, _, ok := GitBranch(ctx, t.TempDir()); ok {
		t.Errorf("a plain directory reported a branch: %q", name)
	}
	if name, _, ok := GitBranch(ctx, ""); ok {
		t.Errorf("an empty directory reported a branch: %q", name)
	}

	repo := gitRepo(t)
	name, detached, ok := GitBranch(ctx, repo)
	if !ok {
		t.Skip("git repo unusable in this environment")
	}
	if detached {
		t.Errorf("a fresh repository reported a detached HEAD (%q)", name)
	}
	want, err := exec.Command("git", "-C", repo, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if name != strings.TrimSpace(string(want)) {
		t.Errorf("branch = %q, want %q", name, strings.TrimSpace(string(want)))
	}

	// Detached: the short commit, flagged.
	sha, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	head := strings.TrimSpace(string(sha))
	if out, err := exec.Command("git", "-C", repo, "checkout", "-q", head).CombinedOutput(); err != nil {
		t.Fatalf("detach HEAD: %v (%s)", err, out)
	}
	name, detached, ok = GitBranch(ctx, repo)
	if !ok || !detached {
		t.Fatalf("detached HEAD reported ok=%v detached=%v (%q)", ok, detached, name)
	}
	if !strings.HasPrefix(head, name) {
		t.Errorf("detached HEAD reported %q, want a prefix of %s", name, head)
	}

	// A subdirectory of the repository is inside the same work tree.
	sub := filepath.Join(repo, "sub")
	if err := exec.Command("mkdir", "-p", sub).Run(); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, _, ok := GitBranch(ctx, sub); !ok {
		t.Error("a directory inside the work tree reported no branch")
	}
}
