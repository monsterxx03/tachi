package checkpoint

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// Preview is what a rewind would do to the workspace, computed before anything
// is written. It exists because a rewind DELETES files as well as restoring
// them: "changes were rewound" is not something a reader should discover
// afterwards.
type Preview struct {
	// Turn is what the caller asked for; Target is the checkpoint that carries
	// the file state for it. They differ when the requested turn wrote nothing
	// (see resolveTarget) — a turn that only reads has no snapshot of its own,
	// and the state at its start is the next writing turn's starting state.
	Turn   int           `json:"turn"`
	Target int           `json:"target"`
	Roots  []RootPreview `json:"roots,omitempty"`
	// State says what the workspace side of this rewind is worth: files to
	// restore, files already right, or files nobody recorded. It is what lets a
	// caller say "未还原任何文件" out loud instead of leaving the reader to
	// assume the workspace moved with the conversation.
	State FileState `json:"state"`
	// Skipped is the technical reason State is unhelpful — a guard's wording,
	// "git is not installed", or a recorded workspace that is no longer there —
	// for a log line and a diagnosis. It is empty for FilesUnchanged, which needs
	// no reason: nothing was wrong.
	Skipped string `json:"skipped,omitempty"`
	// RootMismatch, when set, says the files this rewind would restore belong to a
	// DIFFERENT workspace than the session has now: the checkpoint's roots are not
	// the current ones (a folder change, an added or removed additional root, a
	// desktop project edit, a git worktree). The rewind is still the right one for
	// those turns — it puts each recorded tree back — but the reader has to know
	// that the directory on screen is not the one being written.
	RootMismatch string `json:"root_mismatch,omitempty"`
	// Irreversible lists side effects inside the rewind's span that it cannot take
	// back. The git one is DETECTED (see committedSince): the roots' own HEAD,
	// recorded by the checkpoints, compared against now. The rest of §6's list —
	// MCP calls, SendFile, Cron, memory writes, background processes, anything
	// already pushed to an external service — is not detected yet.
	Irreversible []string `json:"irreversible,omitempty"`
}

// Empty reports whether the preview would change no FILES — the question the
// tests ask to pin "the rewind did what it said". It says nothing about State:
// a preview with no roots is empty whether that is because nothing was recorded
// or because nothing needed to be.
func (p Preview) Empty() bool {
	for _, r := range p.Roots {
		if len(r.Added)+len(r.Changed)+len(r.Deleted) > 0 {
			return false
		}
	}
	return true
}

// RootPreview is one root's part of a preview.
type RootPreview struct {
	Root string `json:"root"`
	// Added are files that exist now and would be DELETED by the rewind,
	// Changed ones whose content would be restored, Deleted ones that would come
	// back. Added is called out separately because it is the destructive half.
	Added   []string `json:"added,omitempty"`
	Changed []string `json:"changed,omitempty"`
	Deleted []string `json:"deleted,omitempty"`
	// Stat is git's own diffstat, for display.
	Stat string `json:"stat,omitempty"`
}

// Restore is what a rewind did.
type Restore struct {
	Turn   int           `json:"turn"`
	Target int           `json:"target"`
	Roots  []RootRestore `json:"roots,omitempty"`
	// State is the resolveTarget outcome the restore acted on, carried out so a
	// caller can tell "restored" from "nothing to do" from "not recorded"
	// without re-reading the manifest.
	State   FileState `json:"state"`
	Skipped string    `json:"skipped,omitempty"`
}

// RootRestore is one root's part of a restore.
type RootRestore struct {
	Root     string   `json:"root"`
	Restored []string `json:"restored,omitempty"`
	Removed  []string `json:"removed,omitempty"`
}

// Preview computes what rewinding to a turn would change, without changing it.
func (m *Manager) Preview(ctx context.Context, turn int) (Preview, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.preview(ctx, turn)
}

