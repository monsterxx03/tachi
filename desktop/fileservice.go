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
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/monsterxx03/tachi/agent/agentref"
	"github.com/monsterxx03/tachi/agent/atfile"
	"github.com/monsterxx03/tachi/agent/sessionref"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/node"
	"github.com/monsterxx03/tachi/pkg/fileindex"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/pkg/strutil"
	"github.com/monsterxx03/tachi/session"
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

// sessionSource builds the #s: resolver over the app's ONE session store: a
// reference expands to a pointer into it, and the picker searches the same list.
//
// It is built per call on purpose — a Source holds a lister and a directory, while
// the memoized list it reads lives in the store itself — and nil means the session
// namespace offers nothing (no store, which the app degrades to when the session
// directory cannot be resolved).
func (d *desktopApp) sessionSource() *sessionref.Source {
	store := d.sessionStore()
	if store == nil {
		return nil
	}
	dir, err := config.SessionDir()
	if err != nil {
		return nil
	}
	return sessionref.New(store.ListSessions, dir)
}

// agentSource builds the #a: resolver over this machine's nodes — the sending side of the same
// picker the session namespace is the reading side of.
//
// The PeerList is kept for the life of the app rather than built per call, and that is load-bearing:
// it holds the last probe's results AND the rate limit that stops a keystroke from being a request,
// so rebuilding it per keystroke would probe on every keystroke. Both the peer list and the sender
// behind it are read through providers, so the cache follows a config the reader edits.
func (d *desktopApp) agentSource() *agentref.Source {
	list := d.peerList()
	if list == nil {
		return nil
	}
	return agentref.New(func() []agentref.Peer {
		list.Probe()
		return list.Snapshot()
	})
}

// warmPeers asks this machine's nodes who they are, in the background, as the app starts.
//
// The picker's list IS the warmup: its probe is asynchronous and rate-limited (node.PeerList), the
// answers it lands are the rows a person sees the moment they type '#', and those rows are read
// through the sending side — which is also what brings the discovery browser up, so a machine that
// has nothing configured but can find nodes is already listening when the reader arrives. The same
// round leaves the pins and the card cache behind that the first turn's reminder reads
// (node.DelegatorCache.WarmUp, the same round without a list to land in).
//
// Nothing here waits, and a machine with no peers and discovery off costs nothing: the list has
// nothing to ask and the sending side is never built.
func (d *desktopApp) warmPeers() {
	if list := d.peerList(); list != nil {
		list.Probe()
	}
}

// peerList is the app's ONE node list, shared by the '#' picker and the node board: one probe, one
// rate limit, one set of answers, so the two cannot disagree about what is up.
func (d *desktopApp) peerList() *node.PeerList {
	d.peersMu.Lock()
	defer d.peersMu.Unlock()
	if d.peers == nil {
		d.peers = node.NewPeerList(
			// The rows come from the sending side's directory, resolved per read: it holds the
			// configured machines AND the ones discovery has found, and a captured list would go
			// stale the moment either changed.
			node.RowsOf(func() *node.Sender { return d.senderFor(d.config()) }),
			func(ctx context.Context, force bool) []node.PeerReport {
				// Resolved per probe, not captured at construction: the peers are a config leaf the
				// settings UI edits in place, and a captured sender would keep asking the machines
				// this app used to know about.
				if sender := d.senderFor(d.config()); sender != nil {
					return sender.PeerReports(ctx, force)
				}
				return nil
			},
			logger.New("desktop"),
		)
	}
	return d.peers
}

// SessionMatchVO is one #s: completion match.
type SessionMatchVO struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Date  string `json:"date"` // display form of the session's last update
	// Ref is the complete reference to insert into the input, '@' included —
	// agent/atfile's own spelling (atfile.RefForSession), so a row the popup
	// offered is a reference the expansion reads back.
	Ref string `json:"ref"`
	// WorkingDir is the directory that conversation ran in. It is what tells two
	// sessions with the same title apart, so it rides along for display.
	WorkingDir string `json:"workingDir"`
}

// AgentMatchVO is one #a: completion match: a node this machine may delegate work to.
//
// Every field is already in the words a row shows, including the ones that could have been left to
// the frontend. That is deliberate: "reachable", "checked" and an identity's short form are the same
// fact in the TUI, and two frontends each deciding how to phrase them is how one window ends up
// saying "offline" while the other says "checking".
type AgentMatchVO struct {
	Label string `json:"label"`
	// Name is what to call the node on the row: the label when there is one, else what the node calls
	// itself, else the front of its id.
	Name string `json:"name"`
	// NodeID is the identity pinned on first outbound contact, empty when this machine has never
	// called that node. It is never the name the node reports about itself.
	NodeID string `json:"nodeId"`
	// IDLabel is the identity as a row shows it: the first characters of the pinned id, or the
	// fact that this machine holds no record of it.
	IDLabel string `json:"idLabel"`
	// Reach is what the last look found (agentref.ReachWord): 可用 / 联系不上 / 检查中….
	Reach string `json:"reach"`
	// Note is the node's own description, or the configured line when it has not answered.
	Note string `json:"note"`
	// Ref is the complete reference to insert (atfile.RefForAgent), so a row the popup offered is
	// a reference the expansion reads back.
	Ref string `json:"ref"`
}

