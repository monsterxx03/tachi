package shutil

import (
	"context"
	"fmt"
	"strings"
)

// GitBranch reports what the repository containing dir has checked out: the branch name, or — when
// HEAD is detached — the short commit hash, with detached saying which of the two it is. ok is false
// when dir is not inside a git work tree, or git is not installed, and callers then say nothing
// rather than "unknown".
//
// It is one implementation for the two callers that ask: the desktop's workspace panel shows it
// beside each root, and the system reminder tells the model where its working directory stands. Two
// probes would be two answers to "where am I" — the branch a reader sees in the UI and the branch the
// model was told about must be the same string.
func GitBranch(ctx context.Context, dir string) (name string, detached bool, ok bool) {
	if dir == "" || !Success(ctx, dir, "git", "rev-parse", "--is-inside-work-tree") {
		return "", false, false
	}
	// On a branch, `--abbrev-ref HEAD` answers with its name; detached, it answers "HEAD".
	if branch, err := Output(ctx, dir, "git", "rev-parse", "--abbrev-ref", "HEAD"); err == nil &&
		branch != "" && branch != "HEAD" {
		return branch, false, true
	}
	if commit, err := Output(ctx, dir, "git", "rev-parse", "--short", "HEAD"); err == nil && commit != "" {
		return commit, true, true
	}
	return "", false, false
}

// GitRepoRoot returns the absolute path of the work tree that contains dir. ok is false when dir
// is not inside one, or git is not installed.
func GitRepoRoot(ctx context.Context, dir string) (string, bool) {
	if dir == "" {
		return "", false
	}
	root, err := Output(ctx, dir, "git", "rev-parse", "--show-toplevel")
	if err != nil || root == "" {
		return "", false
	}
	return root, true
}

// GitWorktree is one linked worktree, as `git worktree list --porcelain` reports it.
type GitWorktree struct {
	Path   string
	Branch string // short branch name; "" when Detached
	// Detached marks a worktree sitting on a commit rather than a branch. Such a worktree holds no
	// branch, so it can never be the reason a branch is "already checked out".
	Detached bool
	// Bare is the repository's own directory (a bare repo has one entry and no work tree).
	Bare bool
}

// GitWorktrees lists the linked worktrees of the repository containing dir, the main checkout
// included. ok is false when dir is not in a work tree or git is not installed.
//
// It is the answer to "which branches are already checked out somewhere": git refuses to check the
// same branch out twice, so the UI needs the occupied set before it lets a user pick one.
func GitWorktrees(ctx context.Context, dir string) ([]GitWorktree, bool) {
	if _, ok := GitRepoRoot(ctx, dir); !ok {
		return nil, false
	}
	out, err := Output(ctx, dir, "git", "worktree", "list", "--porcelain")
	if err != nil {
		return nil, false
	}
	var trees []GitWorktree
	var cur GitWorktree
	flush := func() {
		if cur.Path != "" {
			trees = append(trees, cur)
		}
		cur = GitWorktree{}
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "worktree "):
			flush()
			cur.Path = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "branch "):
			// refs/heads/<name> — anything else (a detached HEAD) has no branch line.
			cur.Branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
		case line == "detached":
			cur.Detached = true
		case line == "bare":
			cur.Bare = true
		}
	}
	flush()
	return trees, true
}

// GitLocalBranches lists the repository's local branches, one per line, as git prints them.
func GitLocalBranches(ctx context.Context, dir string) ([]string, bool) {
	if _, ok := GitRepoRoot(ctx, dir); !ok {
		return nil, false
	}
	out, err := Output(ctx, dir, "git", "for-each-ref", "--format=%(refname:short)", "refs/heads")
	if err != nil {
		return nil, false
	}
	var branches []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			branches = append(branches, line)
		}
	}
	return branches, true
}

// GitValidBranchName reports whether name is a legal branch name, asking git rather than
// re-implementing check-ref-format's rules.
func GitValidBranchName(ctx context.Context, dir, name string) bool {
	if strings.TrimSpace(name) == "" {
		return false
	}
	return Success(ctx, dir, "git", "check-ref-format", "--branch", name)
}

// GitWorktreeAdd creates a linked worktree of repo at path, based on base (a branch name, or "HEAD"
// for the current commit). With createBranch, a NEW branch named branch is created from base and
// checked out there; without it, branch is the existing branch to check out and base is ignored.
//
// git itself refuses the two cases that make this unsafe — a branch checked out elsewhere, a path
// that already exists — and its stderr is what the user is shown, so nothing is re-implemented here.
func GitWorktreeAdd(ctx context.Context, repo, path, branch, base string, createBranch bool) error {
	if createBranch {
		if base == "" {
			base = "HEAD"
		}
		_, err := Output(ctx, repo, "git", "worktree", "add", "-b", branch, path, base)
		return err
	}
	_, err := Output(ctx, repo, "git", "worktree", "add", path, branch)
	return err
}

// GitWorktreeRemove removes a linked worktree's directory. It deliberately does NOT pass --force:
// git refuses a worktree with modified or untracked files, which is exactly the guard that must
// hold before a delete throws someone's work away.
func GitWorktreeRemove(ctx context.Context, repo, path string) error {
	_, err := Output(ctx, repo, "git", "worktree", "remove", path)
	return err
}

// GitWorktreePrune drops the administrative record of worktrees whose directories are gone.
func GitWorktreePrune(ctx context.Context, repo string) error {
	_, err := Output(ctx, repo, "git", "worktree", "prune")
	return err
}

// GitDirtyCount reports how many paths in dir's work tree have uncommitted changes — modified,
// staged, untracked or deleted — ignoring whatever .gitignore covers. ok is false when the command
// could not be run. It is what the UI asks before offering to delete a worktree directory: a count
// is the least a reader needs to know what "delete" would actually throw away.
func GitDirtyCount(ctx context.Context, dir string) (int, bool) {
	out, err := Output(ctx, dir, "git", "status", "--porcelain")
	if err != nil {
		return 0, false
	}
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n, true
}

// GitShortHEAD is the abbreviated commit HEAD points at, for naming a worktree created from a
// detached HEAD (there is no branch name to offer in that case).
func GitShortHEAD(ctx context.Context, dir string) (string, error) {
	out, err := Output(ctx, dir, "git", "rev-parse", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-parse: %w", err)
	}
	return out, nil
}
