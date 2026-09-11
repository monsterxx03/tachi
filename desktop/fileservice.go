package main

// @-file support for the desktop input area: fuzzy completion over the
// session's working directory, mapping of dropped file paths to @-references,
// and forwarding of Wails' native file-drop events to the frontend.
//
// The webview cannot read dropped paths itself, so the flow is always
// Wails → Go (WindowFilesDropped) → frontend event → input area. Search and
// reference resolution run here, and always against the SAME root — the
// session's working directory — so a reference inserted by the UI resolves to
// the file the agent's tools would open.

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/monsterxx03/tachi/agent/atfile"
	"github.com/monsterxx03/tachi/pkg/fileindex"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// FileDropEvent is emitted to the frontend when files are dropped onto the
// window. Paths are the raw native paths; the frontend asks for @-references
// via ResolveDroppedPaths (which needs the session the drop landed in).
type FileDropEvent struct {
	Paths     []string `json:"paths"`
	ElementID string   `json:"elementId"`
}

// FileMatchVO is one @-file completion match, relative to the search root.
type FileMatchVO struct {
	// Path is what the picker SHOWS: always relative to the root the match came
	// from (that is what keeps a row readable — "[shared-lib] src/x.go").
	Path  string `json:"path"`
	IsDir bool   `json:"isDir"`
	// Ref is the complete reference to insert into the input, '@' included. It is
	// the authoritative form: a match under the primary root resolves to "@rel",
	// one under an additional root to an absolute "@abs" (agent/atfile.RefForPath
	// decides, so the rule lives in one place).
	Ref string `json:"ref"`
	// Root labels where the match came from: "" for the primary root, otherwise the
	// root's base name — or its full path when two roots share a base name.
	Root string `json:"root"`
}

// DroppedFileVO describes one dropped file as it will be referenced.
type DroppedFileVO struct {
	Ref   string `json:"ref"`  // "@relative/path", inserted into the input
	Name  string `json:"name"` // base name, for the UI chip/tooltip
	IsDir bool   `json:"isDir"`
	Kind  string `json:"kind"` // dir | image | text | binary | missing
	Size  int64  `json:"size"` // bytes (0 for directories)
}

// searchRoot is one root the @-file search walks.
type searchRoot struct {
	path  string
	label string // "" = the primary root
}

// SearchFiles returns @-file completion matches for a session.
//
// The whole root set is searched (primary + the session's additional roots): an
// additional root is invisible to a primary-only index, and "I added the folder,
// why can't it see it" is the gap this closes. Three rules keep the merged list
// honest:
//
//   - each root gets its own share of the limit, so one large root cannot crowd
//     the others out of the list;
//   - a file reachable through two roots (nested roots, e.g. primary=/repo/pkg
//     with /repo added) appears ONCE, attributed to the primary — the relative
//     reference is the stable one;
//   - a root that cannot be listed (deleted, unmounted, no ripgrep) contributes
//     nothing instead of failing the whole search.
func (s *AgentService) SearchFiles(sessionID, query string, limit int) []FileMatchVO {
	d := s.desk
	if limit <= 0 {
		limit = fileindex.DefaultLimit
	}
	roots := d.searchRoots(sessionID)
	if len(roots) == 0 {
		return nil // no workspace chosen yet (see searchRoots)
	}

	// A query that IS a path is the drill-down after an inserted absolute
	// reference: the picker keeps the typed text as the query, and for a root
	// outside the primary that text is absolute. Fuzzy-matching it against
	// root-relative paths could never match, so it is listed directly.
	if strings.HasPrefix(query, "/") {
		return absoluteEntries(d, roots, query, limit)
	}
	return rootMatches(d, roots, query, limit)
}

// rootMatches searches every root, merged round by score.
func rootMatches(d *desktopApp, roots []searchRoot, query string, limit int) []FileMatchVO {
	per := (limit + len(roots) - 1) / len(roots) // ceil: every root gets a share
	type scored struct {
		vo    FileMatchVO
		score int
	}
	hits := make([]scored, 0, limit)
	seen := make(map[string]bool, limit)

	for _, root := range roots {
		matches, err := d.fileIndex.Search(root.path, query, per)
		if err != nil {
			continue // a root we cannot list contributes nothing (see SearchFiles)
		}
		for _, m := range matches {
			abs := filepath.Join(root.path, filepath.FromSlash(m.Path))
			if seen[abs] {
				continue
			}
			seen[abs] = true
			hits = append(hits, scored{vo: matchVO(roots[0].path, root, abs, m.Path, m.IsDir), score: m.Score})
		}
	}

	// Stable: equal scores keep root order, so the primary's matches stay on top.
	slices.SortStableFunc(hits, func(a, b scored) int { return b.score - a.score })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]FileMatchVO, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.vo)
	}
	return out
}

// absoluteEntries lists one directory for the absolute drill-down. The directory
// is attributed to the root it lives under, so the picker keeps showing where a
// path is coming from.
func absoluteEntries(d *desktopApp, roots []searchRoot, dir string, limit int) []FileMatchVO {
	dir = filepath.Clean(dir)
	if !fileutil.IsDir(dir) {
		return nil
	}
	matches, err := d.fileIndex.Search(dir, "", limit)
	if err != nil {
		return nil
	}

	// The deepest root containing dir labels the entries (the root itself when the
	// user drilled into a root's top level).
	owner := roots[0]
	for _, root := range roots {
		if strings.HasPrefix(dir+"/", root.path+"/") {
			owner = root
		}
	}

	out := make([]FileMatchVO, 0, len(matches))
	for _, m := range matches {
		out = append(out, matchVO(roots[0].path, owner, filepath.Join(dir, filepath.FromSlash(m.Path)), m.Path, m.IsDir))
	}
	return out
}

