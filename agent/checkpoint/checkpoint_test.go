package checkpoint

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setup gives a manager over a fresh, empty workspace root inside a temp dir.
// Tests never touch a real repository or ~/.tachi.
func setup(t *testing.T, opts Options) (*Manager, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	base := t.TempDir()
	root := filepath.Join(base, "work")
	require.NoError(t, os.MkdirAll(root, 0o755))
	return NewManager(filepath.Join(base, "session"), []string{root}, opts), root
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
	require.NoError(t, os.WriteFile(abs, []byte(content), 0o644))
}

func readFile(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, rel))
	require.NoError(t, err)
	return string(data)
}

func exists(root, rel string) bool {
	_, err := os.Stat(filepath.Join(root, rel))
	return err == nil
}

// beginSnapshot is the caller's normal sequence for a turn that writes: mark the
// boundary, then take the file half lazily. It asserts the turn number the
// manager assigns, so a test that expects turn N fails loudly if Begin drifts.
func beginSnapshot(t *testing.T, m *Manager, wantTurn int) {
	t.Helper()
	ctx := context.Background()
	turn, err := m.Begin(ctx, Boundary{Records: wantTurn, APIRecords: wantTurn})
	require.NoError(t, err)
	require.Equal(t, wantTurn, turn)
	require.NoError(t, m.Snapshot(ctx, turn))
}

// TestSnapshotCoversShellWritesAndRestoreUndoesThem is the point of the whole
// design: the file half must cover changes no tool declared. Everything here is
// done with os calls rather than EditFile/WriteFile, which is exactly what a
// Bash command looks like from the outside — and exactly the coverage Claude
// Code's checkpoints document that they do not provide.
func TestSnapshotCoversShellWritesAndRestoreUndoesThem(t *testing.T) {
	m, root := setup(t, Options{})
	ctx := context.Background()
	write(t, root, "a.txt", "v1")
	write(t, root, "keep.txt", "untouched")
	beginSnapshot(t, m, 1)

	write(t, root, "a.txt", "v2")        // edited
	write(t, root, "new.txt", "created") // created
	require.NoError(t, os.Remove(filepath.Join(root, "keep.txt")))

	// The preview must say what will happen before it happens — a rewind deletes
	// files, and that is not something a reader should discover afterwards.
	p, err := m.Preview(ctx, 1)
	require.NoError(t, err)
	require.Len(t, p.Roots, 1)
	assert.Equal(t, []string{"new.txt"}, p.Roots[0].Added, "files created since must be listed for deletion")
	assert.Equal(t, []string{"a.txt"}, p.Roots[0].Changed)
	assert.Equal(t, []string{"keep.txt"}, p.Roots[0].Deleted)
	assert.NotEmpty(t, p.Roots[0].Stat)
	assert.False(t, p.Empty())

	res, err := m.Restore(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Target)
	assert.Equal(t, "v1", readFile(t, root, "a.txt"))
	assert.Equal(t, "untouched", readFile(t, root, "keep.txt"), "a file deleted after the checkpoint must come back")
	assert.False(t, exists(root, "new.txt"), "a file created after the checkpoint must be removed")

	// And after restoring, the preview is empty: the rewind did what it said.
	p2, err := m.Preview(ctx, 1)
	require.NoError(t, err)
	assert.True(t, p2.Empty(), "nothing should be left to change: %+v", p2.Roots)
}