// WarmAgents starts asking this machine's nodes whether they are up, and returns
// without waiting. The composer calls it the moment the '#' mark is typed: the answer is a round
// trip per node, and the typing that follows is the time to spend on it. What it finds shows up in
// a later SearchAgents.
func (s *AgentService) WarmAgents() {
	s.desk.agentSource().Warm()
}

// SearchAgents returns #a: completion matches: the nodes this machine may delegate to, ranked by
// the alias the reader is typing.
//
// It never waits for the network. The list itself is configuration plus the pinned identities (both
// local files), and reachability is whatever the last background probe found — a fresh probe is
// started here, and its answer shows up in a later call. That ordering is what lets the picker run
// on every keystroke.
func (s *AgentService) SearchAgents(query string, limit int) []AgentMatchVO {
	source := s.desk.agentSource()
	if source == nil {
		return nil
	}
	targets := source.Search(query, limit)
	matches := make([]AgentMatchVO, len(targets))
	for i, t := range targets {
		matches[i] = AgentMatchVO{
			Label:   t.Label,
			Name:    t.Name,
			NodeID:  t.NodeID,
			IDLabel: agentref.IDLabel(t),
			Reach:   agentref.ReachWord(t),
			Note:    agentref.RowNote(t, 0),
			Ref:     atfile.RefForAgent(t.NodeID),
		}
	}
	return matches
}

// SearchSessions returns #s: completion matches: other conversations, ranked
// by the store (title, id, working directory). exclude is the session the composer
// belongs to — offering the conversation you are already in says nothing and
// references itself.
func (s *AgentService) SearchSessions(exclude, query string, limit int) []SessionMatchVO {
	source := s.desk.sessionSource()
	if source == nil {
		return nil
	}
	targets := source.Search(query, exclude, limit)
	matches := make([]SessionMatchVO, len(targets))
	for i, t := range targets {
		matches[i] = SessionMatchVO{
			ID:         t.ID,
			Title:      t.Title,
			Date:       t.UpdatedAt.Format(strutil.TimeFormatDateTimeShort),
			Ref:        atfile.RefForSession(t.ID),
			WorkingDir: t.WorkingDir,
		}
	}
	return matches
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

	labels := rootLabels(primary, additional)
	live := make([]string, 0, len(additional))
	for _, dir := range additional {
		if !fileutil.IsDir(dir) {
			continue
		}
		live = append(live, dir)
	}

	roots := make([]searchRoot, 0, len(live)+1)
	roots = append(roots, searchRoot{path: primary})
	for _, dir := range live {
		roots = append(roots, searchRoot{path: dir, label: labels[dir]})
	}
	return roots
}

// rootLabels maps every DECLARED workspace root to the label the UI names it by: "" for the
// primary, else the root's base name — or its full path when two roots share a base name.
//
// One rule, shared by the @-picker and the diff panel (see searchRoots and
// FileDiffVO.RootLabel): two roots holding the same relative path have to be tellable apart
// everywhere in the app, and not in two different ways. It is built from what the session
// declares rather than from what exists on disk, because a diff may name a root whose
// directory has since been removed.
func rootLabels(primary string, additional []string) map[string]string {
	labels := map[string]string{primary: ""}
	baseNames := make(map[string]int, len(additional))
	for _, dir := range additional {
		baseNames[filepath.Base(dir)]++
	}
	for _, dir := range additional {
		label := filepath.Base(dir)
		if baseNames[label] > 1 {
			label = dir
		}
		labels[dir] = label
	}
	return labels
}

