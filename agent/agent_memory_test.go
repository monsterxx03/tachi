package agent

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- Tests: normalizeRepoPaths ----

func TestNormalizeRepoPaths_Empty(t *testing.T) {
	assert.Nil(t, normalizeRepoPaths(nil))
	var empty []string
	assert.Equal(t, empty, normalizeRepoPaths(empty))
}

func TestNormalizeRepoPaths_NoTilde(t *testing.T) {
	paths := []string{"/foo/bar", "/baz"}
	result := normalizeRepoPaths(paths)
	assert.Equal(t, []string{"/foo/bar", "/baz"}, result)
}

func TestNormalizeRepoPaths_TildePrefix(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	result := normalizeRepoPaths([]string{"~/repos/myproject"})
	assert.Equal(t, []string{home + "/repos/myproject"}, result)
}

func TestNormalizeRepoPaths_HomeOnly(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	result := normalizeRepoPaths([]string{"~"})
	assert.Equal(t, []string{home}, result)
}

func TestNormalizeRepoPaths_TrailingSlash(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	result := normalizeRepoPaths([]string{"~/repos/tachi/"})
	assert.Equal(t, []string{home + "/repos/tachi"}, result)
}

func TestNormalizeRepoPaths_Mixed(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	result := normalizeRepoPaths([]string{
		"~/repos/tachi",
		"/absolute/path",
		"  ~/other  ",
		"",
	})
	assert.Equal(t, []string{
		home + "/repos/tachi",
		"/absolute/path",
		home + "/other",
	}, result)
}

// ---- Tests: isRepoExcluded ----

func TestIsRepoExcluded_NoExcludeList(t *testing.T) {
	a := &AIAgent{excludeRepos: nil}
	assert.False(t, a.isRepoExcluded())
}

func TestIsRepoExcluded_NotInGitRepo(t *testing.T) {
	// Create a temp dir that is NOT a git repo
	dir := t.TempDir()
	origWd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	defer func() { _ = os.Chdir(origWd) }()

	a := &AIAgent{excludeRepos: []string{dir}}
	assert.False(t, a.isRepoExcluded())
}

func TestIsRepoExcluded_CurrentRepoNotInList(t *testing.T) {
	// We're inside the tachi repo itself, which won't be in the list
	a := &AIAgent{excludeRepos: []string{"/some/other/repo"}}
	assert.False(t, a.isRepoExcluded())
}

func TestIsRepoExcluded_CurrentRepoMatches(t *testing.T) {
	// Get the actual repo root
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	require.NoError(t, err)
	repoRoot := strings.TrimSpace(string(out))

	a := &AIAgent{excludeRepos: []string{"/some/other", repoRoot}}
	assert.True(t, a.isRepoExcluded())
}

func TestIsRepoExcluded_PartialPrefixNoMatch(t *testing.T) {
	// Get the actual repo root
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	require.NoError(t, err)
	repoRoot := strings.TrimSpace(string(out))

	// A parent directory should NOT match
	a := &AIAgent{excludeRepos: []string{repoRoot[:len(repoRoot)/2]}}
	assert.False(t, a.isRepoExcluded())
}