// TestTurnThatOnlyReadsSharesTheNextWritersState pins the rule that makes lazy
// snapshots safe. A turn that writes nothing has a boundary but no snapshot of
// its own; the state at its start is the state at the start of the next turn
// that did write. Rewinding to either must restore the same content.
func TestTurnThatOnlyReadsSharesTheNextWritersState(t *testing.T) {
	m, root := setup(t, Options{})
	ctx := context.Background()

	write(t, root, "a.txt", "v1")
	beginSnapshot(t, m, 1) // snapshot of the state at the start of turn 1
	write(t, root, "a.txt", "v2")

	// Turn 2 only reads: its boundary is recorded, no snapshot is taken.
	turn2, err := m.Begin(ctx, Boundary{Records: 2, APIRecords: 2})
	require.NoError(t, err)
	require.Equal(t, 2, turn2)

	// Turn 3 writes: its snapshot captures the state at its start, which is also
	// the state at turn 2's start.
	beginSnapshot(t, m, 3)

	// Rewinding to turn 2 must resolve to turn 3's snapshot, and turn 1's write
	// (v1 -> v2) must still be in place.
	p, err := m.Preview(ctx, 2)
	require.NoError(t, err)
	assert.Equal(t, 3, p.Target, "a turn that read only resolves to the next writer's snapshot")
	assert.True(t, p.Empty())

	// Rewinding to turn 1 undoes it.
	if _, err := m.Restore(ctx, 1); err != nil {
		require.NoError(t, err)
	}
	assert.Equal(t, "v1", readFile(t, root, "a.txt"))
}

// TestSnapshotIsIdempotentPerTurn pins the lazy contract: the caller runs
// Snapshot before EVERY tool call that could write, so after the first one it
// must be a no-op — otherwise the state captured would drift forward with each
// call and a rewind would restore the middle of a turn instead of its start.
func TestSnapshotIsIdempotentPerTurn(t *testing.T) {
	m, root := setup(t, Options{})
	ctx := context.Background()
	write(t, root, "a.txt", "v1")
	turn, err := m.Begin(ctx, Boundary{Records: 1})
	require.NoError(t, err)
	require.NoError(t, m.Snapshot(ctx, turn))

	// A later tool call in the same turn changes the file, then asks again.
	write(t, root, "a.txt", "v2")
	require.NoError(t, m.Snapshot(ctx, 1))

	// The recorded state must still be the one from the first call.
	write(t, root, "a.txt", "v3")
	p, err := m.Preview(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, []string{"a.txt"}, p.Roots[0].Changed)
	if _, err := m.Restore(ctx, 1); err != nil {
		require.NoError(t, err)
	}
	assert.Equal(t, "v1", readFile(t, root, "a.txt"), "the snapshot must be the turn's start, not its middle")
}

// TestSnapshotRefusesAFileOverTheByteLimit documents the guard's contract: the
// checkpoint is still recorded (so the conversation can be rewound) but carries
// no file state, and says why.
func TestSnapshotRefusesAFileOverTheByteLimit(t *testing.T) {
	m, root := setup(t, Options{MaxBytes: 16})
	ctx := context.Background()
	write(t, root, "big.bin", strings.Repeat("x", 64))
	turn, err := m.Begin(ctx, Boundary{Records: 1})
	require.NoError(t, err)
	require.NoError(t, m.Snapshot(ctx, turn), "a tripped guard must not fail the turn")

	p, err := m.Preview(ctx, 1)
	require.NoError(t, err)
	assert.Contains(t, p.Skipped, "超过单文件上限")
	assert.Empty(t, p.Roots)

	res, err := m.Restore(ctx, 1)
	require.NoError(t, err)
	assert.NotEmpty(t, res.Skipped)
	assert.Equal(t, strings.Repeat("x", 64), readFile(t, root, "big.bin"), "nothing may be touched")
}

// TestSnapshotRefusesATreeOverTheFileLimit is the count guard, which exists
// because the file count — not the size of a change — is what makes every later
// turn's walk slow.
func TestSnapshotRefusesATreeOverTheFileLimit(t *testing.T) {
	m, root := setup(t, Options{MaxFiles: 3})
	ctx := context.Background()
	for _, name := range []string{"a", "b", "c", "d"} {
		write(t, root, name+".txt", name)
	}
	turn, err := m.Begin(ctx, Boundary{Records: 1})
	require.NoError(t, err)
	require.NoError(t, m.Snapshot(ctx, turn))

	p, err := m.Preview(ctx, 1)
	require.NoError(t, err)
	assert.Contains(t, p.Skipped, "超过上限")
}

