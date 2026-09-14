package checkpoint

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file pins what a session's ROOT CHANGE does to its checkpoints — the case
// the manager used to get silently wrong, because a snapshot's store was derived
// from a root's POSITION (an index into a sorted, mutable set) while the directory
// a rewind wrote to was read from the CURRENT root set.
//
// Measured before the fix, with a temp probe: a session whose root moved from
// /treeA to /treeB rewound to turn 1 by writing /treeA's recorded content into
// /treeB/f.txt, leaving /treeA untouched, and reported `Restored: [f.txt]`.
//
// A desktop project edit, an added or removed additional root, and the checkout
// of a git worktree all change the root set, so this is not an edge case.

// rootsManager builds a manager over an explicit root set sharing one session
// directory — the "same session, different roots" a root change produces.
func rootsManager(t *testing.T, sessionDir string, roots ...string) *Manager {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	return NewManager(sessionDir, roots, Options{})
}

// writeTurn runs one writing turn: mark the boundary, snapshot the start, make the
// change, snapshot the end.
func writeTurn(t *testing.T, m *Manager, turn int, change func()) {
	t.Helper()
	ctx := context.Background()
	got, err := m.Begin(ctx, Boundary{Records: turn, APIRecords: turn})
	require.NoError(t, err)
	require.Equal(t, turn, got)
	require.NoError(t, m.Snapshot(ctx, turn))
	change()
	require.NoError(t, m.SnapshotEnd(ctx, turn))
}

// TestRewindAfterTheRootChangedRestoresTheRecordedTree is the regression test for
// the cross-tree write: after the session moved to another directory, a rewind must
// put the directory the TURN ran in back — and must not touch the new one at all.
func TestRewindAfterTheRootChangedRestoresTheRecordedTree(t *testing.T) {
	ctx := context.Background()
	sessionDir := t.TempDir()
	treeA := filepath.Join(sessionDir, "treeA")
	treeB := filepath.Join(sessionDir, "treeB")
	for _, d := range []string{treeA, treeB} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}
	write(t, treeA, "f.txt", "ORIGINAL-A\n")
	write(t, treeB, "f.txt", "ORIGINAL-B\n")

	// Turn 1 runs in treeA and changes it.
	mA := rootsManager(t, sessionDir, treeA)
	writeTurn(t, mA, 1, func() { write(t, treeA, "f.txt", "CHANGED-BY-TURN-1\n") })

	// The session's root changes to treeB (folder picker / project edit / worktree).
	mB := rootsManager(t, sessionDir, treeB)

	// The preview must say which workspace the checkpoint covers, and must label
	// the tree it measured with that same path.
	pv, err := mB.Preview(ctx, 1)
	require.NoError(t, err)
	require.Len(t, pv.Roots, 1)
	assert.Equal(t, treeA, pv.Roots[0].Root)
	assert.Contains(t, pv.RootMismatch, treeA, "the card has to name the recorded tree")
	assert.Contains(t, pv.RootMismatch, treeB, "and the one on screen")

	// The rewind restores treeA — the tree that turn ran in.
	res, err := mB.Restore(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, FilesAvailable, res.State)
	assert.Equal(t, "ORIGINAL-A\n", readFile(t, treeA, "f.txt"), "the recorded tree comes back")

	// And it does NOT write treeA's content into treeB.
	assert.Equal(t, "ORIGINAL-B\n", readFile(t, treeB, "f.txt"), "the new tree is not touched")
}

// TestARewindWithoutARootChangeSaysNothingAboutMismatchedWorkspaces guards the
// other direction: a checkpoint of the CURRENT workspace must not carry the
// warning, or the card would cry wolf on every ordinary rewind.
func TestARewindWithoutARootChangeSaysNothingAboutMismatchedWorkspaces(t *testing.T) {
	m, root := setup(t, Options{})
	writeTurn(t, m, 1, func() { write(t, root, "f.txt", "changed\n") })

	pv, err := m.Preview(context.Background(), 1)
	require.NoError(t, err)
	assert.Equal(t, FilesAvailable, pv.State)
	assert.Empty(t, pv.RootMismatch)
}

