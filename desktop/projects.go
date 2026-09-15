package main

// Desktop projects: a named workspace (primary + additional roots) that OWNS a group of
// sessions. A session in a project stores only `project_id`; its roots are resolved from
// the project on every read (see sessionRootsFrom), so editing a project moves every
// member session at once with one file write and no fan-out into session meta.
//
// Design: docs/2026-09-14-desktop-project-design.md §2, §3.1.
//
// Three properties of this file are load-bearing:
//
//   - The table is the only place a project is read from or written to, and it lives in
//     memory once loaded, because sessionRootsFrom asks it on every turn — a disk read
//     there would put a meta.json worth of I/O in the prompt-building path.
//   - Roots are validated on the way IN (a user typed them) and on the way OUT
//     (projects.json is a file anyone can edit): a hand-edited `/` or `$HOME` must not
//     reach bash's cwd, the @-file index or a checkpoint.
//   - Nothing here takes d.mu or calls back into the app. The lock order is
//     d.mu → projects.mu; the reverse (holding this lock while touching the app) is what
//     would deadlock, so it is never done.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/session"
)

// projectsFileName is the project table, next to desktop_ui.json in tachi's state dir.
// It is desktop STATE, not user configuration: nothing outside the desktop reads it, and
// it is never written into config.yaml.
const projectsFileName = "projects.json"