// TestIgnoredPathsStayOutsideTheCoverage pins the documented boundary rather
// than leaving it to prose: a path the project's .gitignore excludes is not
// snapshotted, so a rewind neither restores it nor removes it.
func TestIgnoredPathsStayOutsideTheCoverage(t *testing.T) {
	m, root := setup(t, Options{})
	ctx := context.Background()
	write(t, root, ".gitignore", "dist/\n")
	write(t, root, "src/a.txt", "v1")
	beginSnapshot(t, m, 1)

	write(t, root, "src/a.txt", "v2")
	write(t, root, "dist/out.js", "built")

	if _, err := m.Restore(ctx, 1); err != nil {
		require.NoError(t, err)
	}
	assert.Equal(t, "v1", readFile(t, root, "src/a.txt"))
	assert.True(t, exists(root, "dist/out.js"), "an ignored path is outside the coverage, so it survives a rewind")
}

// TestRestoreDoesNotTouchUnchangedFiles pins the mtime rule: rewriting every
// file would make the next incremental build rebuild the world.
func TestRestoreDoesNotTouchUnchangedFiles(t *testing.T) {
	m, root := setup(t, Options{})
	ctx := context.Background()
	write(t, root, "changed.txt", "v1")
	write(t, root, "untouched.txt", "same")
	beginSnapshot(t, m, 1)

	write(t, root, "changed.txt", "v2")
	past := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(root, "untouched.txt"), past, past))

	if _, err := m.Restore(ctx, 1); err != nil {
		require.NoError(t, err)
	}
	info, err := os.Stat(filepath.Join(root, "untouched.txt"))
	require.NoError(t, err)
	assert.WithinDuration(t, past, info.ModTime(), time.Minute, "an unchanged file must not be rewritten")
	assert.Equal(t, "v1", readFile(t, root, "changed.txt"))
}

// TestPruneDropsTheOldestCheckpoint pins the 100-checkpoint ceiling's mechanics:
// the oldest record and its ref go away, and a rewind to it says so instead of
// silently doing nothing.
func TestPruneDropsTheOldestCheckpoint(t *testing.T) {
	m, root := setup(t, Options{Retain: 2})
	ctx := context.Background()

	write(t, root, "a.txt", "v1")
	beginSnapshot(t, m, 1)
	write(t, root, "a.txt", "v2")
	beginSnapshot(t, m, 2)
	write(t, root, "a.txt", "v3")
	beginSnapshot(t, m, 3)

	man, err := loadManifest(m.dir)
	require.NoError(t, err)
	require.Len(t, man.Checkpoints, 2)
	assert.Equal(t, 2, man.Checkpoints[0].Turn)

	p, err := m.Preview(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, FilesUnknown, p.State, "a pruned checkpoint's file state is unknown, not 'nothing to do'")
	assert.Contains(t, p.Skipped, "没有检查点", "…and the reason shown to the reader says which way it is unknown")

	// The surviving checkpoints still work.
	res, err := m.Restore(ctx, 2)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Target)
	assert.Equal(t, "v2", readFile(t, root, "a.txt"))
}

// TestRestoreRefusesAnUnbegunTurn is the other half of that contract: asking for
// something that was never recorded is an error, not a no-op.
func TestRestoreRefusesAnUnbegunTurn(t *testing.T) {
	m, _ := setup(t, Options{})
	write(t, m.roots[0], "a.txt", "v1")
	beginSnapshot(t, m, 1)

	p, err := m.Preview(context.Background(), 7)
	require.NoError(t, err)
	assert.Equal(t, FilesUnknown, p.State)
	assert.Contains(t, p.Skipped, "没有检查点", "…with a reason a rewind command can print as it is")
}

