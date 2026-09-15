package shutil

import "context"

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
