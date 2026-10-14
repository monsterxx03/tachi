package main

// The file tree: one directory at a time, from a session's workspace roots.
//
// The tree is a second READER of the same roots everything else reads (sessionRootsFrom): its top
// level nodes ARE the session's roots, and every path it hands the webview is absolute, so the
// preview (/local), 打开/在文件夹中显示 (OpenPath/RevealPath) and the @-reference the reader may
// insert are all reached with exactly what the row carries. It never invents a "current directory"
// of its own — a GUI process's cwd is "/" (docs/agents/desktop.md), and this app hosts several
// sessions in several trees at once.
//
// Listing is LAZY and bounded, which is the whole reason this file exists rather than a
// "give me the tree" call: a real repository is tens of thousands of entries, and walking it to
// draw a column would be exactly the cost the @-file index goes to some trouble to avoid. One
// os.ReadDir per expanded row, capped per directory, with the real count reported separately so
// "另有 N 项未显示" is a true number.
//
// Two boundary rules are stated here rather than papered over:
//
//   - CONTAINMENT IS LITERAL. A requested path must sit inside one of the session's roots
//     (filepath.Clean, then a prefix comparison — roots.go's dirContains). Symlinks are NOT
//     resolved on the way in: a link inside a root pointing outside it is allowed (that is the
//     user's own directory arrangement, and the row is marked with `link` so they can see they are
//     on one), while "../../../etc" and an absolute path under another tree are refused — even when
//     they arrive from a forged webview request. A refusal is a SENTENCE in DirListingVO.Error,
//     never a silently empty directory: silence is the only symptom a resolution bug has.
//   - IGNORED IS NOT HIDDEN. Tree rows are shown even when .gitignore/.ignore/.rgignore hide them
//     (灰显 + the rule that matched), because the tree answers "what is on this disk" — and the
//     thing a reader most often goes looking for in it is a build product or a log, i.e. exactly
//     what is ignored. The @-picker and the checkpoint snapshots stay "ignored means invisible";
//     that is a different question (what may the model see) and deliberately a different answer.
//
// Design: docs/2026-10-13-desktop-file-tree-design.md §3, §6.

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// fileTreeMaxEntries caps how many rows one directory hands the webview. The cap is on what
	// travels, never on what exists: Total reports the directory's real size, so the "另有 N 项"
	// line is a fact rather than the difference between two guesses.
	fileTreeMaxEntries = 2000

	// fileTreePollMaxPaths caps one polling batch. The frontend sends the expanded directories plus
	// the file on screen — dozens, not hundreds — so this is a floor under a bug, not a budget.
	fileTreePollMaxPaths = 512

	// A workspace remembers at most this many expanded directories, and the file keeps at most this
	// many workspaces (oldest written dropped first). Past the first number it is no longer "what I
	// expanded" but "what I never closed"; past the second, desktop_ui.json grows for projects the
	// reader edited once. Both are clamped on WRITE, and the read side filters independently —
	// neither trusts the other, because the file is hand-editable.
	fileTreeMemoMaxDirs       = 500
	fileTreeMemoMaxWorkspaces = 50
)

// FileNodeVO is one row of the tree.
type FileNodeVO struct {
	Name string `json:"name"`
	// Path is absolute: /local, OpenPath, RevealPath and an @-reference all take it as it is.
	Path  string `json:"path"`
	IsDir bool   `json:"isDir"`
	// Link marks the entry itself as a symbolic link. Its name says so, and a link pointing at a
	// directory is still a directory here (IsDir is os.Stat's answer, which follows it) — the flag is
	// what lets the reader see that they are on one.
	Link bool  `json:"link,omitempty"`
	Size int64 `json:"size,omitempty"` // bytes; 0 for directories
	// Kind is the icon/preview HINT, decided by extension alone (treeKindHint). "unknown" does not
	// mean "cannot be previewed": that question is answered by PreviewFile, which reads content.
	Kind string `json:"kind"`
	// Ignored is non-empty when git hides this entry, and holds the rule that does it
	// (".gitignore:12: dist/") — one field answering both "grey it" and "because of what".
	Ignored string `json:"ignored,omitempty"`
	// Error is this entry's own problem (a broken link, a stat failure). The row still appears.
	Error string `json:"error,omitempty"`
}

