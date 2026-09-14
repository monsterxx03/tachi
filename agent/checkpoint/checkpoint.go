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
	// int64 because it is compared against os.FileInfo.Size().
	DefaultMaxBytes int64 = 32 << 20
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

// Boundary is where a turn started, as far as the session's own history is
// concerned. The turn number is NOT part of it: the manager assigns it from the
// manifest, the same way the request Seq is derived from disk rather than kept
// in memory, so numbering survives a restart and needs no session-wide counter.
type Boundary struct {
	// Records and APIRecords are the lengths of messages.jsonl and
	// api_requests.jsonl at this point. Rewinding truncates BOTH (the request
	// log has to describe the conversation that is still there) but not the
	// usage ledger: tokens were really spent, and a rewind is not a refund.
	Records    int
	APIRecords int
	// UserText is the prompt that started the turn, so a rewind can put it back
	// in the input box (what both Claude Code and Pi do) instead of making the
	// reader retype it.
	UserText string
}

// Begin records where a turn started and returns its number. It is deliberately
// cheap — a manifest entry, not a snapshot — so a caller can mark every turn's
// boundary without paying for the workspace. Call it once per turn, at the turn
// start.
//
// A turn that only reads stops here, and that is the point: its file state is
// the same as the next writing turn's starting state, so nothing is lost (see
// resolveTarget).
func (m *Manager) Begin(ctx context.Context, b Boundary) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	man, err := loadManifest(m.dir)
	if err != nil {
		return 0, err
	}
	turn := 1
	if n := len(man.Checkpoints); n > 0 {
		turn = man.Checkpoints[n-1].Turn + 1
	}
	man.Checkpoints = append(man.Checkpoints, Record{
		Turn:       turn,
		Records:    b.Records,
		APIRecords: b.APIRecords,
		At:         time.Now().UTC(),
		UserText:   b.UserText,
	})
	removed := man.prune(m.opts.retain())
	if err := saveManifest(m.dir, man); err != nil {
		return 0, err
	}
	m.dropRefs(removed)
	return turn, nil
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
		rec.Skipped = "没有安装 git"
	case len(m.roots) == 0:
		rec.Skipped = "这个会话没有工作目录"
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

// SnapshotEnd records where the turn's writes LEFT the workspace, and with it the turn's
// own change summary.
//
// It is the second half of a turn's file state. `Begin` + `Snapshot` capture where the turn
// started — what a rewind needs. This captures where it ended, which is what makes the
// turn's changes READABLE: `git diff <start> <end>` is exactly what this turn did, and it
// cannot be moved by a later turn, a later commit, or the user editing the file again. The
// alternative — inferring the change from what the tool calls said they were about to do —
// cannot see a shell command at all, and has to guess at created and deleted files.
//
// Only a turn that HAS a start state gets one: a turn that wrote nothing has nothing to
// compare, and snapshotting anyway would make every read-only turn pay for a traversal —
// the cost the lazy design exists to avoid.
//
// A failure here does NOT fail the turn, unlike the start snapshot: the work is already
// done, so there is nothing left to refuse. The reason is recorded (Record.Diff.Skipped)
// and every reader falls back to what the tool calls declared.
func (m *Manager) SnapshotEnd(ctx context.Context, turn int) error {
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
	if rec.Diff != nil || rec.Skipped != "" || len(rec.Roots) == 0 {
		return nil // already ended, unknown state, or nothing was written
	}

	reason := m.endSnapshotRoots(ctx, &rec, turn)
	if reason != "" {
		// All or nothing, exactly like the start snapshot: half a set of end states would
		// make the numbers look complete while covering only some roots. But only the END
		// refs this call created are released: the START refs are this turn's rewind point,
		// and a refused END must not take that with it. Dropping a start ref is not a
		// tidiness problem — parentRef NAMES it when chaining the next writing turn's
		// snapshot, so `commit-tree -p <deleted ref>` fails, that turn records no state
		// either, and every turn after it fails the same way: one refused end switches the
		// rest of the session's checkpoints off.
		var ended []RootState
		for i := range rec.Roots {
			if rec.Roots[i].EndRef != "" {
				ended = append(ended, RootState{RootIndex: rec.Roots[i].RootIndex, EndRef: rec.Roots[i].EndRef})
			}
			rec.Roots[i].EndRef, rec.Roots[i].EndTree = "", ""
		}
		m.dropStates(ended)
		rec.Diff = &TurnDiff{Skipped: reason}
		man.Checkpoints[idx] = rec
		m.opts.Logger.Warn(ctx, "checkpoint: end-of-turn snapshot skipped", "turn", turn, "reason", reason)
		return saveManifest(m.dir, man)
	}

	diff, statErr := m.diffStat(ctx, rec)
	if statErr != nil {
		// The trees are there and stay useful (the panel and the review read them); only
		// the counting failed, so the numbers are the missing half.
		m.opts.Logger.Warn(ctx, "checkpoint: turn diff stat failed", "turn", turn, "err", statErr)
		diff = &TurnDiff{Skipped: "统计本轮改动失败: " + statErr.Error()}
	}
	rec.Diff = diff
	man.Checkpoints[idx] = rec
	return saveManifest(m.dir, man)
}

