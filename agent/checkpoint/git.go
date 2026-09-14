package checkpoint

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/monsterxx03/tachi/pkg/shutil"
)

// repo is a private, BARE git repository that records one workspace root.
//
// It is deliberately not a git worktree: the design's whole point is that no
// directory copy is ever made. GIT_DIR holds objects and refs, and the root is
// passed as the work tree, so git reads the user's own files and stores only
// content-addressed blobs of them. Nothing in the user's repository — HEAD,
// index, or worktree — is touched, which is also why this works in a directory
// that is not a git repository at all.
type repo struct {
	dir  string // GIT_DIR: <session>/checkpoints/<root-index>/repo.git
	root string // GIT_WORK_TREE: the user's own directory
}

// configArgs are passed on EVERY invocation rather than written into the
// repository's config, so a half-initialised repo can never be used with the
// wrong semantics. Each one prevents a specific, silent corruption:
//
//   - core.compression=1 — this is a scratch store, not an archive; the cold
//     snapshot is dominated by writing one loose object per file (measured:
//     20k small files => 20k objects, 80MB), and speed matters more than size.
//   - core.autocrlf=false — without it, a machine configured with autocrlf
//     would have line endings rewritten BY GIT on restore, so a rewind would
//     hand the user a file that differs from the one that was there.
//   - core.excludesFile=<repo>/info/exclude — points the "global" ignore at our
//     own (empty) file so the only rules in force are the project's .gitignore
//     plus the built-ins below. A machine-wide ignore would otherwise silently
//     shrink what a checkpoint covers, and the coverage rule has to be the one
//     the manual documents.
func (r repo) configArgs() []string {
	return []string{
		"-c", "core.compression=" + strconv.Itoa(objectCompression),
		"-c", "core.autocrlf=false",
		"-c", "core.excludesFile=" + filepath.Join(r.dir, "info", "exclude"),
	}
}

// args builds a git invocation pinned to this repository and work tree. The
// repository is passed by flag rather than via environment variables on
// purpose: several agents can run in one process (channel mode caches one per
// thread), and an env var set for one would leak into another's commands.
func (r repo) args(cmd ...string) []string {
	base := []string{"--git-dir=" + r.dir, "--work-tree=" + r.root}
	base = append(base, r.configArgs()...)
	return append(base, cmd...)
}

func (r repo) run(ctx context.Context, cmd ...string) error {
	return shutil.Run(ctx, "", "git", r.args(cmd...)...)
}

func (r repo) output(ctx context.Context, cmd ...string) (string, error) {
	return shutil.Output(ctx, "", "git", r.args(cmd...)...)
}

// objectCompression trades repository size for write speed. Nothing reads this
// store except a rewind, and the cold snapshot is dominated by writing one loose
// object per file, so packing is a better place to spend the bytes.
const objectCompression = 1

// excludeFile is the repository's info/exclude: the built-in rules that apply
// on top of the project's own .gitignore. `.git` is excluded because it is the
// user's history (a Bash `git commit` is an irreversible side effect to be
// reported, not silently rewound); `.tachi` because it is agent state.
const excludeFile = ".git\n.tachi\n"

// userHead is the commit the ROOT's own repository has checked out, or "" when
// the root is not a git repository (or has no commit yet).
//
// It is read from the USER's repository, not through this store, because that is
// the one side effect a rewind cannot put back: the snapshot restores files, and
// a `git commit` the agent made still sits in the log afterwards. A rewind across
// one therefore has to say so (see Manager.preview).
//
// The invocation carries no --git-dir/--work-tree on purpose — those point at our
// shadow store. The root is passed as the working directory instead, so git
// discovers the user's repository the way the user's own shell would.
func (r repo) userHead(ctx context.Context) string {
	out, err := shutil.Output(ctx, r.root, "git", "rev-parse", "HEAD")
	if err != nil {
		return "" // not a repository, or no first commit yet: nothing to lose
	}
	return strings.TrimSpace(out)
}