// DirListingVO is one directory's worth of rows.
type DirListingVO struct {
	Path string `json:"path"`
	// Root is the workspace root this layer belongs to, and Label is how that root is named ("" = the
	// session's primary root, else its base name, or the full path when two roots share one — the
	// same rule and the same strings as rootLabels, so the tree, the file view's path line and the
	// diff panel cannot spell a root three ways).
	Root  string `json:"root"`
	Label string `json:"label,omitempty"`
	// Entries are the rows, directories first then names (case-insensitive), .git skipped.
	Entries []FileNodeVO `json:"entries"`
	// Total is how many entries the directory really holds (before the cap), Truncated says the cap
	// bit. The "另有 N 项未显示" line is Total - len(Entries), never a made-up number.
	Total     int  `json:"total"`
	Truncated bool `json:"truncated,omitempty"`
	// Error is "this whole directory could not be read" (permission, deleted a moment ago). It is
	// mutually exclusive with Entries and must be SHOWN — a silent empty directory is a lie.
	Error string `json:"error,omitempty"`
}

// PathStateVO is one path as it stands right now, for the auto-refresh poll.
type PathStateVO struct {
	Path    string `json:"path"`
	Exists  bool   `json:"exists"`
	IsDir   bool   `json:"isDir,omitempty"`
	Size    int64  `json:"size,omitempty"`
	MTimeMs int64  `json:"mtimeMs,omitempty"`
}

// FileTreeMemoVO is what one workspace remembers: which directories were expanded, and the file the
// reader was looking at. Key is the workspace key the backend derived — the frontend never builds
// one, so "what counts as the same workspace" has a single definition (fileTreeKey).
type FileTreeMemoVO struct {
	Key string `json:"key"`
	// Found says this workspace HAD a memo. It is what tells "nothing was ever remembered here"
	// (start from the defaults, primary expanded) from "the reader remembers it with everything
	// collapsed" — a difference an empty Dirs list cannot express.
	Found bool     `json:"found"`
	Dirs  []string `json:"dirs"`
	File  string   `json:"file,omitempty"`
}

// ListWorkspaceDir lists ONE layer of a directory inside the session's workspace.
//
// path must be inside one of the session's roots (§3.2); the root itself is how its top level is
// listed. Everything it can go wrong with comes back as a sentence in Error rather than as an
// empty listing, because "nothing here" and "I would not look" are different facts and the reader
// is the one who has to act on them.
func (s *AgentService) ListWorkspaceDir(sessionID, path string) DirListingVO {
	primary, additional := s.desk.sessionRoots(sessionID)
	if primary == "" {
		return DirListingVO{Path: path, Error: "这个会话还没有工作目录"}
	}
	dir := filepath.Clean(path)
	if path == "" || !filepath.IsAbs(dir) {
		return DirListingVO{Path: path, Error: "只能列出工作目录内的绝对路径"}
	}
	roots := append([]string{primary}, additional...)
	root, ok := rootContaining(roots, dir)
	if !ok {
		return DirListingVO{Path: dir, Error: "该路径不在本会话的工作目录内"}
	}
	labels := rootLabels(primary, additional)
	vo := DirListingVO{Path: dir, Root: root, Label: labels[root]}

	entries, err := os.ReadDir(dir)
	if err != nil {
		// Includes the directory having been deleted since the reader expanded it — a normal thing
		// to happen while an agent is working in the tree.
		vo.Error = "读取目录失败：" + err.Error()
		return vo
	}

	nodes := make([]FileNodeVO, 0, len(entries))
	for _, e := range entries {
		// The one name the tree hides. Everything else starting with a dot is shown: this column
		// answers what is on the disk (.env, .github, .tachi are all things a reader opens here).
		if e.Name() == ".git" {
			continue
		}
		full := filepath.Join(dir, e.Name())
		node := FileNodeVO{Name: e.Name(), Path: full, Link: e.Type()&fs.ModeSymlink != 0}
		// os.Stat, not the DirEntry's own type: a link pointing at a directory has to expand like one.
		if info, err := os.Stat(full); err != nil {
			node.Error = err.Error()
		} else {
			node.IsDir = info.IsDir()
			if !node.IsDir {
				node.Size = info.Size()
			}
		}
		node.Kind = treeKindHint(e.Name(), node.IsDir)
		nodes = append(nodes, node)
	}
	vo.Total = len(nodes)

	sortTreeNodes(nodes)
	if len(nodes) > fileTreeMaxEntries {
		nodes, vo.Truncated = nodes[:fileTreeMaxEntries], true
	}
	markIgnored(root, nodes)
	vo.Entries = nodes
	return vo
}

