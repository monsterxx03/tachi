// Package checkpoint records per-turn file snapshots for a session, so a rewind
// can put both the workspace AND the conversation back to the start of a turn.
//
// The workspace half is a private, bare git repository per session root. It is
// not a git worktree and it never touches the user's repository: GIT_DIR lives
// under the session directory, the user's own directory is passed as the work
// tree, and only content-addressed blobs are stored. That is what makes the
// coverage honest — the snapshot sees the tree, so a file written by a Bash
// command is covered exactly like one written by EditFile (the gap Claude Code's
// checkpoints explicitly document that they do not close).
//
// Recording is LAZY: a caller records the turn's boundary for free (it is a
// number) and runs Ensure right before the first tool call that could write. A
// turn that only reads therefore costs nothing, and the expensive first
// snapshot of a session (measured: 1.4s per 2k files, 16.6s per 20k) is paid
// only by a turn that actually writes.
//
// Because snapshots are lazy, a turn that wrote nothing has a boundary but no
// file state — and the state at the START of such a turn is the state at the
// start of the next turn that did write. Rewinding therefore resolves to the
// earliest snapshot at or after the requested turn (see resolveTarget); turns
// that only read share their predecessor's file state, which is exactly right
// because nothing changed during them.
package checkpoint

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/pkg/logger"
)

const (
	// Guard defaults. These are safety valves rather than tuned limits: a
	// repository with more covered files than this, or a single changed file
	// bigger than this, is refused rather than allowed to make every turn slow
	// or the snapshot store enormous. Both are provisional until P1 has run
	// against real repositories (see the design doc §5.6, "未测").
	DefaultMaxFiles = 50_000
	DefaultMaxBytes = 32 << 20
	// DefaultRetain mirrors Claude Code's own ceiling of 100 checkpoints per
	// session, so the store stays bounded without the user thinking about it.
	DefaultRetain = 100

	// dirName is the subdirectory of a session's own directory. It sits beside
	// subagent/ and oneoff/ for the same reason: what a session produces lives
	// under that session.
	dirName = "checkpoints"
	// refPrefix/<root-index>/<turn>: one ref per root per turn, so pruning one
	// root cannot take another root's history with it.
	refPrefix = "refs/tachi"
)

// Options tunes a Manager. Zero values mean the defaults above.
type Options struct {
	MaxFiles int
	MaxBytes int64
	Retain   int
	Logger   *logger.Logger
}

func (o Options) maxFiles() int {
	if o.MaxFiles > 0 {
		return o.MaxFiles
	}
	return DefaultMaxFiles
}

func (o Options) maxBytes() int64 {
	if o.MaxBytes > 0 {
		return o.MaxBytes
	}
	return DefaultMaxBytes
}

func (o Options) retain() int {
	if o.Retain > 0 {
		return o.Retain
	}
	return DefaultRetain
}

// Manager records and restores the file state of one session's roots.
//
// The mutex guards the store against concurrent commands (channel mode runs
// several agents in one process). It does NOT serialise against a running turn:
// a caller must refuse a rewind while one is in flight, because truncating the
// conversation under a live turn is the one thing that cannot be made safe here.
type Manager struct {
	dir   string
	roots []string
	opts  Options
	noGit bool // git is not installed: the file half is unavailable
	mu    sync.Mutex
}

// NewManager binds a manager to a session directory. It does no filesystem work
// — nothing is created until the first snapshot actually needs it.
func NewManager(sessionDir string, roots []string, opts Options) *Manager {
	if opts.Logger == nil {
		opts.Logger = logger.New("checkpoint")
	}
	_, lookErr := exec.LookPath("git")
	return &Manager{
		dir:   filepath.Join(sessionDir, dirName),
		roots: normalizeRoots(roots),
		opts:  opts,
		noGit: lookErr != nil,
	}
}

// Roots returns the roots this manager covers, in the order it covers them.
func (m *Manager) Roots() []string { return append([]string(nil), m.roots...) }

