package lsp

import (
	"os/exec"
	"path/filepath"
	"strings"
)

// FilterGitIgnored filters out locations whose file paths are gitignored.
// Uses `git check-ignore` with batched paths for efficiency.
func FilterGitIgnored(locations []Location, cwd string) []Location {
	if len(locations) == 0 || cwd == "" {
		return locations
	}

	// Collect unique file paths.
	seen := map[string]string{} // URI → filePath
	for _, loc := range locations {
		if loc.URI != "" {
			if _, ok := seen[loc.URI]; !ok {
				seen[loc.URI] = URItoPath(loc.URI)
			}
		}
	}

	uniquePaths := make([]string, 0, len(seen))
	for _, fp := range seen {
		uniquePaths = append(uniquePaths, fp)
	}

	if len(uniquePaths) == 0 {
		return locations
	}

	// Batch check with git check-ignore.
	ignored := map[string]bool{}
	const batchSize = 50
	for i := 0; i < len(uniquePaths); i += batchSize {
		end := min(i+batchSize, len(uniquePaths))
		batch := uniquePaths[i:end]

		cmd := exec.Command("git", append([]string{"check-ignore"}, batch...)...)
		cmd.Dir = cwd
		out, err := cmd.Output()
		if err != nil {
			// Exit code 0 = at least one file ignored; 1 = none ignored; 128 = not a repo.
			if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 128 {
				// Not a git repo — can't filter.
				return locations
			}
		}
		for line := range strings.SplitSeq(string(out), "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				ignored[line] = true
			}
		}
	}

	if len(ignored) == 0 {
		return locations
	}

	// Filter out ignored locations.
	filtered := make([]Location, 0, len(locations))
	for _, loc := range locations {
		fp := URItoPath(loc.URI)
		if !ignored[fp] && !isUnderIgnoredDir(fp, ignored) {
			filtered = append(filtered, loc)
		}
	}
	return filtered
}

// isUnderIgnoredDir checks if a file path is under any of the ignored directories.
func isUnderIgnoredDir(fp string, ignored map[string]bool) bool {
	for dir := range ignored {
		if strings.HasPrefix(fp, dir+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