// endSnapshotRoots takes the end state of every root the turn's start recorded, filling
// EndRef/EndTree in place. The second return value is the reason it could not, or "".
func (m *Manager) endSnapshotRoots(ctx context.Context, rec *Record, turn int) string {
	for i := range rec.Roots {
		rs := &rec.Roots[i]
		r := m.repoAt(rs.RootIndex)
		// The same guards as the start snapshot, and here they matter for a different
		// reason: this is the first moment a large thing the TURN CREATED is visible, and
		// storing it is exactly what the byte guard exists to refuse.
		if reason := m.guard(ctx, r); reason != "" {
			return reason
		}
		tree, err := r.snapshot(ctx)
		if err != nil {
			return err.Error()
		}
		ref := endRefName(rs.RootIndex, turn)
		if err := r.commitTree(ctx, ref, tree, rs.Ref, fmt.Sprintf("turn %d end", turn)); err != nil {
			return err.Error()
		}
		rs.EndRef, rs.EndTree = ref, tree
	}
	return ""
}

// diffStat counts a turn's changes from its two trees, per root.
func (m *Manager) diffStat(ctx context.Context, rec Record) (*TurnDiff, error) {
	out := &TurnDiff{}
	for _, rs := range rec.Roots {
		if rs.Tree == "" || rs.EndTree == "" {
			continue
		}
		files, added, removed, err := m.repoAt(rs.RootIndex).numstat(ctx, rs.Tree, rs.EndTree)
		if err != nil {
			return nil, err
		}
		out.Files += files
		out.Added += added
		out.Removed += removed
	}
	return out, nil
}

// TurnDiff returns the unified diff of a turn's own two trees, and whether there is a
// pair to diff at all (false for a turn that wrote nothing, or whose end state is
// missing — the caller then falls back to whatever the tool calls declared).
//
// Both sides are recorded trees, so the answer does not change when the worktree does:
// asking an hour later, after a commit, gives the same diff this turn produced.
func (m *Manager) TurnDiff(ctx context.Context, turn int) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	man, err := loadManifest(m.dir)
	if err != nil {
		return "", false, err
	}
	rec, ok := man.find(turn)
	if !ok {
		return "", false, nil
	}
	var b strings.Builder
	for _, rs := range rec.Roots {
		if rs.Tree == "" || rs.EndTree == "" {
			continue
		}
		text, err := m.repoAt(rs.RootIndex).diffText(ctx, rs.Tree, rs.EndTree)
		if err != nil {
			return "", false, err
		}
		b.WriteString(text)
	}
	if b.Len() == 0 {
		return "", false, nil
	}
	return b.String(), true, nil
}

// TurnDiffCommand is a shell command that prints a turn's diff — for a reader that runs git
// itself rather than being handed text (the review fork). Empty when the turn has no pair
// of trees. Several roots are joined with `&&`: each prints its own diff.
func (m *Manager) TurnDiffCommand(turn int) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	man, err := loadManifest(m.dir)
	if err != nil {
		return ""
	}
	rec, ok := man.find(turn)
	if !ok {
		return ""
	}
	var cmds []string
	for _, rs := range rec.Roots {
		if rs.Tree == "" || rs.EndTree == "" {
			continue
		}
		r := m.repoAt(rs.RootIndex)
		cmds = append(cmds, fmt.Sprintf("git --git-dir=%s --work-tree=%s diff %s %s",
			shellQuote(r.dir), shellQuote(r.root), rs.Tree, rs.EndTree))
	}
	return strings.Join(cmds, " && ")
}