// rootLabelFor names one root: the session's own label when it still declares it, else its
// base name — a root the user removed since must not silently read as the primary one (that is
// what the empty label means).
func rootLabelFor(labels map[string]string, root string) string {
	if label, ok := labels[root]; ok {
		return label
	}
	return filepath.Base(root)
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

// PastedImageVO is what a paste produced: the @-reference to splice into the input area, or the
// reason it was not stored. Two fields rather than one string, so the frontend can tell "here is a
// reference" from "here is why not" without sniffing the text for a leading '@'.
type PastedImageVO struct {
	Ref   string `json:"ref,omitempty"`
	Error string `json:"error,omitempty"`
}

// SavePastedImage stores an image pasted into the composer and answers with the @-reference that
// attaches it — the SAME route a dropped file takes (ResolveDroppedPaths), because that is already
// how an image reaches the model: @-file expansion turns the reference into a multi-modal content
// part, and the bubble keeps showing the text the user sent (DisplayContent).
//
// data is base64, and it is the only thing the webview has: a screenshot on the clipboard is bytes
// with no path, and a string is a shape the binding generator handles everywhere. The file name is
// generated — there is no name to preserve, and one paste must not overwrite the last.
//
// It carries no root: the reference this returns is ABSOLUTE (the pasted file lives under the
// session directory, which is never inside a workspace root — Tachi's own state is refused as a
// root), and an absolute reference resolves as-is. That is also why nothing has to be re-scanned on
// RELOAD: a session's stored records are converted as they are (ConvertSessionToLLMMessages never
// expands anything), so the transcript afterwards shows the reference the user typed.
//
// The IMAGE, though, does survive the reload — from the record rather than from this file:
// expansion is a per-turn thing, and the part it produced is persisted with its bytes by the
// session store (session.Message.Parts + the session's images/ blobs). So a later turn, in this
// process or after a restart, rebuilds a request that still carries the image
// (docs/agents/prompt-cache.md). This pasted file is what the @-reference keeps pointing at — a
// copy of the bytes, deliberately, since the record must not depend on a file the reader can
// edit or delete.
//
// WHERE the file goes is the session package's business, not this binding's: the TUI pastes images
// too, and its own way in is the same file in the same place (session.SavePastedImage) — one
// layout for both frontends, stated once.
func (s *AgentService) SavePastedImage(sessionID, mediaType, data string) PastedImageVO {
	ext, ok := llm.ImageExtForMediaType(mediaType)
	if !ok {
		return PastedImageVO{Error: "粘贴的不是支持的图片（支持 png / jpg / gif / webp）"}
	}
	raw, err := base64.StdEncoding.DecodeString(stripSpace(data))
	if err != nil || len(raw) == 0 {
		return PastedImageVO{Error: "粘贴的图片读不出来"}
	}
	// Refused HERE instead of stored and annotated by expansion: a file over the model's limit
	// would sit in the session forever and never be seen by anyone, so the reader is better told
	// now, while the screenshot is still on their clipboard.
	if int64(len(raw)) > llm.MaxImageSize {
		return PastedImageVO{Error: fmt.Sprintf("粘贴的图片太大（%s，上限 %s）",
			strutil.HumanBytes(int64(len(raw))), strutil.HumanBytes(llm.MaxImageSize))}
	}
	// The id names one directory under the store and comes from the webview, so it is resolved —
	// and refused when it is not a single path element — before anything is written; the message is
	// this frontend's, which is why the resolution happens here rather than inside the store call.
	if _, err := sessionDirPath(sessionID); err != nil {
		return PastedImageVO{Error: err.Error()}
	}
	path, err := session.SavePastedImage(sessionID, ext, raw)
	if err != nil {
		return PastedImageVO{Error: "保存粘贴的图片失败：" + err.Error()}
	}
	return PastedImageVO{Ref: "@" + path}
}

// stripSpace removes the whitespace a base64 payload may carry (a data URL read by a browser does
// not add any, but a decoder rejects what it finds rather than ignoring it).
func stripSpace(s string) string {
	if !strings.ContainsAny(s, " \t\r\n") {
		return s
	}
	return strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "").Replace(s)
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

// expansionRoot returns the @-file resolution root of a run: the session's primary
// root when set, otherwise the process working directory — the same fallback the
// tools' wdctx uses, so references and tool paths always point at the same tree.
//
// The primary comes from sessionRootsFrom, never from the record directly: this is one
// of the consumers that used to read WorkingDir itself and so kept resolving @-file
// references into the OLD tree after a directory change, while the tools had already
// moved to the new one.
//
// The caller must hold d.mu (the run's session manager is read here; sessionRootsFrom
// takes no locks of its own).
func (d *desktopApp) expansionRoot(r *sessionRun) string {
	if r != nil && r.sm != nil {
		if cur := r.sm.Current(); cur != nil {
			if primary, _ := d.sessionRootsFrom(cur); primary != "" {
				return primary
			}
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

// sessionWorkDir returns a session's primary root ("" when unset): the directory bash
// runs in, relative paths resolve against, and the composer's chip names. It is a thin
// wrapper over sessionRoots, so it can never disagree with the prompt or the @-file
// root — the record it reads is the same one they do.
func (d *desktopApp) sessionWorkDir(sessionID string) string {
	primary, _ := d.sessionRoots(sessionID)
	return primary
}

// newFileIndex builds the @-file completion index, logging index builds under
// the desktop logger.
func newFileIndex() *fileindex.Index {
	return fileindex.New(logger.New("desktop"))
}
