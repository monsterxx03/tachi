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
	assert.Contains(t, p.Skipped, "byte limit")
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
	assert.Contains(t, p.Skipped, "file limit")
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
	assert.Contains(t, p.Skipped, "not checkpointed", "a pruned turn must refuse rather than silently no-op")

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
	assert.Contains(t, p.Skipped, "not checkpointed")
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