// shellQuote wraps a path so a shell hands it over as ONE argument. The store lives under
// the session directory, and a session directory (or a workspace) can contain spaces.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ChangedSinceTurn reports which paths the working tree no longer has where the turn left
// them, so a frozen diff can say 「这个文件之后又改过」 instead of letting the reader
// believe the panel shows what is on disk right now.
//
// Paths are root-relative and returned as a set: with several roots the same relative path
// can name two different files, and a caller that cares about that distinction has to ask
// per root (nothing in the UI does yet).
func (m *Manager) ChangedSinceTurn(ctx context.Context, turn int) (map[string]bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	man, err := loadManifest(m.dir)
	if err != nil {
		return nil, err
	}
	rec, ok := man.find(turn)
	if !ok {
		return nil, nil
	}
	out := map[string]bool{}
	for _, rs := range rec.Roots {
		if rs.EndTree == "" {
			continue
		}
		paths, err := m.repoAt(rs.RootIndex).treeChangedSince(ctx, rs.EndTree)
		if err != nil {
			return nil, err
		}
		for _, p := range paths {
			out[p] = true
		}
	}
	return out, nil
}

// snapshotRoots snapshots every root. The second return value is the reason the
// file half was refused, or "" when it was taken.
//
// The refusals are written for a READER — they end up on the rewind card as the
// reason no file was restored — so they are Chinese, like the other strings a
// frontend shows. Raw git errors keep their own wording after a Chinese lead, the
// way every other wrapped error in this repo does.
func (m *Manager) snapshotRoots(ctx context.Context, man *Manifest, turn int) ([]RootState, string) {
	states := make([]RootState, 0, len(m.roots))
	// All or nothing: the caller records NO state when any root fails (restoring
	// the roots that worked would silently move the others to the wrong point in
	// time), so the refs this call already created are released with it. Left
	// behind they would keep their objects alive for good, invisible to every
	// other path that prunes, drops or rewinds.
	fail := func(reason string) ([]RootState, string) {
		m.dropStates(states)
		return nil, reason
	}
	for i, root := range m.roots {
		r := m.repoAt(i)
		if err := r.init(ctx); err != nil {
			return fail("初始化检查点仓库失败: " + err.Error())
		}
		if reason := m.guard(ctx, r); reason != "" {
			return fail(reason)
		}
		tree, err := r.snapshot(ctx)
		if err != nil {
			return fail(err.Error())
		}
		ref := refName(i, turn)
		parent := m.parentRef(man, i, turn)
		if err := r.commitTree(ctx, ref, tree, parent, fmt.Sprintf("turn %d", turn)); err != nil {
			return fail(err.Error())
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
		states = append(states, RootState{
			RootIndex: i,
			Root:      root,
			Ref:       ref,
			Tree:      tree,
			Head:      r.userHead(ctx),
		})
	}
	return states, ""
}

// guard applies the count and size limits BEFORE any object is written: after
// `add -A` the cost these guards exist to prevent has already been paid.
func (m *Manager) guard(ctx context.Context, r repo) string {
	count, err := r.trackedFileCount(ctx)
	if err != nil {
		return "统计文件数量失败: " + err.Error()
	}
	if count > m.opts.maxFiles() {
		return fmt.Sprintf("仓库有 %d 个文件，超过上限 %d（agent.checkpoints.max_files）", count, m.opts.maxFiles())
	}
	candidates, err := r.candidateFiles(ctx)
	if err != nil {
		return "列出改动文件失败: " + err.Error()
	}
	for _, rel := range candidates {
		info, err := os.Stat(filepath.Join(r.root, rel))
		if err != nil {
			continue // deleted between the listing and the stat
		}
		if !info.IsDir() && info.Size() > m.opts.maxBytes() {
			return fmt.Sprintf("%s 有 %d 字节，超过单文件上限 %d（agent.checkpoints.max_bytes）",
				rel, info.Size(), m.opts.maxBytes())
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

// DropAfter deletes the checkpoints of every turn AFTER the given one and
// returns how many went.
//
// A rewind cuts the conversation back to the start of turn N, which makes every
// record after N describe a conversation that no longer exists: its Records
// points past the new end of messages.jsonl, and its refs hold a branch that was
// abandoned. Keeping them is not harmless — a rewind to such a turn restores the
// FILES to a point in time that belongs to the discarded branch, while the
// conversation cut is silently a no-op (measured: exactly that, before this
// existed), and the turn numbering runs on past a gap nobody can see.
//
// Turn N itself is kept. Its Records IS the new end of the conversation and its
// snapshot is the state the rewind just restored, so going back to it again
// stays a no-op instead of an error, and the next turn is numbered N+1 — which
// is the numbering a reader expects after "back to turn N".
func (m *Manager) DropAfter(turn int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	man, err := loadManifest(m.dir)
	if err != nil {
		return 0, err
	}
	keep := make([]Record, 0, len(man.Checkpoints))
	var dropped []Record
	for _, rec := range man.Checkpoints {
		if rec.Turn > turn {
			dropped = append(dropped, rec)
			continue
		}
		keep = append(keep, rec)
	}
	if len(dropped) == 0 {
		return 0, nil
	}
	man.Checkpoints = keep
	if err := saveManifest(m.dir, man); err != nil {
		return 0, err
	}
	// The refs of the dropped turns are released with them, and the objects they
	// held become unreachable — the design's "an abandoned branch is
	// disposable" (decision 3). Their sidecar transcripts survive in rewound/.
	m.dropRefs(dropped)
	return len(dropped), nil
}

// dropRefs deletes the refs of dropped checkpoints and repacks once per root, so
// a long session's store does not grow without bound.
func (m *Manager) dropRefs(removed []Record) {
	var states []RootState
	for _, rec := range removed {
		states = append(states, rec.Roots...)
	}
	m.dropStates(states)
}

// dropStates releases the refs of a set of root states and repacks the roots they
// belonged to, so the objects those refs were the only handle on become garbage.
//
// EVERY ref a state carries goes, including both halves of a state that has an end — the
// callers are a checkpoint being pruned/dropped and a snapshot that failed after releasing
// some of its refs, and in both cases the whole state is disposable. A state whose start
// MUST survive therefore arrives here with only EndRef set: that is what a refused END does
// (see SnapshotEnd), and why the caller builds the list rather than passing rec.Roots.
func (m *Manager) dropStates(states []RootState) {
	if len(states) == 0 {
		return
	}
	ctx := context.Background()
	touched := map[int]bool{}
	for _, rs := range states {
		for _, ref := range []string{rs.Ref, rs.EndRef} {
			if ref == "" {
				continue
			}
			if err := m.repoAt(rs.RootIndex).run(ctx, "update-ref", "-d", ref); err != nil {
				m.opts.Logger.Warn(ctx, "checkpoint: dropping ref failed", "ref", ref, "err", err)
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

// FileState is what a rewind knows about the WORKSPACE at the point it is
// returning to. It is the difference between "the files are already right",
// "here is what they were", and "nobody recorded what they were" — three
// outcomes a rewind must report differently, because only the middle one can
// put files back.
type FileState int

const (
	// FilesAvailable: a snapshot carries the tree a rewind must restore.
	FilesAvailable FileState = iota

	// FilesUnchanged: no turn at or after this one wrote anything, so the
	// workspace is already in the state a rewind to it would produce. Nothing
	// to restore and nothing to delete — a fact, not a refusal, and no git work
	// is needed to establish it.
	//
	// This is the state of every turn in a session that never wrote a file, and
	// of the read-only turns at the tail of any session: going back to one of
	// them must still work (the reader wants the CONVERSATION back), which is
	// why it cannot be reported as "unknown".
	FilesUnchanged

	// FilesUnknown: the file state was never recorded (a guard refused the
	// snapshot, git is missing) or is gone (pruned). The conversation half of
	// the checkpoint still exists, so a rewind may proceed — but it must say
	// that no file was restored rather than imply the workspace moved with it.
	FilesUnknown
)

// resolveTarget returns the checkpoint that carries the file state a rewind to
// turn needs, together with what that state is worth.
//
// The subtlety lazy snapshots create: a turn that wrote nothing has a boundary
// but no snapshot of its own, and ITS starting state is the next writing turn's
// starting state — so resolving forward is correct, and FilesUnchanged if there
// is no next writer at all. But that reasoning only holds while the turns in
// between are known to have written nothing, which is what "no Roots and no
// Skipped" means. A turn whose snapshot was refused (a guard tripped) is
// UNKNOWN, and a later snapshot is not a stand-in for it: the files would
// silently come back to the wrong point in time.
func (m *Manager) resolveTarget(man *Manifest, turn int) (Record, FileState, string) {
	rec, ok := man.find(turn)
	if !ok {
		return Record{}, FilesUnknown, fmt.Sprintf("第 %d 轮没有检查点", turn)
	}
	if len(rec.Roots) > 0 {
		return rec, FilesAvailable, ""
	}
	if rec.Skipped != "" {
		return Record{}, FilesUnknown, rec.Skipped
	}
	for _, later := range man.Checkpoints {
		if later.Turn <= turn {
			continue
		}
		if later.Skipped != "" {
			return Record{}, FilesUnknown, later.Skipped
		}
		if len(later.Roots) > 0 {
			return later, FilesAvailable, ""
		}
	}
	return Record{}, FilesUnchanged, ""
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
		if isUnboundedRoot(abs) {
			continue
		}
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

// isUnboundedRoot reports whether a path is too wide to ever be a workspace.
//
// The filesystem root and the user's home directory are the two that actually
// occur (a GUI process's CWD is "/", and a session created before a workspace was
// chosen inherits whatever was around): snapshotting either means walking it on
// every turn, and `git add -A` over "/" does not finish at all. The desktop
// refuses these as workspaces for the same reason (see defaultWorkspaceFor /
// wideRootReason), so this is the same rule at the layer that would hang.
func isUnboundedRoot(path string) bool {
	if path == string(filepath.Separator) {
		return true
	}
	home, err := os.UserHomeDir()
	return err == nil && home != "" && path == filepath.Clean(home)
}

// refName is the ref holding the checkpoint of a turn for a root.
func refName(rootIndex, turn int) string {
	return fmt.Sprintf("%s/%02d/%d", refPrefix, rootIndex, turn)
}

// endRefName is the ref holding where a turn LEFT that root (see SnapshotEnd). It sits
// beside the start's ref so one prune or drop releases both.
func endRefName(rootIndex, turn int) string {
	return fmt.Sprintf("%s/%02d/%d-end", refPrefix, rootIndex, turn)
}

// TurnInfo describes one checkpointed turn, for a picker or a rewind command.
type TurnInfo struct {
	Turn       int       `json:"turn"`
	Records    int       `json:"records"`
	APIRecords int       `json:"api_records"`
	At         time.Time `json:"at"`
	UserText   string    `json:"user_text,omitempty"`
	// NoFiles is set when this turn has no file state (a guard refused it, git
	// is missing, or the turn only read). A rewind to a turn with NoFiles either
	// shares a later writer's snapshot or refuses — see resolveTarget.
	NoFiles bool `json:"no_files,omitempty"`
	// Reason explains NoFiles when the state is genuinely unknown (a skipped
	// snapshot), as opposed to "nothing wrote during it".
	Reason string `json:"reason,omitempty"`
	// Diff is what this turn changed, counted from its own two trees — nil when it
	// wrote nothing, Skipped when the numbers could not be taken. It rides along here
	// because a reader that already has the turn list (the desktop's page builder, a
	// picker) then has the footer's numbers too, without a second call or a git run.
	Diff *TurnDiff `json:"diff,omitempty"`
}

// Turns lists the session's checkpoints, oldest first.
func (m *Manager) Turns() ([]TurnInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	man, err := loadManifest(m.dir)
	if err != nil {
		return nil, err
	}
	out := make([]TurnInfo, 0, len(man.Checkpoints))
	for _, rec := range man.Checkpoints {
		info := TurnInfo{
			Turn:       rec.Turn,
			Records:    rec.Records,
			APIRecords: rec.APIRecords,
			At:         rec.At,
			UserText:   rec.UserText,
			NoFiles:    len(rec.Roots) == 0,
			Reason:     rec.Skipped,
			Diff:       rec.Diff,
		}
		out = append(out, info)
	}
	return out, nil
}

// Record returns the checkpoint recorded for a turn.
func (m *Manager) Record(turn int) (Record, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	man, err := loadManifest(m.dir)
	if err != nil {
		return Record{}, false, err
	}
	rec, ok := man.find(turn)
	return rec, ok, nil
}
