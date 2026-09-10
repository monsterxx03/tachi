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

	"github.com/monsterxx03/tachi/agent/atfile"
	"github.com/monsterxx03/tachi/pkg/fileindex"
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
	Path  string `json:"path"`
	IsDir bool   `json:"isDir"`
}

// DroppedFileVO describes one dropped file as it will be referenced.
type DroppedFileVO struct {
	Ref   string `json:"ref"`  // "@relative/path", inserted into the input
	Name  string `json:"name"` // base name, for the UI chip/tooltip
	IsDir bool   `json:"isDir"`
	Kind  string `json:"kind"` // dir | image | text | binary | missing
	Size  int64  `json:"size"` // bytes (0 for directories)
}

// SearchFiles returns @-file completion matches for a session. An empty query
// lists the immediate entries of the session's working directory; otherwise the
// whole tree is fuzzy-searched. A failed search (no ripgrep, unreadable root)
// yields no completions rather than an error the input area would have to
// handle on every keystroke; the index logs the underlying failure once per
// cache period.
func (s *AgentService) SearchFiles(sessionID, query string, limit int) []FileMatchVO {
	matches, err := s.desk.fileIndex.Search(s.desk.atFileRoot(sessionID), query, limit)
	if err != nil {
		return nil
	}

	out := make([]FileMatchVO, 0, len(matches))
	for _, m := range matches {
		out = append(out, FileMatchVO{Path: m.Path, IsDir: m.IsDir})
	}
	return out
}

// ResolveDroppedPaths maps paths dropped onto the window into @-references for
// a session, skipping paths that no longer exist.
func (s *AgentService) ResolveDroppedPaths(sessionID string, paths []string) []DroppedFileVO {
	root := s.desk.atFileRoot(sessionID)

	out := make([]DroppedFileVO, 0, len(paths))
	for _, p := range paths {
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