// TestRestoreHandlesAPathThatChangedShape: a path that changed KIND between the
// checkpoint and now (a file became a directory of the same name, or the reverse)
// is a D/F conflict. git resolves it by DELETING the obstacle, so if the removals
// run after the writes, the second half fails on a path that is no longer there in
// that shape — an error raised after the workspace has already moved, and one that
// tells the reader nothing was touched.
func TestRestoreHandlesAPathThatChangedShape(t *testing.T) {
	cases := []struct {
		name string
		// at is the workspace when the checkpoint is taken; now is the workspace a
		// rewind starts from (each removing the old shape first — creating a path
		// where the other kind sits is the very conflict under test); want is what
		// the checkpoint's state must look like afterwards.
		at   func(t *testing.T, root string)
		now  func(t *testing.T, root string)
		want func(t *testing.T, root string) bool
	}{
		{
			name: "文件变成了同名目录",
			at:   func(t *testing.T, root string) { write(t, root, "thing", "was a file") },
			now: func(t *testing.T, root string) {
				require.NoError(t, os.Remove(filepath.Join(root, "thing")))
				write(t, root, "thing/inner.txt", "inner")
			},
			want: func(t *testing.T, root string) bool {
				return !isDir(t, root, "thing") && readFile(t, root, "thing") == "was a file"
			},
		},
		{
			name: "目录变成了同名文件",
			at:   func(t *testing.T, root string) { write(t, root, "thing/inner.txt", "inner") },
			now: func(t *testing.T, root string) {
				require.NoError(t, os.RemoveAll(filepath.Join(root, "thing")))
				write(t, root, "thing", "now a file")
			},
			want: func(t *testing.T, root string) bool {
				return isDir(t, root, "thing") && readFile(t, root, "thing/inner.txt") == "inner"
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, root := setup(t, Options{})
			tc.at(t, root)
			beginSnapshot(t, m, 1)
			tc.now(t, root)

			_, err := m.Restore(context.Background(), 1)
			require.NoError(t, err, "a path that changed shape must not fail the whole rewind")
			assert.True(t, tc.want(t, root), "the checkpoint's shape must be back")
		})
	}
}

func isDir(t *testing.T, root, rel string) bool {
	t.Helper()
	info, err := os.Stat(filepath.Join(root, rel))
	require.NoError(t, err)
	return info.IsDir()
}

// TestPreviewReportsACommitTheRewindCannotUndo is the one item on the design's
// "irreversible" list that can be DETECTED rather than merely declared: the
// snapshot restores files, and a commit the agent made stays in the log. The test
// asserts both halves — the warning appears, and the commit really is still there
// after the rewind.
func TestPreviewReportsACommitTheRewindCannotUndo(t *testing.T) {
	m, root := setup(t, Options{})
	ctx := context.Background()

	git := func(args ...string) string {
		t.Helper()
		base := []string{"-C", root, "-c", "user.name=checkpoint-test", "-c", "user.email=test@example.invalid"}
		out, err := exec.Command("git", append(base, args...)...).CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
		return strings.TrimSpace(string(out))
	}
	git("init", "--quiet", ".")
	write(t, root, "a.txt", "v1")
	git("add", "-A")
	git("commit", "--quiet", "-m", "first")

	// The turn starts HERE, so its snapshot is both the file state and the HEAD
	// the rewind will be measured against.
	beginSnapshot(t, m, 1)

	p, err := m.Preview(ctx, 1)
	require.NoError(t, err)
	assert.Empty(t, p.Irreversible, "nothing has committed since the checkpoint")

	// The work of the turn, committed by the agent — the thing a rewind cannot take
	// back, and the reason the card exists.
	write(t, root, "a.txt", "v2")
	git("add", "-A")
	git("commit", "--quiet", "-m", "agent work")
	after := git("rev-parse", "HEAD")

	p, err = m.Preview(ctx, 1)
	require.NoError(t, err)
	require.Len(t, p.Irreversible, 1)
	assert.Contains(t, p.Irreversible[0], "git commit 在 "+root)
	assert.Contains(t, p.Irreversible[0], "HEAD")

	// The warning is information, not a veto: the rewind runs, the FILE comes back…
	_, err = m.Restore(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, "v1", readFile(t, root, "a.txt"))
	// …and the commit is exactly where it was.
	assert.Equal(t, after, git("rev-parse", "HEAD"))
}

