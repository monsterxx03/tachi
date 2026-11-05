package main

// Git status for the file tree's rows: which entries are uncommitted, and how.
//
// It is a SEPARATE reader from ListWorkspaceDir (filetree.go) on purpose, and the reason is the
// one thing the tree cannot see for itself. A directory's mtime moves when entries come and go, so
// the poll re-lists it and the rows stay true — but an Edit tool rewriting a file IN PLACE moves
// nothing the poll watches, so a row listed before the edit would keep saying "clean" until the
// reader clicked ↻. Status therefore travels on its own clock: the frontend asks for it every tick
// and merges it onto rows by absolute path. Nothing is stored on the row, so the two answers can
// never disagree about the same file.
//
// It is also not the diff panel's question (gitdiff.go): that panel wants hunks, one file at a
// time and only when a pane is opened. This one wants one letter per path, often, for a column that
// may be showing a hundred rows.
//
// Design: docs/2026-10-13-desktop-file-tree-design.md §4.

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/monsterxx03/tachi/pkg/fileutil"
)

// gitStatusMaxPaths caps what one answer carries. The tree only marks the rows it draws (hundreds,
// at most, for a deep expansion); past this the column simply has no marks, which is what a
// truncation is allowed to cost here — unlike a listing, no row is hidden by it.
const gitStatusMaxPaths = 5000

// GitStatusVO is one path as git sees it right now.
type GitStatusVO struct {
	// Path is absolute, resolved against the session's own root, so the frontend can merge it onto
	// a row without knowing anything about repositories or where they start.
	Path string `json:"path"`
	// Code is the one letter the row shows: M A D R C U ! — see gitStatusCode.
	Code string `json:"code"`
	// Label says what that letter means in the reader's words. It travels with the letter because
	// the letter→word table is this side's answer; a second copy in the frontend is free to drift.
	Label string `json:"label"`
}

// conflictCode is the display code for the seven porcelain pairs that mean "unresolved in a merge".
// They are spelled out because there is no pattern to them: each pair names which SIDES disagree.
var conflictPairs = map[string]bool{
	"DD": true, "AU": true, "UD": true, "UA": true, "DU": true, "AA": true, "UU": true,
}

// gitStatusCode turns porcelain's two-letter XY pair into the one letter a row shows, with the
// words its tooltip uses. ok is false for a pair this column has nothing to say about.
//
// A file that is staged AND edited again ("MM") reads as M: the row answers "is this file different
// from HEAD", and how many times it was staged is what the diff panel is for.
func gitStatusCode(xy string) (code, label string, ok bool) {
	switch {
	case xy == "??":
		return "U", "未跟踪", true
	case conflictPairs[xy]:
		return "!", "有冲突", true
	case strings.ContainsRune(xy, 'R'):
		return "R", "重命名", true
	case strings.ContainsRune(xy, 'C'):
		return "C", "复制", true
	case strings.ContainsRune(xy, 'D'):
		return "D", "已删除", true
	case strings.ContainsRune(xy, 'A'):
		return "A", "新增", true
	case strings.ContainsAny(xy, "MT"):
		return "M", "已修改", true
	}
	return "", "", false
}

// GetWorkspaceGitStatus answers what is uncommitted under the session's roots.
//
// Every root is asked, and a path that two NESTED roots share keeps the first root's answer — the
// same precedence rootContaining gives it everywhere else in this app. A root that is not a
// repository (or has no git at all) contributes nothing: an empty answer is not an error here,
// because the tree draws the same rows either way.
func (s *AgentService) GetWorkspaceGitStatus(sessionID string) []GitStatusVO {
	primary, additional := s.desk.sessionRoots(sessionID)
	if primary == "" {
		return nil
	}
	roots := append([]string{primary}, additional...)
	out := make([]GitStatusVO, 0, 16)
	seen := make(map[string]bool)
	for _, root := range roots {
		if !fileutil.IsDir(root) {
			continue // a root whose directory is gone has nothing to report
		}
		for _, st := range workingTreeStatus(root) {
			if seen[st.Path] {
				continue
			}
			seen[st.Path] = true
			out = append(out, st)
			if len(out) == gitStatusMaxPaths {
				return sortGitStatuses(out)
			}
		}
	}
	return sortGitStatuses(out)
}

// workingTreeStatus is ONE root's share: `git status --porcelain` restricted to paths inside it.
func workingTreeStatus(root string) []GitStatusVO {
	top, err := gitOutput(root, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil
	}
	top = strings.TrimSpace(top)
	if top == "" {
		return nil
	}
	// git prints paths RELATIVE TO THE REPOSITORY ROOT, whatever directory it was asked from — so
	// the prefix this root sits at has to be subtracted. It is computed on resolved paths because
	// git answers with a resolved toplevel (on macOS /tmp is /private/tmp), while the root comes
	// from the session and may well still carry the symlink.
	realRoot := root
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		realRoot = resolved
	}
	rel, err := filepath.Rel(top, realRoot)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil
	}
	prefix := ""
	if rel != "." {
		prefix = filepath.ToSlash(rel) + "/"
	}

	// --untracked-files=all is what makes a file inside a NEW directory its own entry: the default
	// collapses the directory to one line, and the tree has no row for a directory's status.
	out, err := gitOutput(root, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil
	}
	return statusesFromPorcelain(out, root, prefix)
}

// statusesFromPorcelain parses `git status --porcelain=v1 -z` output into rows' statuses.
//
// The format is "XY <path>\0" per entry, with one twist: a rename or copy carries its ORIGINAL path
// in a second, separate field, so that entry consumes two. Skipping it would make the old path read
// as a status line of its own — and git renames are exactly what a reader is looking for here.
func statusesFromPorcelain(out, root, prefix string) []GitStatusVO {
	fields := strings.Split(out, "\x00")
	statuses := make([]GitStatusVO, 0, len(fields))
	for i := 0; i < len(fields); i++ {
		entry := fields[i]
		// "XY" + a space + at least one character of path.
		if len(entry) < 4 || entry[2] != ' ' {
			continue
		}
		xy, rel := entry[:2], entry[3:]
		if xy[0] == 'R' || xy[0] == 'C' {
			i++
		}
		code, label, ok := gitStatusCode(xy)
		if !ok {
			continue
		}
		if prefix != "" {
			if !strings.HasPrefix(rel, prefix) {
				continue // uncommitted, but under another root (or outside every one)
			}
			rel = strings.TrimPrefix(rel, prefix)
		}
		if rel == "" {
			continue
		}
		statuses = append(statuses, GitStatusVO{
			Path:  filepath.Join(root, filepath.FromSlash(rel)),
			Code:  code,
			Label: label,
		})
	}
	return statuses
}

// sortGitStatuses makes the answer deterministic (two roots may report one repository): the
// frontend builds a map out of it, but an order that changes between calls is untestable.
func sortGitStatuses(in []GitStatusVO) []GitStatusVO {
	sort.SliceStable(in, func(i, j int) bool { return in[i].Path < in[j].Path })
	return in
}