// TestARootKeepsItsStoreWhenItsPositionInTheSetChanges covers the second half of
// the old identity bug: the index is a position in a SORTED set, so adding an
// additional root can move an existing root to another index without its path
// changing at all. Its snapshots must keep going to its own store, and a rewind
// must still reach them.
func TestARootKeepsItsStoreWhenItsPositionInTheSetChanges(t *testing.T) {
	ctx := context.Background()
	sessionDir := t.TempDir()
	// "aaa" sorts before "work", so adding it pushes the original root from
	// index 0 to index 1.
	treeB := filepath.Join(sessionDir, "aaa")
	treeA := filepath.Join(sessionDir, "work")
	for _, d := range []string{treeA, treeB} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}
	write(t, treeA, "f.txt", "A1\n")
	write(t, treeB, "g.txt", "B1\n")

	mA := rootsManager(t, sessionDir, treeA)
	writeTurn(t, mA, 1, func() { write(t, treeA, "f.txt", "A1\n") })
	rec1, ok, err := mA.Record(1)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, rec1.Roots, 1)

	// A second root is added; the manager's set is sorted, so treeA is now index 1.
	mBoth := rootsManager(t, sessionDir, treeA, treeB)
	require.Equal(t, []string{treeB, treeA}, mBoth.Roots())
	writeTurn(t, mBoth, 2, func() { write(t, treeA, "f.txt", "A2\n") })
	rec2, ok, err := mBoth.Record(2)
	require.NoError(t, err)
	require.True(t, ok)

	// treeA's second snapshot went to treeA's own store — the one turn 1 wrote —
	// even though its index changed. treeB got a store of its own.
	var stateA2, stateB2 *RootState
	for i := range rec2.Roots {
		switch rec2.Roots[i].Root {
		case treeA:
			stateA2 = &rec2.Roots[i]
		case treeB:
			stateB2 = &rec2.Roots[i]
		}
	}
	require.NotNil(t, stateA2)
	require.NotNil(t, stateB2)
	assert.Equal(t, rec1.Roots[0].Store, stateA2.Store, "a root's store follows its path")
	assert.NotEqual(t, stateA2.Store, stateB2.Store, "two trees never share a store")

	// Rewinding to turn 1 (recorded before treeB existed) restores treeA and leaves
	// treeB alone.
	res, err := mBoth.Restore(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, FilesAvailable, res.State)
	assert.Equal(t, "A1\n", readFile(t, treeA, "f.txt"))
	assert.Equal(t, "B1\n", readFile(t, treeB, "g.txt"))
}

// TestRewindRefusesWhenARecordedRootIsGone: a directory can be moved or unmounted
// after a snapshot, and the recorded tree is useless without it. The answer is a
// reason (no file restored, conversation still rewinds), never a raw git error and
// never a write into whatever now occupies the path.
func TestRewindRefusesWhenARecordedRootIsGone(t *testing.T) {
	ctx := context.Background()
	sessionDir := t.TempDir()
	tree := filepath.Join(sessionDir, "work")
	require.NoError(t, os.MkdirAll(tree, 0o755))
	write(t, tree, "f.txt", "v1\n")

	m := rootsManager(t, sessionDir, tree)
	writeTurn(t, m, 1, func() { write(t, tree, "f.txt", "v2\n") })
	require.NoError(t, os.RemoveAll(tree))

	pv, err := m.Preview(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, FilesUnknown, pv.State)
	assert.Contains(t, pv.Skipped, tree)
	assert.Empty(t, pv.Roots, "nothing is offered to restore")

	res, err := m.Restore(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, FilesUnknown, res.State)
	assert.Contains(t, res.Skipped, "已不存在")
}

// TestAPathKeepsTheStoreItAlreadyHad covers the upgrade path: a session whose
// records predate path-keyed stores keeps its own store — so its commit chain stays
// unbroken and its index cache survives (no cold snapshot of the whole tree) —
// while a path with no record yet gets a fresh store of its own.
func TestAPathKeepsTheStoreItAlreadyHad(t *testing.T) {
	ctx := context.Background()
	sessionDir := t.TempDir()
	root := filepath.Join(sessionDir, "work")
	require.NoError(t, os.MkdirAll(root, 0o755))
	write(t, root, "f.txt", "v1\n")

	m := rootsManager(t, sessionDir, root)
	writeTurn(t, m, 1, func() { write(t, root, "f.txt", "v2\n") })

	// Downgrade the store to the pre-fix shape: the index-derived directory, with
	// no Store recorded — exactly what a manifest written by the old code holds.
	man, err := loadManifest(m.dir)
	require.NoError(t, err)
	require.Len(t, man.Checkpoints, 1)
	hashStore := man.Checkpoints[0].Roots[0].Store
	require.NotEmpty(t, hashStore)
	legacy := legacyStoreDir(0)
	require.NoError(t, os.Rename(filepath.Join(m.dir, hashStore), filepath.Join(m.dir, legacy)))
	man.Checkpoints[0].Roots[0].Store = ""
	require.NoError(t, saveManifest(m.dir, man))

	// The next writing turn adopts that store instead of starting a new one.
	writeTurn(t, m, 2, func() { write(t, root, "f.txt", "v3\n") })
	rec, ok, err := m.Record(2)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, rec.Roots, 1)
	assert.Equal(t, legacy, rec.Roots[0].Store, "the same path keeps the store it already had")
	assert.Equal(t, []string{legacy}, storeDirs(t, m.dir), "no second store was created")

	// The chain is unbroken: turn 2's commit descends from turn 1's, which is only
	// possible because the parent ref resolved inside the same store.
	out, err := exec.Command("git", "--git-dir="+filepath.Join(m.dir, legacy, "repo.git"),
		"rev-list", "--count", "refs/tachi/00/2").Output()
	require.NoError(t, err)
	assert.Equal(t, "2\n", string(out))

	// And the old checkpoint still reads back through its (recorded-as-legacy) store.
	res, err := m.Restore(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, FilesAvailable, res.State)
	assert.Equal(t, "v1\n", readFile(t, root, "f.txt"))
}

// storeDirs lists the checkpoint store directories of a session's checkpoint dir.
func storeDirs(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}
