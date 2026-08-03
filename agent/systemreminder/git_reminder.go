package systemreminder

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

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
	// Only fire if we're inside a git repository.
	if err := exec.Command("git", "rev-parse", "--is-inside-work-tree").Run(); err != nil {
		return nil
	}

	var lines []string

	// Current branch (including detached HEAD state).
	branchOut, err := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err == nil {
		branch := strings.TrimSpace(string(branchOut))
		if branch == "HEAD" {
			// Detached HEAD, show short commit hash.
			commitOut, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
			if err == nil {
				lines = append(lines, fmt.Sprintf("Git HEAD: detached at %s", strings.TrimSpace(string(commitOut))))
			}
		} else {
			lines = append(lines, fmt.Sprintf("Git branch: %s", branch))
		}
	}

	// Short status (porcelain).
	statusOut, err := exec.Command("git", "status", "--porcelain").Output()
	if err == nil {
		statusLines := strutil.SplitBy(string(statusOut), "\n")
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
