package mcp

import (
	"path/filepath"
	"sort"

	"github.com/monsterxx03/tachi/pkg/fileutil"
)

// RootsForDirs turns workspace directories into the roots a server is told about.
//
// It is the ONE rule for building the answer to MCP's `roots` capability, shared by every
// frontend that declares it (the desktop, answering for all its live sessions; ACP, answering
// for one), because the constraints are properties of what SERVERS do with the answer, not of
// any frontend:
//
//   - file:// URIs, since that is all the spec allows today;
//   - only directories that EXIST: a server resolves each root and skips the ones it cannot
//     (chrome-devtools logs "Could not resolve configured root"), so a stale root is noise at
//     best and, for a server that reads an empty answer as "no workspace", a denial at worst;
//   - deduplicated and sorted: servers CACHE the list, and an answer that reshuffles between
//     two asks for the same workspace set is churn nobody asked for.
//
// The name is the directory's base name — what a server shows a user who is deciding whether the
// access is right.
func RootsForDirs(dirs []string) []Root {
	seen := make(map[string]bool, len(dirs))
	kept := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		if dir == "" || seen[dir] || !fileutil.IsDir(dir) {
			continue
		}
		seen[dir] = true
		kept = append(kept, dir)
	}
	sort.Strings(kept)

	roots := make([]Root, 0, len(kept))
	for _, dir := range kept {
		roots = append(roots, Root{URI: "file://" + dir, Name: filepath.Base(dir)})
	}
	return roots
}