// TestHeadDriftNeedsARecordedHead: a root that was not a repository (or had no
// commit) when the checkpoint was taken cannot report a drift — there is nothing
// to compare against, and guessing "no commit" as a baseline would turn a root
// that merely BECAME a repository into a warning nobody can act on.
func TestHeadDriftNeedsARecordedHead(t *testing.T) {
	m, root := setup(t, Options{})
	ctx := context.Background()
	write(t, root, "a.txt", "v1")
	beginSnapshot(t, m, 1)

	cmd := exec.Command("git", "-C", root, "-c", "user.name=t", "-c", "user.email=t@example.invalid",
		"init", "--quiet", ".")
	require.NoError(t, cmd.Run())
	cmd = exec.Command("git", "-C", root, "-c", "user.name=t", "-c", "user.email=t@example.invalid",
		"commit", "--quiet", "-am", "made a repository after the checkpoint")
	_ = cmd.Run() // nothing staged: the commit may legitimately fail, the point is HEAD now exists

	p, err := m.Preview(ctx, 1)
	require.NoError(t, err)
	assert.Empty(t, p.Irreversible, "no recorded HEAD means no baseline, not a drift")
}

// TestDropAfterReleasesTheAbandonedTurns pins the rewind's own cleanup: the
// checkpoints of the turns it undid index a conversation that no longer exists, so
// their records AND their refs go — but the turn rewound TO stays, because it is
// the new end of the conversation and the next turn continues from it.
func TestDropAfterReleasesTheAbandonedTurns(t *testing.T) {
	m, root := setup(t, Options{})
	ctx := context.Background()

	write(t, root, "a.txt", "v1")
	beginSnapshot(t, m, 1)
	require.NoError(t, m.SnapshotEnd(ctx, 1))
	write(t, root, "a.txt", "v2")
	beginSnapshot(t, m, 2)
	require.NoError(t, m.SnapshotEnd(ctx, 2))
	write(t, root, "a.txt", "v3")
	beginSnapshot(t, m, 3)
	require.NoError(t, m.SnapshotEnd(ctx, 3))
	// Both refs of every turn are here: where it started and where it ended.
	require.ElementsMatch(t, []string{
		"refs/tachi/00/1", "refs/tachi/00/1-end",
		"refs/tachi/00/2", "refs/tachi/00/2-end",
		"refs/tachi/00/3", "refs/tachi/00/3-end",
	}, m.refsAt(t, 0))

	dropped, err := m.DropAfter(1)
	require.NoError(t, err)
	assert.Equal(t, 2, dropped)

	turns, err := m.Turns()
	require.NoError(t, err)
	require.Len(t, turns, 1)
	assert.Equal(t, 1, turns[0].Turn)
	assert.ElementsMatch(t, []string{"refs/tachi/00/1", "refs/tachi/00/1-end"}, m.refsAt(t, 0),
		"an abandoned branch is disposable: BOTH of its refs go (a lone tree could not be diffed anyway)")

	// A dropped turn is refused rather than restoring the abandoned branch...
	p, err := m.Preview(ctx, 2)
	require.NoError(t, err)
	assert.Equal(t, FilesUnknown, p.State)

	// ...the numbering continues from the cut...
	turn, err := m.Begin(ctx, Boundary{})
	require.NoError(t, err)
	assert.Equal(t, 2, turn, "the turn after the cut is numbered from the cut")

	// ...and the surviving checkpoint still restores.
	res, err := m.Restore(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Target)
	assert.Equal(t, "v1", readFile(t, root, "a.txt"))
}