func (m *Manager) preview(ctx context.Context, turn int) (Preview, error) {
	man, err := loadManifest(m.dir)
	if err != nil {
		return Preview{}, err
	}
	target, state, reason := m.resolveTarget(man, turn)
	// The irreversible half is computed from what the checkpoints recorded about
	// the user's own repositories, so it is available for every outcome — including
	// the ones that restore no file at all (a commit is still not undone).
	irreversible := m.committedSince(ctx, man, turn)
	if state != FilesAvailable {
		// No checkout is attempted: the workspace is either already right
		// (FilesUnchanged) or unknown (FilesUnknown, where the reason travels
		// with the answer so the caller can say so).
		return Preview{Turn: turn, State: state, Skipped: reason, Irreversible: irreversible}, nil
	}
	p := Preview{Turn: turn, Target: target.Turn, State: state, Irreversible: irreversible}
	// The tree this rewind will write to is the one the CHECKPOINT recorded, not
	// the one the session has now (see repoFor). They are usually the same — and
	// when they are not, the card has to say so: "the workspace went back" would
	// otherwise read as "the files I am looking at moved", while the directory on
	// screen is not the one being written.
	p.RootMismatch = rootMismatch(m.roots, target.Roots)
	for _, rs := range target.Roots {
		r := m.repoFor(rs)
		// The current state has to become a tree before it can be compared: git
		// cannot see untracked files in a diff against a tree, and a file the
		// agent created is exactly what a reader needs to be warned about. The
		// objects written here are the ones the next snapshot would write anyway.
		cur, err := r.snapshot(ctx)
		if err != nil {
			return Preview{}, err
		}
		changes, err := r.changes(ctx, rs.Tree, cur)
		if err != nil {
			return Preview{}, err
		}
		rp := RootPreview{Root: rs.Root}
		for _, c := range parseChanges(changes) {
			switch c.Status {
			case statusAdded:
				rp.Added = append(rp.Added, c.Path)
			case statusDeleted:
				rp.Deleted = append(rp.Deleted, c.Path)
			default:
				rp.Changed = append(rp.Changed, c.Path)
			}
		}
		if rp.Stat, err = r.stat(ctx, rs.Tree, cur); err != nil {
			return Preview{}, err
		}
		p.Roots = append(p.Roots, rp)
	}
	return p, nil
}

// Restore puts the workspace back to the state a rewind to turn requires.
//
// It writes only the paths that actually differ (a full checkout would refresh
// every mtime and make the next incremental build rebuild the world), and it
// removes the files that were created after the checkpoint — the destructive
// half, which is why a caller shows Preview first.
//
// The removals run FIRST, and that order is load-bearing rather than cosmetic:
// when a path changed shape (a file became a directory of the same name, say —
// `foo.ts` -> `foo/index.ts`), the diff lists the new shape as added and the old
// one as deleted, and the checkout of the old shape cannot be written while the
// new one still occupies the name. git resolves the conflict by deleting the
// obstacle, which then makes the second half fail on a path that is no longer
// there in that shape — an error AFTER the workspace has already moved. Taking
// the added files out first removes the obstacle instead of colliding with it.
//
// It does NOT touch the conversation: truncating messages.jsonl and
// api_requests.jsonl is the session store's job, and the two must happen
// together so the model's belief and the disk never disagree.
func (m *Manager) Restore(ctx context.Context, turn int) (Restore, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	man, err := loadManifest(m.dir)
	if err != nil {
		return Restore{}, err
	}
	target, state, reason := m.resolveTarget(man, turn)
	if state != FilesAvailable {
		return Restore{Turn: turn, State: state, Skipped: reason}, nil
	}

	out := Restore{Turn: turn, Target: target.Turn, State: state}
	for _, rs := range target.Roots {
		r := m.repoFor(rs)
		cur, err := r.snapshot(ctx)
		if err != nil {
			return out, err
		}
		changes, err := r.changes(ctx, rs.Tree, cur)
		if err != nil {
			return out, err
		}
		var restore, remove []string
		for _, c := range parseChanges(changes) {
			if c.Status == statusAdded {
				remove = append(remove, c.Path)
				continue
			}
			restore = append(restore, c.Path)
		}

		// Destructive half first, so a path that changed shape is out of the way
		// of the write below (see the doc comment).
		if err := removePaths(r.root, remove); err != nil {
			return out, err
		}
		// The index is pointed at the checkpoint next, so checkout-index writes
		// the recorded content rather than whatever the index happened to hold.
		if len(restore) > 0 {
			if err := r.setIndex(ctx, rs.Tree); err != nil {
				return out, err
			}
			if err := r.writePaths(ctx, restore); err != nil {
				return out, err
			}
		}
		rr := RootRestore{Root: rs.Root, Removed: remove}
		if len(restore) > 0 {
			sort.Strings(restore)
			rr.Restored = restore
		}
		out.Roots = append(out.Roots, rr)
	}
	return out, nil
}

