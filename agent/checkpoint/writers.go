package checkpoint

import (
	"strings"

	"github.com/monsterxx03/tachi/agent/tools"
)

// mcpToolPrefix marks a tool that came from an MCP server (`mcp__<server>__<tool>`).
const mcpToolPrefix = "mcp__"

// couldWriteTools are the built-in tools whose execution can change the
// workspace. The set is a POLICY, and it is deliberately generous: a tool that
// is listed but turns out to write nothing costs one skipped manifest lookup
// (Snapshot is idempotent), while a tool that is MISSING is a change no
// checkpoint covers — and the whole point of the design is that the snapshot
// sees the tree rather than trusting the tools to announce themselves.
//
// Read-only tools (ReadFile, Glob, Grep, WebFetch, WebSearch, LSP*, AskUserQuestion,
// ReportFinding, MCPSearchTools, MemoryRecall, Skill) are absent on purpose.
var couldWriteTools = map[string]bool{
	tools.ToolNameBash:         true, // arbitrary shell: the reason this feature exists
	tools.ToolNameWrite:        true,
	tools.ToolNameEdit:         true,
	tools.ToolNameSubAgent:     true, // a subagent edits the tree as it runs
	tools.ToolNameCron:         true, // persists a task definition
	tools.ToolNameSavePlan:     true, // writes the plan document
	tools.ToolNameRecordMemory: true, // the topic backend can write inside the repo
}

// CouldWrite reports whether executing a tool could change the workspace, and so
// whether the file half of the turn's checkpoint must be taken before it runs.
//
// MCP tools are always treated as writable: a server's tool can do anything, and
// "the checkpoint is missing a change because the tool came from MCP" is not a
// failure mode worth trading a few microseconds for.
func CouldWrite(toolName string) bool {
	if strings.HasPrefix(toolName, mcpToolPrefix) {
		return true
	}
	return couldWriteTools[toolName]
}