// PollWorkspacePaths reports the current state of a batch of paths — what the frontend's refresh
// timer asks about. It deliberately makes NO judgement about what a change MEANS (a directory's
// mtime moving means its entries changed; a file's size or mtime moving means its content did):
// that reading belongs to the half that has to show it.
//
// A session with no roots has nothing to poll, and says so with an empty answer rather than a list
// of "does not exist" for paths nobody listed.
func (s *AgentService) PollWorkspacePaths(sessionID string, paths []string) []PathStateVO {
	primary, _ := s.desk.sessionRoots(sessionID)
	if primary == "" {
		return nil
	}
	if len(paths) > fileTreePollMaxPaths {
		paths = paths[:fileTreePollMaxPaths]
	}
	out := make([]PathStateVO, 0, len(paths))
	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			continue
		}
		vo := PathStateVO{Path: p}
		if info, err := os.Stat(p); err == nil {
			vo.Exists = true
			vo.IsDir = info.IsDir()
			vo.Size = info.Size()
			vo.MTimeMs = info.ModTime().UnixMilli()
		}
		out = append(out, vo)
	}
	return out
}

// GetFileTreeMemo returns what the session's WORKSPACE remembers: the expanded directories and the
// file that was being read.
//
// Two things make the answer different from what is stored. The key is derived here (fileTreeKey),
// so the frontend cannot have its own idea of what a workspace is. And the paths are FILTERED on the
// way out: a directory that is no longer inside any root, or is no longer a directory, is dropped —
// a deleted directory must not leave a ghost row, and a root that was re-pointed (a project edited
// to another checkout) should simply start from the default expansion. The filter is a stat per
// path (hundreds at most).
//
// The result is not written back: that would cost a file write every time a panel opens, and what
// was stored was only ever valid when it was stored.
func (s *AgentService) GetFileTreeMemo(sessionID string) FileTreeMemoVO {
	key := s.desk.fileTreeKey(sessionID)
	if key == "" {
		return FileTreeMemoVO{Dirs: []string{}}
	}
	memo, ok := loadUIState().FileTreeMemo[key]
	if !ok {
		return FileTreeMemoVO{Key: key, Dirs: []string{}}
	}
	primary, additional := s.desk.sessionRoots(sessionID)
	roots := append([]string{primary}, additional...)
	dirs := make([]string, 0, len(memo.Dirs))
	for _, dir := range memo.Dirs {
		if len(dirs) == fileTreeMemoMaxDirs {
			break
		}
		if !insideAnyRoot(roots, dir) || !isDirNow(dir) {
			continue
		}
		dirs = append(dirs, dir)
	}
	file := ""
	if insideAnyRoot(roots, memo.File) {
		// Existence is deliberately NOT required: a file the reader had open that has since been
		// deleted reopens as 文件已不存在 (preview.go's `missing`), which is the honest answer.
		file = memo.File
	}
	return FileTreeMemoVO{Key: key, Found: true, Dirs: dirs, File: file}
}

