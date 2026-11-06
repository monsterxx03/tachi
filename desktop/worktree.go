package main

// Git worktrees for a single conversation: the session moves into its own checkout so its edits
// stop sharing a directory with every other session that started in the same project.
//
// Two facts make this safe rather than destructive:
//
//   - It only MOVES the session. The old directory, its files and its branch are untouched, so
//     「搬回原目录」 is a setting change, not a merge.
//   - The session records that the directory is ours (Session.WorktreeOf/WorktreeBranch), which
//     is the only thing that later lets DeleteSession offer to remove it. A worktree the user made
//     by hand carries no such record and is never touched.
//
// git's own rules are the UI's rules: a branch can be checked out in only one worktree at a time,
// and `git worktree add` fails loudly on a path that exists — both are reported verbatim rather
// than re-implemented here.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/shutil"
	"github.com/monsterxx03/tachi/session"
)

// WorktreeBranchesVO answers the create-worktree form: which repo the session is in, where it
// stands, and which branches are free to check out.
type WorktreeBranchesVO struct {
	// Repo is the work tree's top level — the repository the new worktree is cut from.
	Repo string `json:"repo"`
	// Current / Detached are the primary directory's branch, as the panel shows it.
	Current  string `json:"current,omitempty"`
	Detached bool   `json:"detached,omitempty"`
	// Branches is every local branch, current one included (the form offers it as the base).
	Branches []string `json:"branches"`
	// CheckedOut maps a branch to the worktree that already has it checked out. A branch in here
	// cannot be checked out again — git refuses — so the form greys it out and names the holder.
	CheckedOut map[string]string `json:"checkedOut,omitempty"`
	// SuggestedBranch / SuggestedDir prefill the form: a fresh branch under tachi/ and a directory
	// under Tachi's state dir, where a checkout nobody asked to keep is obviously ours to remove.
	SuggestedBranch string `json:"suggestedBranch,omitempty"`
	SuggestedDir    string `json:"suggestedDir,omitempty"`
	// Err explains why no form can be shown ("" when it can): not a git repo, already a worktree.
	Err string `json:"err,omitempty"`
}

// GetWorktreeBranches returns what the create-worktree form needs for one session. It never fails:
// a session that cannot have a worktree comes back with Err set, which is what the UI shows.
func (s *AgentService) GetWorktreeBranches(id string) WorktreeBranchesVO {
	var vo WorktreeBranchesVO
	sess := s.desk.sessionRecord(id)
	if sess == nil {
		vo.Err = "会话不存在"
		return vo
	}
	if sess.WorktreeOf != "" {
		vo.Err = "这个会话已经在 worktree 里了"
		return vo
	}
	primary, _ := s.desk.sessionRootsFrom(sess)
	if primary == "" {
		vo.Err = "请先设置主目录，才能建 worktree"
		return vo
	}
	ctx := context.Background()
	repo, ok := shutil.GitRepoRoot(ctx, primary)
	if !ok {
		vo.Err = "主目录不在 git 仓库里"
		return vo
	}
	vo.Repo = repo
	vo.CheckedOut = map[string]string{}
	vo.Current, vo.Detached, _ = shutil.GitBranch(ctx, primary)
	vo.Branches, _ = shutil.GitLocalBranches(ctx, repo)
	// The occupied set comes from the worktree LIST rather than from the branches: a branch that
	// exists but is checked out nowhere is free, and that is the case this map must not mark.
	if trees, ok := shutil.GitWorktrees(ctx, repo); ok {
		for _, t := range trees {
			if t.Branch != "" && !t.Detached {
				vo.CheckedOut[t.Branch] = t.Path
			}
		}
	}
	vo.SuggestedBranch = suggestedWorktreeBranch(sess)
	vo.SuggestedDir = suggestedWorktreeDir(repo, vo.SuggestedBranch)
	return vo
}

// suggestedWorktreeBranch is 「tachi/<session tail>」: short, obviously a machine's own branch, and
// unique per conversation. The user can type over it.
func suggestedWorktreeBranch(sess *session.Session) string {
	id := sess.ID
	if len(id) > 6 {
		id = id[len(id)-6:]
	}
	if id == "" {
		id = "session"
	}
	return "tachi/" + id
}

// suggestedWorktreeDir parks a worktree under Tachi's state dir, next to the session records and
// checkpoint repos: it is a checkout nobody asked to keep, so keeping it somewhere the user does
// not work is the default. The form lets them pick another directory.
func suggestedWorktreeDir(repo, branch string) string {
	base := config.BaseDir()
	if base == "" {
		return ""
	}
	name := strings.NewReplacer("/", "-", "\\", "-").Replace(branch)
	return filepath.Join(base, "worktrees", filepath.Base(filepath.Clean(repo)), name)
}

