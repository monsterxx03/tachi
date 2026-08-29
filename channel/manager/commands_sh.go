package manager

import (
	"context"
	"strings"

	"github.com/monsterxx03/tachi/pkg/shutil"
)

// handleShCommand runs a raw shell command on behalf of the /sh slash
// command and returns the output for direct echo — no LLM involvement.
//
// The command executes in the thread's working directory (set via /cd or
// the agent's Bash tool); with no per-thread directory it inherits the
// process CWD. Note that `cd` inside a /sh invocation is ephemeral (the
// command runs as one `sh -c` process) — persistent directory changes
// belong to /cd.
//
// Output (stdout+stderr combined) is capped by shutil.Shell and formatted
// by shutil.FormatShellResult as a fenced code block so markdown-capable
// IM clients render it verbatim. Execution failures — non-zero exit,
// timeout — are reported as part of the reply text rather than handler
// errors: they are expected outcomes of running arbitrary commands, not
// command-handler faults.
func (m *Manager) handleShCommand(ctx context.Context, threadID, command string) (string, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "用法: `/sh <command>` — 在当前线程工作目录执行 shell 命令并回显输出(目录切换不持久,持久切目录用 /cd)", nil
	}

	cwd := m.getThreadWorkDir(threadID)
	cctx, cancel := context.WithTimeout(ctx, shutil.DefaultShellTimeout)
	defer cancel()

	out, code, _ := shutil.Shell(cctx, cwd, command)
	return shutil.FormatShellResult(command, out, code, cctx.Err() != nil), nil
}