// TestAFailedSnapshotReleasesTheRootsItAlreadyTook: a snapshot is all-or-nothing
// (the caller records no state when any root fails, because restoring the roots
// that worked would silently move the others to the wrong point in time), so the
// refs the call created before the failure must not outlive it — nothing else
// would ever release them, and they keep their objects alive for good.
func TestAFailedSnapshotReleasesTheRootsItAlreadyTook(t *testing.T) {
	base := t.TempDir()
	// The names matter: roots are sorted, and the one that trips the guard has to
	// be snapshotted LAST for the rollback to have something to roll back.
	good := filepath.Join(base, "a-good")
	bad := filepath.Join(base, "z-bad")
	write(t, good, "a.txt", "v1")
	write(t, bad, "b1.txt", "v1")
	write(t, bad, "b2.txt", "v2")

	m := NewManager(filepath.Join(base, "session"), []string{good, bad}, Options{MaxFiles: 1})
	ctx := context.Background()
	turn, err := m.Begin(ctx, Boundary{Records: 1})
	require.NoError(t, err)
	require.NoError(t, m.Snapshot(ctx, turn), "a tripped guard must not fail the turn")

	rec, ok, err := m.Record(turn)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Empty(t, rec.Roots, "no state is recorded when any root fails")
	assert.Contains(t, rec.Skipped, "超过上限")

	assert.Empty(t, m.refsAt(t, 0), "the half-taken snapshot must not leave a ref behind")
}

// TestTurnDiffIsExactAndFrozen is the point of the end-of-turn snapshot: a turn's changes
// are read from its own two trees, so a shell command's writes count (nothing declared
// them), created and deleted files are exact, and the answer does not change when the
// worktree — or the store — moves on afterwards.
func TestTurnDiffIsExactAndFrozen(t *testing.T) {
	m, root := setup(t, Options{})
	ctx := context.Background()
	write(t, root, "keep.txt", "untouched\n")
	write(t, root, "gone.txt", "will be deleted")

	beginSnapshot(t, m, 1)
	// The "turn": everything below is what a Bash command looks like from the outside.
	write(t, root, "new.txt", "one\ntwo\nthree\n")
	write(t, root, "keep.txt", "untouched\nchanged\n")
	require.NoError(t, os.Remove(filepath.Join(root, "gone.txt")))
	require.NoError(t, m.SnapshotEnd(ctx, 1))

	rec, ok, err := m.Record(1)
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, rec.Diff, "a turn that wrote gets a summary")
	assert.Empty(t, rec.Diff.Skipped)
	assert.Equal(t, 3, rec.Diff.Files, "created + changed + deleted")
	assert.Equal(t, 4, rec.Diff.Added, "three new lines plus the one added to keep.txt")
	assert.Equal(t, 1, rec.Diff.Removed, "gone.txt's single line — keep.txt only GAINED a line")

	text, hasPair, err := m.TurnDiff(ctx, 1)
	require.NoError(t, err)
	require.True(t, hasPair)
	for _, want := range []string{"new.txt", "keep.txt", "gone.txt", "+three", "-will be deleted"} {
		assert.Contains(t, text, want)
	}

	// Frozen: a later change to the same file is NOT part of this turn's diff...
	write(t, root, "keep.txt", "a completely different content\n")
	later, hasPair, err := m.TurnDiff(ctx, 1)
	require.NoError(t, err)
	require.True(t, hasPair)
	assert.Equal(t, text, later, "the turn's diff must not move when the worktree does")

	// ...and the panel can say so, per file, instead of implying it shows the disk.
	changed, err := m.ChangedSinceTurn(ctx, 1)
	require.NoError(t, err)
	assert.True(t, changed["keep.txt"])
	assert.False(t, changed["new.txt"], "a file nobody touched since is still where the turn left it")

	// Ending twice is a no-op (a turn can be re-run through the same path on a retry).
	require.NoError(t, m.SnapshotEnd(ctx, 1))
	again, _, err := m.Record(1)
	require.NoError(t, err)
	assert.Equal(t, rec.Diff, again.Diff)
}