// CreateSessionWorktree cuts a worktree off the session's primary directory and moves the session
// into it. newBranch says whether to create a branch: non-empty means 「新分支 newBranch，基于
// base」, empty means 「签出已有分支 base」.
//
// Returns "ok" or the reason to show; nothing is written and nothing is created on a refusal.
func (s *AgentService) CreateSessionWorktree(id, base, newBranch, dir string) string {
	d := s.desk
	sess := d.sessionRecord(id)
	if sess == nil {
		return "会话不存在"
	}
	if sess.WorktreeOf != "" {
		return "这个会话已经在 worktree 里了"
	}
	primary, _ := d.sessionRootsFrom(sess)
	if primary == "" {
		return "请先设置主目录，才能建 worktree"
	}
	ctx := context.Background()
	repo, ok := shutil.GitRepoRoot(ctx, primary)
	if !ok {
		return "主目录不在 git 仓库里"
	}

	branch := strings.TrimSpace(newBranch)
	base = strings.TrimSpace(base)
	create := branch != ""
	if create {
		if !shutil.GitValidBranchName(ctx, repo, branch) {
			return fmt.Sprintf("分支名 %s 不合法", branch)
		}
		if branches, ok := shutil.GitLocalBranches(ctx, repo); ok {
			for _, b := range branches {
				if b == branch {
					return fmt.Sprintf("分支 %s 已经存在，换个名字或者直接签出它", branch)
				}
			}
		}
		if base == "" {
			base = "HEAD"
		}
	} else {
		if branch == "" && base == "" {
			return "请选择基于哪个分支"
		}
		branch = base
		if branch == "" {
			return "请选择要签出的分支"
		}
		// git refuses a double checkout with its own message; saying it up front names the holder.
		if trees, ok := shutil.GitWorktrees(ctx, repo); ok {
			for _, t := range trees {
				if !t.Detached && t.Branch == branch {
					return fmt.Sprintf("分支 %s 已经被 %s 签出", branch, t.Path)
				}
			}
		}
		base = ""
	}

	target, err := expandRootPath(dir)
	if err != nil {
		return err.Error()
	}
	if reason := wideRootReason(target); reason != "" {
		return reason
	}
	if fileutil.Exists(target) {
		return fmt.Sprintf("%s 已经存在", target)
	}
	// git creates the last path component, not the parents.
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Sprintf("无法创建上级目录：%v", err)
	}
	if err := shutil.GitWorktreeAdd(ctx, repo, target, branch, base, create); err != nil {
		return fmt.Sprintf("创建 worktree 失败：%v", err)
	}

	if err := d.updateSessionMeta(id, func(cur *session.Session) {
		cur.WorkingDir = target
		cur.WorktreeOf = repo
		cur.WorktreeBranch = branch
	}); err != nil {
		// The checkout exists; the session just did not move. Say so rather than pretending, and
		// leave the directory alone — it may hold nothing yet, but removing it is the user's call.
		return fmt.Sprintf("worktree 已创建在 %s，但会话没能切过去：%v", target, err)
	}
	// Everything derived from the old tree is now wrong: skills, the agent's permission trees and
	// the prompt are rebuilt at the next turn start, exactly as they are after a directory change.
	d.markTreeStale(id)
	d.notifyMCPRootsChanged()
	d.emitWorkspaceChanged("", id)
	return "ok"
}

// worktreeDeleteRefusal is the pre-flight half of 「删除会话时连 worktree 一起删」: "" means the
// removal can go ahead, anything else is the reason to show instead. It touches nothing, so a
// refusal leaves the session (and the directory) exactly as they were.
func (d *desktopApp) worktreeDeleteRefusal(sess *session.Session) string {
	if sess == nil || sess.WorktreeOf == "" || sess.WorkingDir == "" {
		return ""
	}
	ctx := context.Background()
	if !fileutil.IsDir(sess.WorkingDir) {
		// Already gone: the delete only needs git's record pruned, which cannot fail in a way
		// worth refusing for.
		return ""
	}
	if n, ok := shutil.GitDirtyCount(ctx, sess.WorkingDir); ok && n > 0 {
		return fmt.Sprintf("worktree 里有 %d 个未提交的改动，先提交（或取消勾选）再删", n)
	}
	if !fileutil.IsDir(sess.WorktreeOf) {
		return fmt.Sprintf("worktree 所属的仓库 %s 已经不在了，无法用它删除", sess.WorktreeOf)
	}
	return ""
}

// removeSessionWorktree deletes the worktree directory a session recorded, if there is one.
//
// Refusals are deliberate and are the caller's to report: a dirty worktree is not removed (git
// refuses too — untracked files are how an hour of an agent's work looks before anyone commits),
// and a directory that is already gone only needs git's record pruned. It returns "" when there
// was nothing to do.
func (d *desktopApp) removeSessionWorktree(sess *session.Session) string {
	if sess == nil || sess.WorktreeOf == "" || sess.WorkingDir == "" {
		return ""
	}
	ctx := context.Background()
	repo, path := sess.WorktreeOf, sess.WorkingDir
	if !fileutil.IsDir(path) {
		_ = shutil.GitWorktreePrune(ctx, repo)
		return ""
	}
	if n, ok := shutil.GitDirtyCount(ctx, path); ok && n > 0 {
		return fmt.Sprintf("worktree 里有 %d 个未提交的改动，没有删除", n)
	}
	if err := shutil.GitWorktreeRemove(ctx, repo, path); err != nil {
		return fmt.Sprintf("删除 worktree 失败：%v", err)
	}
	_ = shutil.GitWorktreePrune(ctx, repo)
	return ""
}