// Begin records where a turn started. It is deliberately cheap — a manifest
// entry, not a snapshot — so a caller can mark every turn's boundary without
// paying for the workspace. Idempotent: calling it twice for a turn is a
// lookup.
//
// A turn that only reads stops here, and that is the point: its file state is
// the same as the next writing turn's starting state, so nothing is lost (see
// resolveTarget).
func (m *Manager) Begin(ctx context.Context, rec Record) error {
	if rec.Turn <= 0 {
		return fmt.Errorf("checkpoint: turn must be positive, got %d", rec.Turn)
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	man, err := loadManifest(m.dir)
	if err != nil {
		return err
	}
	if _, ok := man.find(rec.Turn); ok {
		return nil
	}
	rec.At = time.Now().UTC()
	rec.Roots, rec.Skipped = nil, ""
	man.Checkpoints = append(man.Checkpoints, rec)
	removed := man.prune(m.opts.retain())
	if err := saveManifest(m.dir, man); err != nil {
		return err
	}
	m.dropRefs(removed)
	return nil
}

// Snapshot takes the file half of a turn's checkpoint if it has not been taken.
//
// This is the lazy execution point: callers run it right before the first tool
// call that could write, and again before every such call (after the first it
// is a manifest lookup). A turn that only reads never calls it, so it never
// pays for walking the tree — and the expensive first snapshot of a session is
// only paid by a turn that actually writes.
//
// The snapshot captures the state at THIS moment, which is exactly "the start
// of the turn" because it runs before the first change. A guard that trips, or
// a machine without git, does not fail the turn: the reason is recorded and a
// later rewind reports that no files were restored. Any other failure IS
// returned, because the caller's contract is to block the write it was about to
// make rather than let it happen unrecorded.
func (m *Manager) Snapshot(ctx context.Context, turn int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	man, err := loadManifest(m.dir)
	if err != nil {
		return err
	}
	idx, rec, ok := man.findIndex(turn)
	if !ok {
		return fmt.Errorf("checkpoint: turn %d was never begun", turn)
	}
	if len(rec.Roots) > 0 || rec.Skipped != "" {
		return nil
	}
	switch {
	case m.noGit:
		rec.Skipped = "git is not installed"
	case len(m.roots) == 0:
		rec.Skipped = "no workspace root"
	default:
		states, reason := m.snapshotRoots(ctx, man, turn)
		rec.Roots, rec.Skipped = states, reason
		if reason != "" {
			m.opts.Logger.Warn(ctx, "checkpoint: file snapshot skipped", "turn", turn, "reason", reason)
		}
	}
	man.Checkpoints[idx] = rec
	return saveManifest(m.dir, man)
}

// snapshotRoots snapshots every root. The second return value is the reason the
// file half was refused, or "" when it was taken.
func (m *Manager) snapshotRoots(ctx context.Context, man *Manifest, turn int) ([]RootState, string) {
	states := make([]RootState, 0, len(m.roots))
	for i, root := range m.roots {
		r := m.repoAt(i)
		if err := r.init(ctx); err != nil {
			return nil, "init: " + err.Error()
		}
		if reason := m.guard(ctx, r); reason != "" {
			return nil, reason
		}
		tree, err := r.snapshot(ctx)
		if err != nil {
			return nil, err.Error()
		}
		ref := refName(i, turn)
		parent := m.parentRef(man, i, turn)
		if err := r.commitTree(ctx, ref, tree, parent, fmt.Sprintf("turn %d", turn)); err != nil {
			return nil, err.Error()
		}
		if parent == "" {
			// The first snapshot of a root pays for every file, and every loose
			// object costs a filesystem block; packing right away reclaims the
			// waste it just created (measured: 20k small files => 80MB of loose
			// objects for ~1MB of content).
			if err := r.gc(ctx); err != nil {
				m.opts.Logger.Warn(ctx, "checkpoint: gc after cold snapshot failed", "root", root, "err", err)
			}
		}
		states = append(states, RootState{RootIndex: i, Root: root, Ref: ref, Tree: tree})
	}
	return states, ""
}

// guard applies the count and size limits BEFORE any object is written: after
// `add -A` the cost these guards exist to prevent has already been paid.
func (m *Manager) guard(ctx context.Context, r repo) string {
	count, err := r.trackedFileCount(ctx)
	if err != nil {
		return "count files: " + err.Error()
	}
	if count > m.opts.maxFiles() {
		return fmt.Sprintf("%d files exceed the %d file limit", count, m.opts.maxFiles())
	}
	candidates, err := r.candidateFiles(ctx)
	if err != nil {
		return "list changed files: " + err.Error()
	}
	for _, rel := range candidates {
		info, err := os.Stat(filepath.Join(r.root, rel))
		if err != nil {
			continue // deleted between the listing and the stat
		}
		if !info.IsDir() && info.Size() > m.opts.maxBytes() {
			return fmt.Sprintf("%s is %d bytes, over the %d byte limit", rel, info.Size(), m.opts.maxBytes())
		}
	}
	return ""
}

// parentRef finds the commit the new checkpoint should chain to: the newest
// earlier turn that has a snapshot for this root. Turns are not contiguous —
// turns that only read record no snapshot — so "the previous turn" is wrong.
func (m *Manager) parentRef(man *Manifest, rootIndex, turn int) string {
	for i := len(man.Checkpoints) - 1; i >= 0; i-- {
		rec := man.Checkpoints[i]
		if rec.Turn >= turn {
			continue
		}
		for _, rs := range rec.Roots {
			if rs.RootIndex == rootIndex && rs.Tree != "" {
				return refName(rootIndex, rec.Turn)
			}
		}
	}
	return ""
}

// dropRefs deletes the refs of pruned checkpoints and repacks once per root, so
// a long session's store does not grow without bound.
func (m *Manager) dropRefs(removed []Record) {
	if len(removed) == 0 {
		return
	}
	ctx := context.Background()
	touched := map[int]bool{}
	for _, rec := range removed {
		for _, rs := range rec.Roots {
			if err := m.repoAt(rs.RootIndex).run(ctx, "update-ref", "-d", rs.Ref); err != nil {
				m.opts.Logger.Warn(ctx, "checkpoint: dropping ref failed", "ref", rs.Ref, "err", err)
				continue
			}
			touched[rs.RootIndex] = true
		}
	}
	for idx := range touched {
		if err := m.repoAt(idx).gc(ctx); err != nil {
			m.opts.Logger.Warn(ctx, "checkpoint: gc after prune failed", "root", m.roots[idx], "err", err)
		}
	}
}

// resolveTarget returns the checkpoint that carries the file state a rewind to
// turn needs, or the reason there is none.
//
// The subtlety lazy snapshots create: a turn that wrote nothing has a boundary
// but no snapshot of its own, and ITS starting state is the next writing turn's
// starting state — so resolving forward is correct. But that reasoning only
// holds while the turns in between are known to have written nothing, which is
// what "no Roots and no Skipped" means. A turn whose snapshot was refused (a
// guard tripped) or pruned is UNKNOWN, and a later snapshot is not a stand-in
// for it: the files would silently come back to the wrong point in time.
func (m *Manager) resolveTarget(man *Manifest, turn int) (Record, bool, string) {
	rec, ok := man.find(turn)
	if !ok {
		return Record{}, false, fmt.Sprintf("turn %d is not checkpointed", turn)
	}
	if len(rec.Roots) > 0 {
		return rec, true, ""
	}
	if rec.Skipped != "" {
		return Record{}, false, rec.Skipped
	}
	for _, later := range man.Checkpoints {
		if later.Turn <= turn {
			continue
		}
		if later.Skipped != "" {
			return Record{}, false, later.Skipped
		}
		if len(later.Roots) > 0 {
			return later, true, ""
		}
	}
	return Record{}, false, "nothing wrote during or after this turn, so the files are already as they were"
}

// repoAt builds the repo handle for a root index.
func (m *Manager) repoAt(index int) repo {
	return repo{
		dir:  filepath.Join(m.dir, fmt.Sprintf("root-%02d", index), "repo.git"),
		root: m.roots[index],
	}
}

// normalizeRoots makes the root set absolute, deduplicated, and free of roots
// nested inside another one — a nested root would be snapshotted twice, once as
// its own tree and once as part of the parent.
func normalizeRoots(roots []string) []string {
	seen := make(map[string]bool, len(roots))
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		if strings.TrimSpace(r) == "" {
			continue
		}
		abs, err := filepath.Abs(r)
		if err != nil {
			continue
		}
		abs = filepath.Clean(abs)
		if seen[abs] {
			continue
		}
		seen[abs] = true
		out = append(out, abs)
	}
	sort.Strings(out)
	kept := make([]string, 0, len(out))
	for i, r := range out {
		if i > 0 && strings.HasPrefix(r, out[i-1]+string(filepath.Separator)) {
			continue
		}
		kept = append(kept, r)
	}
	return kept
}

// refName is the ref holding the checkpoint of a turn for a root.
func refName(rootIndex, turn int) string {
	return fmt.Sprintf("%s/%02d/%d", refPrefix, rootIndex, turn)
}