// TestTurnThatOnlyReadsHasNoDiff: the end snapshot is taken only for a turn that has a
// START state, so a read-only turn pays nothing and reports nothing — and a turn whose
// end state was refused says why rather than reporting zero files.
func TestTurnThatOnlyReadsHasNoDiff(t *testing.T) {
	m, root := setup(t, Options{})
	ctx := context.Background()
	write(t, root, "a.txt", "v1")

	// A turn that only reads: Begin, no Snapshot, then its end.
	turn, err := m.Begin(ctx, Boundary{Records: 1})
	require.NoError(t, err)
	require.NoError(t, m.SnapshotEnd(ctx, turn))
	rec, _, err := m.Record(turn)
	require.NoError(t, err)
	assert.Nil(t, rec.Diff, "nothing was written, so there is no pair to compare")

	_, hasPair, err := m.TurnDiff(ctx, turn)
	require.NoError(t, err)
	assert.False(t, hasPair)

	// A turn whose end snapshot is refused: the numbers are absent WITH a reason, and the
	// end ref it took is released rather than left for nobody to find — but the START ref
	// stays, because that is this turn's rewind point (see the cascade test below).
	a, aRoot := setup(t, Options{})
	beginSnapshot(t, a, 1)
	write(t, aRoot, "huge.bin", strings.Repeat("x", 64))
	a.opts.MaxBytes = 16
	require.NoError(t, a.SnapshotEnd(ctx, 1), "a refused end must not fail the turn")

	rec, _, err = a.Record(1)
	require.NoError(t, err)
	require.NotNil(t, rec.Diff)
	assert.Contains(t, rec.Diff.Skipped, "超过单文件上限")
	assert.Zero(t, rec.Diff.Files)
	assert.ElementsMatch(t, []string{"refs/tachi/00/1"}, a.refsAt(t, 0),
		"a refused end releases only its own end ref: the start ref is still the way back")
}

// TestARefusedEndKeepsTheSessionCheckpointing is the blast radius of the cleanup above, and
// the reason it may not take rec.Roots wholesale: parentRef names the START ref of the
// newest earlier writing turn, so a missing one makes `commit-tree -p <ref>` fail — that
// turn records no state either, and every turn after it fails the same way. One refused end
// would switch the rest of the session's checkpoints off, including the rewind to the turn
// that was refused.
func TestARefusedEndKeepsTheSessionCheckpointing(t *testing.T) {
	m, root := setup(t, Options{MaxBytes: 16})
	ctx := context.Background()
	write(t, root, "a.txt", "v1")
	beginSnapshot(t, m, 1)
	write(t, root, "a.txt", "v2")
	write(t, root, "big.bin", strings.Repeat("x", 64))
	require.NoError(t, m.SnapshotEnd(ctx, 1))
	rec1, _, err := m.Record(1)
	require.NoError(t, err)
	require.Contains(t, rec1.Diff.Skipped, "超过单文件上限")

	// The turn that comes next must chain to turn 1's START ref as usual — the refused end
	// belongs to the end-only half and says nothing about the start.
	require.NoError(t, os.Remove(filepath.Join(root, "big.bin")))
	m.opts.MaxBytes = 0 // back to the default: the guard tripped on the turn's own file
	beginSnapshot(t, m, 2)
	write(t, root, "b.txt", "written by turn 2\n")
	require.NoError(t, m.SnapshotEnd(ctx, 2))

	rec2, _, err := m.Record(2)
	require.NoError(t, err)
	require.NotNil(t, rec2.Diff, "turn 2 still gets its own numbers")
	assert.Empty(t, rec2.Diff.Skipped)
	assert.Equal(t, 1, rec2.Diff.Files)

	// And the refused turn is still rewound to: its files come back from the START tree.
	res, err := m.Restore(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, FilesAvailable, res.State)
	assert.Equal(t, "v1", readFile(t, root, "a.txt"))
	assert.False(t, exists(root, "b.txt"), "the restored state is the refused turn's start")
}

