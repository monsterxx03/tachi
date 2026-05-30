package agent

import (
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent/memory"
)

// MemoryState bundles all memory-related state for an AIAgent.
// A nil *MemoryState means memory is not configured.
type MemoryState struct {
	Backend          memory.Backend
	Timeout          time.Duration // context deadline for Store/Recall/Forget
	ToolResultMaxLen int           // max chars for tool result in memory (0 = no limit)
	ExcludeRepos     []string      // git repo roots to skip all memory writes
	SkipWrites       bool          // suppress turn-level memory writes (e.g. /commit, /init)
	SkipRecall       bool          // suppress memory recall (e.g. "tachi run")
}

// IsRepoExcluded checks whether the current git repo root is in the
// exclude_repos list. If we're not in a git repo, returns false.
func (m *MemoryState) IsRepoExcluded() bool {
	if len(m.ExcludeRepos) == 0 {
		return false
	}
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return false
	}
	repoRoot := strings.TrimSpace(string(out))
	return slices.ContainsFunc(m.ExcludeRepos, func(excluded string) bool {
		return filepath.Clean(repoRoot) == filepath.Clean(excluded)
	})
}