// SetFileTreeMemo records one workspace's expansion state and the file on screen. The frontend
// debounces its calls (an expand/collapse/toggle is not worth a whole-file rewrite) and calls once
// more when the panel closes; this side only clamps, and settles which key it belongs to.
func (s *AgentService) SetFileTreeMemo(sessionID string, dirs []string, file string) {
	key := s.desk.fileTreeKey(sessionID)
	if key == "" {
		// No project and no primary directory: there is no tree to remember, and inventing a key
		// from the session id would remember one that nothing else can ever name.
		return
	}
	clean := make([]string, 0, len(dirs))
	seen := make(map[string]bool, len(dirs))
	for _, dir := range dirs {
		if len(clean) == fileTreeMemoMaxDirs {
			break
		}
		dir = filepath.Clean(dir)
		if dir == "." || dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		clean = append(clean, dir)
	}
	updateUIState(func(st *uiState) bool {
		if st.FileTreeMemo == nil {
			st.FileTreeMemo = map[string]fileTreeMemo{}
		}
		prev, existed := st.FileTreeMemo[key]
		next := fileTreeMemo{Dirs: clean, File: file, At: time.Now()}
		// A no-op repaint should not cost a write (updateUIState's rule): same paths, same file.
		if existed && sameStrings(prev.Dirs, next.Dirs) && prev.File == next.File {
			return false
		}
		st.FileTreeMemo[key] = next
		evictFileTreeMemo(st.FileTreeMemo)
		return true
	})
}

// fileTreeMemo is one workspace's remembered state as it is stored in desktop_ui.json: the
// expanded directories (absolute — with several roots, a relative one cannot say which tree it
// belongs to), the file being read, and when it was written (the table drops the oldest
// workspaces first).
type fileTreeMemo struct {
	Dirs []string  `json:"dirs,omitempty"`
	File string    `json:"file,omitempty"`
	At   time.Time `json:"at,omitempty"`
}

// The file tree column's width bounds and default, mirrored by the frontend
// (desktop/frontend/src/filetree.tsx): the layout clamps what a drag produces to the room there is,
// and this side keeps a hand-edited file from storing a width no layout could honour. The default
// lives ONLY in the frontend — a value this side never reads would be a second copy of it.
const (
	fileTreeMinWidth = 240
	fileTreeMaxWidth = 560
)

func validFileTreeWidth(px int) bool {
	return px >= fileTreeMinWidth && px <= fileTreeMaxWidth
}

// SetFileTreeOpen records whether the file-tree column is open. Written on every toggle and best
// effort, like the rest of uiState: a failure costs the next launch the column's position.
func (s *AgentService) SetFileTreeOpen(open bool) {
	updateUIState(func(st *uiState) bool {
		if st.FileTreeOpen == open {
			return false
		}
		st.FileTreeOpen = open
		return true
	})
}

// SetFileTreeWidth records how wide the reader made the column, refusing a value outside the bounds
// above (a hand-editable file, and a width the layout cannot honour is worse than the default).
func (s *AgentService) SetFileTreeWidth(px int) {
	if !validFileTreeWidth(px) {
		return
	}
	updateUIState(func(st *uiState) bool {
		if st.FileTreeWidth == px {
			return false
		}
		st.FileTreeWidth = px
		return true
	})
}

// fileTreeKey is the ONE definition of "the same workspace": a session driven by a usable project
// belongs to that project (every conversation in it shares one expansion — the tree's identity is
// the trees, not this conversation); anything else belongs to its primary directory. The two
// prefixes keep the two kinds of key from ever colliding (an id is not a path). "" means "nothing to
// remember": a project that no longer drives the session, or a session with no primary directory.
func (d *desktopApp) fileTreeKey(sessionID string) string {
	sess := d.sessionRecord(sessionID)
	if sess == nil {
		return ""
	}
	if sess.ProjectID != "" {
		if _, _, ok := d.projects.drives(sess.ProjectID); ok {
			return "p:" + sess.ProjectID
		}
	}
	if primary, _ := d.sessionRootsFrom(sess); primary != "" {
		return "d:" + primary
	}
	return ""
}