// project is one named workspace. ID — never the name — is what a session stores and what
// names a worktree directory, so renaming a project rewrites one string here and nothing
// on disk moves.
type project struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	WorkingDir     string    `json:"workingDir"`
	AdditionalDirs []string  `json:"additionalDirs,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// projectFile is the on-disk shape. A slice rather than a map so the file keeps a stable,
// readable order and a hand-edit cannot silently drop an entry to a duplicated key.
type projectFile struct {
	Projects []*project `json:"projects"`
}

// projectTable is the in-memory view of projects.json, guarded by its own lock. It starts
// empty and loads on first use, so a corrupt or missing file degrades to "no projects" —
// every member session then falls back to its own snapshot — instead of failing startup.
//
// The read methods tolerate a nil receiver ("no projects"), because a hand-built
// desktopApp in a test may legitimately leave the table unset and a session's root
// resolution must not depend on which constructor was used.
type projectTable struct {
	mu       sync.RWMutex
	projects []*project
	loaded   bool
	// broken records that projects.json EXISTS but could not be read or parsed. The
	// table then holds no projects, and saving MUST NOT happen: a write would replace a
	// file the user can still repair with just the entry being added right now, losing
	// every project in it. Reads keep degrading (a broken file must not make the desktop
	// unusable); a write refuses and says so — see upsert.
	broken bool
}

// projectsPath is resolved per call rather than cached: config.BaseDir is a process global
// that tests repoint, and a path cached at construction would write a user's table into a
// temp dir (or read a temp dir's table forever).
func projectsPath() string {
	return filepath.Join(config.BaseDir(), projectsFileName)
}

// load reads the file once. A missing file is the normal first-run case; anything else
// unreadable is warned about, recorded as broken, and treated the same way — because a
// broken projects.json must not make the desktop unusable. Nothing is written back: a
// broken file is left exactly as it is (and upsert refuses to save over it), so the user
// can repair it.
func (t *projectTable) load() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.loaded {
		return
	}
	t.loaded = true
	t.readLocked()
}

// readLocked (re)reads projects.json into the table and records whether it was readable at
// all. Callers hold t.mu. load() runs it once; upsert runs it again while broken, so a user
// who repairs or deletes the file gets the feature back without restarting the app.
func (t *projectTable) readLocked() {
	t.projects = nil
	t.broken = false

	path := projectsPath()
	var pf projectFile
	if err := fileutil.ReadJSON(path, &pf); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			t.broken = true
			logger.New("desktop").Warn(context.Background(),
				"projects.json unreadable, starting with no projects", "path", path, "err", err)
		}
		return
	}
	// Entries are kept even when their roots no longer validate: dropping one here would
	// unbind every member session AND delete the project on the next save. Unusable
	// entries are filtered at read time instead (projectTable.drives).
	for _, p := range pf.Projects {
		if p == nil || p.ID == "" {
			continue
		}
		t.projects = append(t.projects, p)
	}
}

// list returns a copy of every project, newest first — the order the sidebar groups use.
func (t *projectTable) list() []project {
	if t == nil {
		return nil
	}
	t.load()
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]project, 0, len(t.projects))
	for _, p := range t.projects {
		out = append(out, *p)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// get returns one project by ID, if it is there at all.
func (t *projectTable) get(id string) (project, bool) {
	if t == nil || id == "" {
		return project{}, false
	}
	t.load()
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, p := range t.projects {
		if p.ID == id {
			return *p, true
		}
	}
	return project{}, false
}

// drives reports whether a project can drive its member sessions RIGHT NOW, and with which
// primary: it exists and its PRIMARY root is still usable (see resolveProjectPrimary).
// This is THE predicate — the resolver (sessionRootsFrom), the write guards (projectGuard)
// and the UI's rootsUsable all read it, so the read and write paths can never disagree
// about whether a session is a project member.
//
// A project whose primary directory has vanished does not drive on purpose: handing a tree
// that is not there to bash would leave the session with no working directory at all, and
// the snapshot is the last known-good workspace. Additional roots do NOT get a say — they
// follow the session rule instead (an unmounted one is reported exists=false and skipped
// by the prompt and the @-file search), because one unmounted volume must not relocate
// every member session back to its snapshot.
func (t *projectTable) drives(id string) (project, string, bool) {
	p, ok := t.get(id)
	if !ok {
		return project{}, "", false
	}
	primary, err := resolveProjectPrimary(p.WorkingDir)
	if err != nil {
		return project{}, "", false
	}
	return p, primary, true
}

// usable is drives for callers that only need the project and the verdict.
func (t *projectTable) usable(id string) (project, bool) {
	p, _, ok := t.drives(id)
	return p, ok
}

// upsert inserts or replaces a project and writes the whole table. It is the only writer.
func (t *projectTable) upsert(p project) error {
	t.load()
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.writableLocked(); err != nil {
		return err
	}
	found := false
	for i, existing := range t.projects {
		if existing.ID == p.ID {
			t.projects[i] = &p
			found = true
			break
		}
	}
	if !found {
		t.projects = append(t.projects, &p)
	}
	return t.saveLocked()
}

// writableLocked reports whether the table may be written, re-reading the file first while it
// is marked broken: a save would replace whatever the user still has (and can repair) with
// only the change being made right now, so a file we could not read is never written over —
// but repairing or deleting it must be enough to get the feature back, without a restart.
// Callers hold t.mu.
func (t *projectTable) writableLocked() error {
	if t.broken {
		t.readLocked()
	}
	if t.broken {
		return projectsUnreadableError(projectsPath())
	}
	return nil
}

// remove deletes a project and writes the table out. It is the only delete; the caller is
// DeleteProject, which has already detached the members.
func (t *projectTable) remove(id string) error {
	t.load()
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.writableLocked(); err != nil {
		return err
	}
	kept := make([]*project, 0, len(t.projects))
	for _, p := range t.projects {
		if p.ID != id {
			kept = append(kept, p)
		}
	}
	t.projects = kept
	return t.saveLocked()
}

// saveLocked writes the table out. Callers hold t.mu.
func (t *projectTable) saveLocked() error {
	pf := projectFile{Projects: t.projects}
	if pf.Projects == nil {
		pf.Projects = []*project{}
	}
	return fileutil.AtomicWriteJSONShared(projectsPath(), pf)
}

// projectNameFor is the default name for a project rooted at primary: the directory's base
// name, with -2 / -3 appended while it collides. The resolved name is what the sidebar
// shows, so two projects must never read the same.
func (t *projectTable) projectNameFor(primary string) string {
	base := filepath.Base(filepath.Clean(primary))
	if base == "" || base == string(filepath.Separator) || base == "." {
		base = "项目"
	}
	taken := make(map[string]bool)
	for _, p := range t.list() {
		taken[p.Name] = true
	}
	if !taken[base] {
		return base
	}
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s-%d", base, n)
		if !taken[candidate] {
			return candidate
		}
	}
}

// nameTaken reports whether another project already reads as name; exceptID is the project
// being edited, whose own name is not a collision. It is what keeps the sidebar's groups
// apart: the name IS the label, so two projects must never read the same — including the
// ones a user typed, not only the derived defaults.
func (t *projectTable) nameTaken(name, exceptID string) bool {
	for _, p := range t.list() {
		if p.ID != exceptID && p.Name == name {
			return true
		}
	}
	return false
}

// resolveProjectPrimary validates a project's PRIMARY root and returns the form every
// consumer must use (absolute, cleaned). It is the READ half of the root rules, and it
// deliberately looks at the primary only: a project's additional roots follow the SESSION's
// staleness rule (see roots.go's header) — an unmounted one is reported exists=false, the
// prompt and the @-file search skip it, and the project keeps driving its members.
//
// The primary is not whitespace-checked, on purpose: that rule exists for @-references
// (whitespace-delimited ABSOLUTE refs into an additional root), and a relative reference
// never contains the primary itself. It is the same rule SetSessionWorkingDir applies to a
// session's primary.
func resolveProjectPrimary(primary string) (string, error) {
	if strings.TrimSpace(primary) == "" {
		return "", errors.New("请选择主目录")
	}
	abs, err := expandRootPath(primary)
	if err != nil {
		return "", err
	}
	if reason := wideRootReason(abs); reason != "" {
		return "", errors.New(reason)
	}
	if !fileutil.IsDir(abs) {
		return "", fmt.Errorf("%s 不存在或不是目录", abs)
	}
	return abs, nil
}

// projectsUnreadableError is the refusal upsert returns while projects.json cannot be read,
// with the path in it so the user can go and look.
func projectsUnreadableError(path string) error {
	return fmt.Errorf("projects.json 无法读取或解析(%s):已拒绝写入,以免覆盖还能修复的文件。请修复或删除它后重试", path)
}

// usableProjectRoots validates a project's root set with the SESSION's rules and returns
// the stored (absolute, cleaned) form. It is the WRITE half — the paths a user just typed —
// so nothing is relaxed relative to a session's own roots: a bad root here would be
// inherited by every member session at once. Reads go through resolveProjectPrimary
// instead, which is the same primary rule; the additional roots are where the two halves
// legitimately differ (a dead one is refused on the way in, tolerated on the way out).
func usableProjectRoots(primary string, additional []string) (string, []string, error) {
	abs, err := resolveProjectPrimary(primary)
	if err != nil {
		return "", nil, err
	}
	extra, err := normalizedAdditionalRoots(abs, additional)
	if err != nil {
		return "", nil, err
	}
	return abs, extra, nil
}

// normalizedAdditionalRoots applies the shared additional-root rules: validateRootPaths
// (whitespace / existence / wide roots) then agent.NormalizeAdditionalRoots, which drops
// the primary itself along with duplicate and nested entries. An empty list, or one that is
// entirely drops, yields no roots and no error.
func normalizedAdditionalRoots(primary string, dirs []string) ([]string, error) {
	if len(dirs) == 0 {
		return nil, nil
	}
	abs, err := validateRootPaths(dirs)
	if err != nil {
		return nil, err
	}
	return agent.NormalizeAdditionalRoots(primary, abs)
}

// projectRootsForSession is the resolver shared with the agent side (the checkpoint's
// RootFunc): "which trees do this session's tools and snapshots cover?" — the project's
// roots when one owns the session and still drives it, otherwise the record's own snapshot
// (reported as false, so the caller falls back to the record it already has).
//
// The primary comes back cleaned/expanded, because projects.json is hand-editable and the
// stored string may be a "~/" path: this value ends up as bash's cwd.
func (t *projectTable) projectRootsForSession(sess *session.Session) (string, []string, bool) {
	if sess == nil || sess.ProjectID == "" {
		return "", nil, false
	}
	p, primary, ok := t.drives(sess.ProjectID)
	if !ok {
		return "", nil, false
	}
	return primary, append([]string(nil), p.AdditionalDirs...), true
}
