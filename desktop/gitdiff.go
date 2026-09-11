package main

// Working-tree diff for the changes panel (P2): what the files a turn touched look
// like NOW, against HEAD, with REAL line numbers.
//
// P1 answers "what did this call set out to change" from the call's own arguments.
// That is enough for a card but carries no file coordinates. This is the other half —
// the authoritative view of the working tree — and it is also the only place review
// findings can be attached, because findings name real lines.
//
// Boundaries are stated rather than papered over: a session outside a git repository
// gets a sentence instead of a diagram, and paths that do not belong to the
// repository are left out with a note (they can only have fragment diffs).
//
// Design: docs/2026-09-11-desktop-diff-review-design.md §12.2

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/linediff"
)

const (
	// gitTimeout bounds one git invocation. A diff on a cold repository is slow; the
	// UI must not hang on it.
	gitTimeout = 10 * time.Second
	// maxDiffHunks caps what travels to the webview for one panel open.
	maxDiffHunks = 2000
)

// TurnDiffVO is the changes panel's payload.
type TurnDiffVO struct {
	// Files are the requested paths that differ from HEAD, tracked ones in git's
	// order, then untracked ones — stable between opens.
	Files []FileDiffVO `json:"files"`
	// Root is the workspace the diff was taken in ("" when the session has none).
	Root string `json:"root,omitempty"`
	// Note says in the user's words why the list is empty or incomplete: no
	// workspace, not a git repository, paths outside it, output truncated.
	Note string `json:"note,omitempty"`
}

// FileDiffVO is one file's working-tree change.
type FileDiffVO struct {
	Path    string          `json:"path"`
	OldPath string          `json:"oldPath,omitempty"`
	Created bool            `json:"created,omitempty"`
	Deleted bool            `json:"deleted,omitempty"`
	Binary  bool            `json:"binary,omitempty"`
	Hunks   []linediff.Hunk `json:"hunks"`
	Added   int             `json:"added"`
	Removed int             `json:"removed"`
}

// GetTurnDiff returns the working-tree diff of paths (the files a turn touched) under
// the session's workspace. An empty paths list means the whole tree.
func (s *AgentService) GetTurnDiff(sessionID string, paths []string) TurnDiffVO {
	root, _ := s.desk.sessionRoots(sessionID)
	if root == "" {
		return TurnDiffVO{Note: "尚未选择工作目录，只能看到片段 diff"}
	}
	vo := TurnDiffVO{Root: root}

	if out, err := gitOutput(root, "rev-parse", "--is-inside-work-tree"); err != nil || strings.TrimSpace(out) != "true" {
		vo.Note = "工作目录不在 git 仓库内，只能看到片段 diff"
		return vo
	}

	inside, outside := splitByRepo(root, paths)
	if len(outside) > 0 {
		vo.Note = fmt.Sprintf("%d 个文件不在该 git 仓库内，只能看到片段 diff", len(outside))
	}

	// Tracked changes. A repository without commits has no HEAD to compare against;
	// everything in it is then "new" from the panel's point of view, which the
	// untracked branch below covers — so the failure is not an error here.
	var tracked string
	if _, err := gitOutput(root, "rev-parse", "--verify", "HEAD"); err == nil {
		tracked, _ = gitOutput(root, gitArgs([]string{"diff", "HEAD", "--no-color", "-U3", "--"}, inside)...)
	} else {
		staged, _ := gitOutput(root, gitArgs([]string{"diff", "--cached", "--no-color", "-U3", "--"}, inside)...)
		unstaged, _ := gitOutput(root, gitArgs([]string{"diff", "--no-color", "-U3", "--"}, inside)...)
		tracked = staged + unstaged
	}
	for _, fd := range linediff.ParseUnified(tracked) {
		vo.Files = append(vo.Files, FileDiffVO{
			Path: fd.Path, OldPath: fd.OldPath,
			Created: fd.Created, Deleted: fd.Deleted, Binary: fd.Binary,
			Hunks: fd.Hunks, Added: fd.Added, Removed: fd.Removed,
		})
	}

	// Untracked files never appear in git diff: they are new content from end to end.
	// The hunks are synthesized here (git would only say "untracked"), so the panel can
	// show them without a second round trip.
	if listing, err := gitOutput(root, gitArgs([]string{"ls-files", "--others", "--exclude-standard", "--"}, inside)...); err == nil {
		for _, rel := range strings.Split(strings.TrimSpace(listing), "\n") {
			if rel = strings.TrimSpace(rel); rel == "" {
				continue
			}
			vo.Files = append(vo.Files, untrackedFileDiff(root, rel))
		}
	}

	// Truncate rather than stall: the panel says so and the file itself is one click away.
	if total, kept := countHunks(vo.Files), 0; total > maxDiffHunks {
		trimmed := vo.Files[:0]
		for _, f := range vo.Files {
			if kept >= maxDiffHunks {
				break
			}
			if kept+len(f.Hunks) > maxDiffHunks {
				f.Hunks = f.Hunks[:maxDiffHunks-kept]
				f.Added, f.Removed = linediff.Counts(f.Hunks)
			}
			kept += len(f.Hunks)
			trimmed = append(trimmed, f)
		}
		vo.Files = trimmed
		vo.Note = joinNotes(vo.Note, "改动很大，这里只显示了一部分")
	}
	return vo
}

// gitArgs joins a command prefix with a pathspec list. Spelled out because a slice
// spread cannot be mixed with other variadic arguments in one call.
func gitArgs(prefix, paths []string) []string {
	return append(append(make([]string, 0, len(prefix)+len(paths)), prefix...), paths...)
}

// splitByRepo keeps the paths that live inside root (what git can diff) and reports
// the rest, which can only have fragment diffs.
func splitByRepo(root string, paths []string) (inside, outside []string) {
	prefix := strings.TrimSuffix(root, string(filepath.Separator)) + string(filepath.Separator)
	seen := make(map[string]bool, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		p = filepath.Clean(p)
		if !strings.HasPrefix(p+string(filepath.Separator), prefix) {
			outside = append(outside, p)
			continue
		}
		if !seen[p] {
			seen[p] = true
			inside = append(inside, p)
		}
	}
	return inside, outside
}

// untrackedFileDiff renders one untracked file as an all-add change with real line
// numbers (it is new content, so every line is an addition).
func untrackedFileDiff(root, rel string) FileDiffVO {
	vo := FileDiffVO{Path: rel, Created: true}
	abs := filepath.Join(root, filepath.FromSlash(rel))

	if looksText, err := fileutil.LooksLikeText(abs); err != nil || !looksText {
		vo.Binary = true
		return vo
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		vo.Binary = true
		return vo
	}
	text := string(data)
	if len(text) > maxPreviewBytes {
		text = text[:maxPreviewBytes]
	}
	for i, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		vo.Hunks = append(vo.Hunks, linediff.Hunk{Kind: linediff.KindAdd, NewLine: i + 1, Text: line})
	}
	vo.Added, vo.Removed = linediff.Counts(vo.Hunks)
	return vo
}

func countHunks(files []FileDiffVO) int {
	total := 0
	for _, f := range files {
		total += len(f.Hunks)
	}
	return total
}

func joinNotes(a, b string) string {
	if a == "" {
		return b
	}
	return a + "；" + b
}

// gitOutput runs one git command in root and returns its stdout. The error carries
// git's own stderr, which is what makes "not a repository" debuggable from a log.
func gitOutput(root string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}