// evictFileTreeMemo keeps the memo table bounded by dropping the least recently written workspaces.
// Called with the key just written already in the map, so the entry being stored can never be the
// one evicted.
func evictFileTreeMemo(memo map[string]fileTreeMemo) {
	for len(memo) > fileTreeMemoMaxWorkspaces {
		oldestKey, oldestAt := "", time.Time{}
		for key, m := range memo {
			if oldestKey == "" || m.At.Before(oldestAt) {
				oldestKey, oldestAt = key, m.At
			}
		}
		if oldestKey == "" {
			return
		}
		delete(memo, oldestKey)
	}
}

// sortTreeNodes orders one layer the way fileindex.Immediate does (directories first, then names
// compared case-insensitively) — the tree column and the @-picker's column hold the same files, and
// two different orders for one directory read as two different directories. Stable, so names that
// differ only by case keep the order the filesystem gave them.
func sortTreeNodes(nodes []FileNodeVO) {
	sort.SliceStable(nodes, func(i, j int) bool {
		if nodes[i].IsDir != nodes[j].IsDir {
			return nodes[i].IsDir
		}
		return strings.Compare(strings.ToLower(nodes[i].Name), strings.ToLower(nodes[j].Name)) < 0
	})
}

// treeKindHint guesses a file's preview kind from its NAME alone, reusing the very tables
// preview.go classifies with so the two can never drift (a second table would say "text" here and
// open as binary there).
//
// It is only a hint because previewKind answers the real question by reading content (an 8KB probe)
// — correct for one file, unacceptable for the several hundred a layer holds. So an unmatched
// extension is kindUnknown and the icon is the neutral one; whether the file can actually be
// previewed is still decided when it is opened.
func treeKindHint(name string, isDir bool) string {
	if isDir {
		return kindDir
	}
	ext := strings.ToLower(filepath.Ext(name))
	switch {
	case imageExts[ext]:
		return kindImage
	case ext == ".md" || ext == ".markdown":
		return kindMarkdown
	case ext == ".html" || ext == ".htm" || ext == ".xhtml":
		return kindHTML
	case ext == ".pdf":
		return kindPDF
	case ext == ".csv" || ext == ".tsv":
		return kindCSV
	case binaryExts[ext]:
		return kindBinary
	}
	if _, ok := langByBase[name]; ok {
		return kindText
	}
	if _, ok := langByExt[ext]; ok {
		return kindText
	}
	return kindUnknown
}

// markIgnored fills each node's Ignored from one `git check-ignore -v` run over the whole layer.
// One subprocess per expanded directory is the price of the marks; a root that is not a git
// repository (or a machine without git) simply has none, which is not an error.
func markIgnored(root string, nodes []FileNodeVO) {
	if len(nodes) == 0 {
		return
	}
	paths := make([]string, 0, len(nodes))
	for _, n := range nodes {
		paths = append(paths, n.Path)
	}
	// git prints the path it was GIVEN (absolute here), so the map keys line up with the rows
	// without any re-resolution.
	rules := ignoredRuleMap(checkIgnoreLines(root, paths))
	for i := range nodes {
		nodes[i].Ignored = rules[nodes[i].Path]
	}
}

// rootContaining finds the root a requested path belongs to (literal containment, no symlink
// resolution — see the file comment).
//
// The FIRST match wins, which is what the diff panel's own attribution does (GetTurnDiff's loop over
// its roots): a path inside two nested roots therefore reads as the OUTER one everywhere in this app.
// Making this one pick the most specific root instead would give the tree and the diff panel two
// different answers about the same file, which is the failure this function exists to prevent.
func rootContaining(roots []string, dir string) (string, bool) {
	for _, root := range roots {
		if root != "" && dirContains(root, dir) {
			return root, true
		}
	}
	return "", false
}

// insideAnyRoot is the same rule as a predicate, for a path that is not about to be read. An empty
// path is inside nothing (an empty memo field must not read as "the root itself").
func insideAnyRoot(roots []string, path string) bool {
	if path == "" {
		return false
	}
	_, ok := rootContaining(roots, filepath.Clean(path))
	return ok
}

// isDirNow reports whether a remembered path is still a directory (following symlinks: a link the
// reader expanded is the directory they expanded).
func isDirNow(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// sameStrings compares two path lists for the "nothing changed, do not write" check.
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
