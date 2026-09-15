package systemreminder

import (
	"context"
	"fmt"

	"github.com/monsterxx03/tachi/pkg/shutil"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

// GitReminder injects the current git repository status on the first message
// of a brand-new conversation. It runs git commands to gather branch info and
// status, giving the model awareness of the git context without hard-coding it
// in the system prompt.
type GitReminder struct{}

func (GitReminder) Generate(ctx context.Context, rctx Context) []string {
	if !rctx.IsFirstMessage {
		return nil
	}
	// The session's tree, not the process's (see workDir): a desktop process hosts
	// several sessions, and reporting the branch of whatever directory the app was
	// launched from would contradict the working directory in the system prompt.
	dir := workDir(ctx)

	var lines []string

	// Current branch, including the detached case (shutil.GitBranch is the one probe; the desktop's
	// workspace panel reads it too, so the branch shown there is the one the model is told about).
	if branch, detached, ok := shutil.GitBranch(ctx, dir); ok {
		if detached {
			// Detached HEAD, show short commit hash.
			lines = append(lines, fmt.Sprintf("Git HEAD: detached at %s", branch))
		} else {
			lines = append(lines, fmt.Sprintf("Git branch: %s", branch))
		}
	}

	// Short status (porcelain).
	if statusOut, err := shutil.Output(ctx, dir, "git", "status", "--porcelain"); err == nil {
		statusLines := strutil.SplitBy(statusOut, "\n")
		if len(statusLines) > 0 {
			// Limit to at most 30 lines to avoid blowing up the context.
			if len(statusLines) > 30 {
				statusLines = append(statusLines[:30], "... (truncated)")
			}
			lines = append(lines, "Git status:")
			for _, s := range statusLines {
				lines = append(lines, fmt.Sprintf("  %s", s))
			}
		} else {
			lines = append(lines, "Git status: clean")
		}
	}

	if len(lines) == 0 {
		return nil
	}
	return lines
}