// matchVO renders one hit: the reference to insert (from the shared rule, so it is
// always something the backend can expand again), the display path relative to its
// root, and the root label.
func matchVO(primary string, root searchRoot, abs, rel string, isDir bool) FileMatchVO {
	ref, ok := atfile.RefForPath(primary, abs)
	if !ok {
		// The file vanished between listing and rendering (a rebuild race). Keep the
		// row usable rather than dropping it mid-keystroke.
		ref = "@" + filepath.ToSlash(abs)
	}
	return FileMatchVO{Path: rel, IsDir: isDir, Ref: ref, Root: root.label}
}

// searchRoots returns the roots @-file completion walks: the primary first, then
// the session's additional roots that still exist (a root whose directory is gone
// has nothing to list — see the design's staleness rule). Labels disambiguate the
// picker; two roots sharing a base name fall back to their full paths.
//
// The first element is always the primary: callers rely on roots[0] for the
// reference rule.
func (d *desktopApp) searchRoots(sessionID string) []searchRoot {
	primary, additional := d.sessionRoots(sessionID)
	if primary == "" {
		if d.sessionIsKnown(sessionID) {
			// A session whose workspace the user has not chosen yet: search NOTHING.
			// Falling back to the process cwd here would build an index of "/" for a
			// Finder-launched app — the very cost this design exists to avoid — and
			// the references it produced would resolve against a directory that is not
			// a workspace. The composer's chip asks for a directory instead.
			return nil
		}
		primary = processCWD() // no session at all (tests, one-off callers)
	}

	live := make([]string, 0, len(additional))
	baseNames := make(map[string]int, len(additional))
	for _, dir := range additional {
		if !fileutil.IsDir(dir) {
			continue
		}
		live = append(live, dir)
		baseNames[filepath.Base(dir)]++
	}

	roots := make([]searchRoot, 0, len(live)+1)
	roots = append(roots, searchRoot{path: primary})
	for _, dir := range live {
		label := filepath.Base(dir)
		if baseNames[label] > 1 {
			label = dir
		}
		roots = append(roots, searchRoot{path: dir, label: label})
	}
	return roots
}

// sessionIsKnown reports whether id belongs to a run this app manages, as opposed to
// an id no session was ever created for.
func (d *desktopApp) sessionIsKnown(id string) bool {
	d.mu.Lock()
	r := d.getRun(id)
	d.mu.Unlock()
	return r != nil && r.sm != nil && r.sm.Current() != nil
}

// ResolveDroppedPaths maps paths dropped onto the window into @-references for
// a session, skipping paths that no longer exist.
func (s *AgentService) ResolveDroppedPaths(sessionID string, paths []string) []DroppedFileVO {
	root := s.desk.atFileRoot(sessionID)

	out := make([]DroppedFileVO, 0, len(paths))
	for _, p := range paths {
		// RefForPath already decides the form for a path outside the primary root
		// (absolute), so no per-root search is needed here.
		ref, ok := atfile.RefForPath(root, p)
		if !ok {
			continue
		}
		kind, size := atfile.Classify(p)
		out = append(out, DroppedFileVO{
			Ref:   ref,
			Name:  filepath.Base(p),
			IsDir: kind == atfile.KindDir,
			Kind:  kindName(kind),
			Size:  size,
		})
	}
	return out
}

// kindName renders an atfile.Kind for the frontend.
func kindName(k atfile.Kind) string {
	switch k {
	case atfile.KindDir:
		return "dir"
	case atfile.KindImage:
		return "image"
	case atfile.KindImageTooLarge:
		return "image-too-large"
	case atfile.KindText:
		return "text"
	case atfile.KindBinary:
		return "binary"
	default:
		return "missing"
	}
}

// emitFileDrop forwards a native file drop to the frontend.
func (d *desktopApp) emitFileDrop(paths []string, elementID string) {
	if d.app == nil || len(paths) == 0 {
		return
	}
	d.app.Event.Emit("agent:filedrop", FileDropEvent{Paths: paths, ElementID: elementID})
}

// atFileRoot returns the directory a session's @-file references resolve
// against (see expansionRoot; this variant takes d.mu itself).
func (d *desktopApp) atFileRoot(sessionID string) string {
	d.mu.Lock()
	r := d.getRun(sessionID)
	d.mu.Unlock()
	return d.expansionRoot(r)
}

// expansionRoot returns the @-file resolution root of a run: the session's
// working directory when set, otherwise the process working directory — the
// same fallback the tools' wdctx uses, so references and tool paths always
// point at the same tree.
//
// The caller must hold d.mu (the run's session manager is read here).
func (d *desktopApp) expansionRoot(r *sessionRun) string {
	if r != nil && r.sm != nil {
		if cur := r.sm.Current(); cur != nil && cur.WorkingDir != "" {
			return cur.WorkingDir
		}
	}
	return processCWD()
}

// processCWD is the working-directory fallback for sessions without one.
func processCWD() string {
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	return "."
}

// sessionWorkDir returns a session's configured working directory ("" when
// unset). The per-session session manager is bound as soon as the session is
// activated (AgentService.ActivateSession → prepareSession).
func (d *desktopApp) sessionWorkDir(sessionID string) string {
	d.mu.Lock()
	r := d.getRun(sessionID)
	d.mu.Unlock()
	if r == nil || r.sm == nil {
		return ""
	}
	if cur := r.sm.Current(); cur != nil {
		return cur.WorkingDir
	}
	return ""
}

// newFileIndex builds the @-file completion index, logging index builds under
// the desktop logger.
func newFileIndex() *fileindex.Index {
	return fileindex.New(logger.New("desktop"))
}
