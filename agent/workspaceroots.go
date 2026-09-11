package agent

import (
	"fmt"
	"path/filepath"
)

// NormalizeAdditionalRoots validates and normalizes additional workspace roots
// against a primary root: every entry must be a non-empty absolute path, entries
// are cleaned, exact duplicates and entries equal to primary are dropped, and
// first-occurrence order is preserved. A nil or empty input yields nil.
//
// This is the SHARED half of the rule — syntax only. ACP (additionalDirectories
// from the editor) and the desktop (directories the user picked) both go through
// it, so the two frontends cannot drift into different notions of what a root set
// is.
//
// What it deliberately does not do: check that a directory exists, or reject paths
// containing whitespace. Those are frontend concerns — the caller that has a user to
// tell. The desktop rejects spaced roots because its @-references are
// whitespace-delimited; an editor handing ACP such a path is not doing anything
// wrong, and refusing it there would break a working client.
func NormalizeAdditionalRoots(primary string, roots []string) ([]string, error) {
	if len(roots) == 0 {
		return nil, nil
	}

	out := make([]string, 0, len(roots))
	seen := make(map[string]bool, len(roots))
	for _, root := range roots {
		if root == "" {
			return nil, fmt.Errorf("additional workspace root must not be empty")
		}
		if !filepath.IsAbs(root) {
			return nil, fmt.Errorf("additional workspace root must be an absolute path: %q", root)
		}
		// Clean BEFORE the duplicate check: without it "/a/b/../c" and "/a/c"
		// would be kept as two roots for the same directory.
		root = filepath.Clean(root)
		if root == primary || seen[root] {
			continue
		}
		seen[root] = true
		out = append(out, root)
	}
	return out, nil
}
