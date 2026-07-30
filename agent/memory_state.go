package agent

import (
	"github.com/monsterxx03/tachi/agent/memory"
)

// MemoryState bundles runtime memory state for an AIAgent.
// Static config values (timeout) are read directly from a.Config.FullConfig.Memory
// rather than duplicated here.
// A nil *MemoryState means memory is not configured.
type MemoryState struct {
	Backend    memory.Backend
	SkipRecall bool // suppress memory recall (e.g. "tachi run")
}
