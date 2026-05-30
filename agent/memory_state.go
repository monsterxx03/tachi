package agent

import (
	"github.com/monsterxx03/tachi/agent/memory"
)

// MemoryState bundles runtime memory state for an AIAgent.
// Static config values (timeout, tool_result_max_len, exclude_repos) are
// read directly from a.cfg.Memory rather than duplicated here.
// A nil *MemoryState means memory is not configured.
type MemoryState struct {
	Backend    memory.Backend
	SkipWrites bool // suppress turn-level memory writes (e.g. /commit, /init)
	SkipRecall bool // suppress memory recall (e.g. "tachi run")
}