// committedSince lists the roots whose own git HEAD moved since the turn: the one
// side effect a rewind CANNOT undo. The files come back; the commits stay in the
// log. A reader told "changes were rewound" therefore has to be told this in the
// same breath (design §6), and it is the one item on that list that can be
// DETECTED rather than merely declared.
//
// It compares against the HEAD the checkpoints recorded, so it needs no per-turn
// work: the value is taken when a turn's file half is snapshotted anyway.
func (m *Manager) committedSince(ctx context.Context, man *Manifest, turn int) []string {
	if _, ok := man.find(turn); !ok {
		return nil // no checkpoint, so nothing recorded a HEAD to compare against
	}
	var out []string
	for _, st := range recordedHeads(man, turn) {
		// Only a NEW commit is worth saying. A root whose repository is gone (the
		// directory was moved or deleted) reports no HEAD at all, and "HEAD moved to
		// nothing" describes the disappearance rather than a commit — the rewind
		// will fail on that root by itself, with the real reason.
		if now := m.repoFor(st).userHead(ctx); now != "" && now != st.Head {
			out = append(out, fmt.Sprintf("git commit 在 %s（HEAD %s → %s）", st.Root, shortHead(st.Head), shortHead(now)))
		}
	}
	return out
}

// recordedHeads returns, per root PATH, the newest HEAD the checkpoints recorded at
// or before turn.
//
// Keyed by path rather than by index, because a path is what a repository IS: the
// same index can name two different directories over a session's life (a folder
// change, an added root), and comparing the wrong pair would either miss a real
// commit or invent one.
//
// A turn that wrote nothing has no state of its own, and the value that describes
// it is the newest earlier one: a commit needs Bash, a turn that runs Bash
// snapshots, so nothing can have committed in between without leaving a record.
func recordedHeads(man *Manifest, turn int) []RootState {
	seen := map[string]bool{}
	var out []RootState
	for i := len(man.Checkpoints) - 1; i >= 0; i-- {
		rec := man.Checkpoints[i]
		if rec.Turn > turn {
			continue
		}
		for _, rs := range rec.Roots {
			if rs.Head == "" || rs.Root == "" || seen[rs.Root] {
				continue
			}
			seen[rs.Root] = true
			out = append(out, rs)
		}
	}
	return out
}

// rootMismatch describes the difference between the roots a checkpoint covers and
// the session's current ones, as one sentence for a card, or "" when they are the
// same set.
//
// It is compared as a SET: order is not meaningful to a reader ("the workspace is
// these directories"), and the root set is sorted anyway.
func rootMismatch(current []string, recorded []RootState) string {
	was := make([]string, 0, len(recorded))
	for _, rs := range recorded {
		if rs.Root != "" {
			was = append(was, rs.Root)
		}
	}
	sort.Strings(was)
	now := append([]string(nil), current...)
	sort.Strings(now)
	if slices.Equal(was, now) {
		return ""
	}
	return fmt.Sprintf("这一轮记录的工作区是 %s；当前会话的工作区是 %s",
		strings.Join(was, "、"), strings.Join(now, "、"))
}

// shortHead trims a commit hash to the part a reader compares while looking at two
// of them side by side.
func shortHead(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

// change is one entry of git diff --name-status.
type change struct {
	Status string
	Path   string
}

// Git's status letters, as far as a rewind cares: anything that is not A or D
// (M, T, C, ...) is "restore the recorded content".
const (
	statusAdded   = "A"
	statusDeleted = "D"
)

// parseChanges reads `git diff --name-status -z` output: alternating status and
// path fields, NUL-terminated. NUL separation is what makes it safe for paths
// with tabs, quotes, or non-UTF-8 bytes in them.
func parseChanges(out []byte) []change {
	fields := strings.Split(string(out), "\x00")
	var changes []change
	for i := 0; i+1 < len(fields); i += 2 {
		if fields[i] == "" {
			break
		}
		changes = append(changes, change{Status: fields[i], Path: fields[i+1]})
	}
	return changes
}

// removePaths deletes files a rewind is taking back, then prunes directories
// they leave empty — a rewind that leaves an empty dist/ behind is not what
// "back to that point" means.
func removePaths(root string, paths []string) error {
	for _, rel := range paths {
		abs := filepath.Join(root, rel)
		if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", abs, err)
		}
		pruneEmptyParents(root, filepath.Dir(abs))
	}
	return nil
}

// pruneEmptyParents walks up from dir removing empty directories, stopping at
// root (which is never removed) or at the first directory that still has
// something in it.
//
// The prefix is compared WITH the separator: "/a/bc" merely starts with "/a/b",
// and this loop deletes what it walks over, so a bare HasPrefix would let a
// sibling directory be removed.
func pruneEmptyParents(root, dir string) {
	for dir != root && strings.HasPrefix(dir, root+string(filepath.Separator)) {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}