// init creates the bare repository if it is not there yet and (re)writes the
// built-in excludes. Idempotent: called lazily, right before the first
// snapshot of a root, never at session start.
func (r repo) init(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(r.dir, "HEAD")); err != nil {
		if err := shutil.Run(ctx, "", "git", "init", "--quiet", "--bare", r.dir); err != nil {
			return fmt.Errorf("init checkpoint repo %s: %w", r.dir, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(r.dir, "info"), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(r.dir, "info", "exclude"), []byte(excludeFile), 0o644)
}

// trackedFileCount counts everything a snapshot would cover: tracked files plus
// untracked ones that are not ignored. It is the number the file-count guard
// compares against, because that count — not the size of a change — is what
// makes every later turn slower.
func (r repo) trackedFileCount(ctx context.Context) (int, error) {
	out, err := r.output(ctx, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return 0, err
	}
	return len(splitNul(out)), nil
}

// candidateFiles lists the paths that a snapshot would pick up: modified and
// untracked files, ignoring what .gitignore ignores.
//
// It exists for the byte guard: the size check has to run BEFORE `add -A`,
// because by then the blobs are written and the cost — the one thing the guard
// is there to avoid — is already paid. `ls-files -o -m` uses git's own index
// stat cache, so it is the same cheap walk `add -A` would do anyway.
func (r repo) candidateFiles(ctx context.Context) ([]string, error) {
	out, err := r.output(ctx, "ls-files", "-z", "--others", "--modified", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	return splitNul(out), nil
}

// snapshot writes the working tree's current state and returns its tree hash.
// It updates this repository's index — never the user's.
func (r repo) snapshot(ctx context.Context) (string, error) {
	if err := r.run(ctx, "add", "-A"); err != nil {
		return "", fmt.Errorf("git add -A in %s: %w", r.root, err)
	}
	tree, err := r.output(ctx, "write-tree")
	if err != nil {
		return "", fmt.Errorf("git write-tree in %s: %w", r.root, err)
	}
	return strings.TrimSpace(tree), nil
}

// changes lists how one tree differs from another, as NUL-joined
// "status\0path\0" fields from git diff --name-status -z.
//
// Both sides are TREES, so the caller must snapshot the working tree first.
// Comparing against the working tree directly (`git diff <tree>`) is not enough:
// untracked files are invisible to it, and "the agent created a file with a
// shell command" is precisely the case this feature exists for. --no-renames
// keeps two fields per entry, which keeps the parser honest.
func (r repo) changes(ctx context.Context, from, to string) ([]byte, error) {
	out, err := r.output(ctx, "diff", "--name-status", "--no-renames", "-z", from, to)
	if err != nil {
		return nil, err
	}
	return []byte(out), nil
}

// numstat is git's own accounting for two trees: one line per changed file with
// added/removed counts ("-\t-" for a binary one). It is the number the turn footer
// shows, taken from the checkpoint rather than from what the tool calls claimed.
func (r repo) numstat(ctx context.Context, from, to string) (files, added, removed int, err error) {
	out, err := r.output(ctx, "diff", "--numstat", "--no-renames", from, to)
	if err != nil {
		return 0, 0, 0, err
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.SplitN(strings.TrimRight(line, "\r"), "\t", 3)
		if len(fields) != 3 || fields[2] == "" {
			continue // blank line, or a rename header we did not ask for
		}
		files++
		// A binary file reports "-" for both sides: it changed, but there is no line
		// count to add. Counting it as a file without lines is the honest summary.
		if n, convErr := strconv.Atoi(fields[0]); convErr == nil {
			added += n
		}
		if n, convErr := strconv.Atoi(fields[1]); convErr == nil {
			removed += n
		}
	}
	return files, added, removed, nil
}

// diffText is the unified diff between two TREES, for the changes panel and the
// review prompt: real file coordinates, and — because both sides are recorded —
// the same answer whenever it is asked, even after the worktree has moved on.
func (r repo) diffText(ctx context.Context, from, to string) (string, error) {
	return r.output(ctx, "diff", "--no-color", "-U3", "--no-renames", from, to)
}

// treeChangedSince reports whether the ROOT's working tree still differs from a tree,
// as NUL-joined paths. It is how a frozen diff says 「这个文件之后又改过」 without
// pretending the panel is looking at what is on disk right now.
func (r repo) treeChangedSince(ctx context.Context, tree string) ([]string, error) {
	out, err := r.output(ctx, "diff", "--name-only", "--no-renames", "-z", tree)
	if err != nil {
		return nil, err
	}
	return splitNul(out), nil
}

// stat returns git's diffstat between two trees, for the summary a reader
// confirms before anything is written back.
func (r repo) stat(ctx context.Context, from, to string) (string, error) {
	return r.output(ctx, "diff", "--stat", "--no-color", "--no-renames", from, to)
}

// setIndex points this repository's index at a tree without touching any file.
func (r repo) setIndex(ctx context.Context, tree string) error {
	if err := r.run(ctx, "read-tree", tree); err != nil {
		return fmt.Errorf("git read-tree %s in %s: %w", tree, r.root, err)
	}
	return nil
}

// writePaths rewrites the listed paths from the repository's index. Only the
// paths a rewind actually changes are passed: `checkout-index -a -f` would
// refresh every file's mtime and make the next incremental build rebuild the
// world.
func (r repo) writePaths(ctx context.Context, paths []string) error {
	for _, batch := range batches(paths, checkoutBatch) {
		args := append([]string{"checkout-index", "--force", "--"}, batch...)
		if err := r.run(ctx, args...); err != nil {
			return fmt.Errorf("git checkout-index in %s: %w", r.root, err)
		}
	}
	return nil
}

// checkoutBatch caps how many paths go into one git invocation: a rewind of a
// large repository can list more paths than the argument limit allows.
const checkoutBatch = 200

// batches splits paths into chunks of at most size.
func batches(paths []string, size int) [][]string {
	var out [][]string
	for len(paths) > 0 {
		n := min(size, len(paths))
		out = append(out, paths[:n])
		paths = paths[n:]
	}
	return out
}

// commitTree records tree as a commit whose parent is parent (empty for the
// first checkpoint of a root) and points ref at it. A tree hash would be enough
// to restore from; the commit adds the parent chain, which is what makes a
// checkpoint's diff against its predecessor a one-liner (`git diff --stat`).
func (r repo) commitTree(ctx context.Context, ref, tree, parent, message string) error {
	args := []string{"commit-tree", tree, "-m", message}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	commit, err := r.output(ctx, args...)
	if err != nil {
		return fmt.Errorf("git commit-tree in %s: %w", r.root, err)
	}
	if err := r.run(ctx, "update-ref", ref, strings.TrimSpace(commit)); err != nil {
		return fmt.Errorf("git update-ref %s in %s: %w", ref, r.root, err)
	}
	return nil
}

// gc packs loose objects. Loose objects each occupy a filesystem block, so a
// cold snapshot of many small files wastes far more space than the files
// themselves (measured: 20k files ≈ 1MB of content => 80MB of objects). Called
// right after the cold snapshot and again when checkpoints are pruned.
func (r repo) gc(ctx context.Context) error {
	return r.run(ctx, "gc", "--quiet", "--prune=now")
}

// splitNul splits NUL-delimited git output, dropping the trailing empty field.
func splitNul(s string) []string {
	parts := strings.Split(s, "\x00")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
