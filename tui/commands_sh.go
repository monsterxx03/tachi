package tui

import (
	"context"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/monsterxx03/tachi/pkg/shutil"
)

// shDoneMsg carries the finished /sh result from the executor goroutine
// back to the TUI update loop. Execution is async so the UI keeps
// rendering while the command runs.
type shDoneMsg struct {
	content string
}

// handleShCommand implements the TUI side of /sh: it parses the raw
// subcommand input ("/sh <command>"), echoes it, and launches the shell
// asynchronously — the result lands via shDoneMsg. TUI has no per-thread
// working directory (that's a channel concept), so commands run in the
// process CWD.
func (m *Model) handleShCommand() tea.Cmd {
	raw := strings.TrimSpace(m.subcommandInput)
	command := ""
	if rest, ok := strings.CutPrefix(raw, "/sh"); ok {
		command = strings.TrimSpace(rest)
	}
	if command == "" {
		m.chatview.AddMessage(chatMessage{
			Role:    "assistant",
			Content: "用法: /sh <command> — 执行 shell 命令并回显输出(目录切换不持久)",
		})
		return nil
	}

	m.chatview.AddMessage(chatMessage{Role: "user", Content: "/sh " + command})

	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), shutil.DefaultShellTimeout)
		defer cancel()
		out, code, _ := shutil.Shell(ctx, "", command)
		return shDoneMsg{content: shutil.FormatShellResult(command, out, code, ctx.Err() != nil)}
	}
}