// refsAt lists one root's checkpoint refs, so a test can assert what a drop (or a
// rollback) released rather than trusting the manifest alone.
func (m *Manager) refsAt(t *testing.T, index int) []string {
	t.Helper()
	out, err := m.repoAt(index).output(context.Background(), "for-each-ref", "--format=%(refname)")
	require.NoError(t, err)
	fields := strings.Fields(out)
	if fields == nil {
		return []string{}
	}
	return fields
}

// TestTheUserRepositoryIsNeverTouched is the safety claim the design rests on,
// asserted rather than assumed: with a real repository in the root, recording a
// checkpoint must leave its HEAD, index and status exactly as they were.
func TestTheUserRepositoryIsNeverTouched(t *testing.T) {
	m, root := setup(t, Options{})

	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
		return strings.TrimSpace(string(out))
	}
	git("init", "--quiet", ".")
	git("config", "user.email", "smoke@example.com")
	git("config", "user.name", "smoke")
	write(t, root, "tracked.txt", "committed")
	git("add", "tracked.txt")
	git("commit", "--quiet", "-m", "initial")
	write(t, root, "dirty.txt", "uncommitted work")

	before := git("status", "--porcelain")
	headBefore := git("rev-parse", "HEAD")

	beginSnapshot(t, m, 1)

	assert.Equal(t, before, git("status", "--porcelain"), "the user's working tree state must be untouched")
	assert.Equal(t, headBefore, git("rev-parse", "HEAD"), "the user's HEAD must not move")
	assert.Equal(t, "", git("diff", "--cached", "--name-only"), "the user's index must not be staged into")
	// And the private store is not inside the user's repository.
	_, err := os.Stat(filepath.Join(root, ".git", "refs", "tachi"))
	assert.True(t, os.IsNotExist(err), "checkpoint refs must live in the session directory, not the user's repo")
}

// TestNormalizeRootsDropsNestedRoots keeps a nested root from being snapshotted
// twice — once as its own tree and once as part of its parent.
func TestNormalizeRootsDropsNestedRoots(t *testing.T) {
	base := t.TempDir()
	inner := filepath.Join(base, "inner")
	require.NoError(t, os.MkdirAll(inner, 0o755))

	got := normalizeRoots([]string{base, inner, base, "", "  "})
	assert.Equal(t, []string{base}, got)
}

// TestCouldWriteClassifiesTools pins the policy the lazy snapshot rests on: a
// tool that is missing from the set is a change no checkpoint would cover, so
// the write-capable ones are named explicitly and MCP tools are treated as
// writable whatever they are.
func TestCouldWriteClassifiesTools(t *testing.T) {
	for _, name := range []string{
		tools.ToolNameBash, tools.ToolNameWrite, tools.ToolNameEdit,
		tools.ToolNameSubAgent, tools.ToolNameCron, tools.ToolNameSavePlan,
		tools.ToolNameRecordMemory, "mcp__pg__query",
	} {
		assert.True(t, CouldWrite(name), "%s must trigger a snapshot before it runs", name)
	}
	for _, name := range []string{
		tools.ToolNameRead, tools.ToolNameSendFile, "Glob", "Grep",
		"WebFetch", "WebSearch", "AskUserQuestion", "MCPSearchTools", "",
	} {
		assert.False(t, CouldWrite(name), "%s cannot change the workspace", name)
	}
}

// TestNormalizeRootsDropsUnboundedRoots is the safety net for the same trap: a root
// that is the filesystem or the user's home is never a workspace, and snapshotting
// one means walking it every turn.
func TestNormalizeRootsDropsUnboundedRoots(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	keep := filepath.Join(t.TempDir(), "work")
	require.NoError(t, os.MkdirAll(keep, 0o755))

	assert.Equal(t, []string{keep}, normalizeRoots([]string{"/", home, keep}))
}
