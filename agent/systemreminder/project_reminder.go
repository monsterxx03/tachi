package systemreminder

import (
	"context"
	"os"
	"path/filepath"
)

// projectContextRules is the standing contract that rides WITH every injected
// .tachi.md — on purpose here rather than inside that file: the file belongs to
// whichever repository is being worked in, so rules written into it would only
// apply to that one repo. Emitted by the reminder, they apply to every repo.
//
// Kept terse: this block is paid by every session of every repository that has a
// .tachi.md.
const projectContextRules = `The .tachi.md below is written FOR YOU, an agent, not for a human — dense on purpose. Treat it as fact,
and keep it that way:

- **Keep it true, in the same turn.** Work that invalidates a line — in this file or in a document it
  points at — fixes or deletes it right then. A stale line is worse than a missing one: it costs tokens
  and sends the next session down a wrong path.
- **Convention, not history.** Record how things ARE. A bug that has already been fixed is not context,
  however instructive it felt at the time; a trap another session would plausibly step in again IS.
- **It is injected whole into the first message of every session**, so its length is paid by every
  conversation. When a subject outgrows a line or two, split it into a document beside this file and
  leave an index entry there saying WHEN to read it — the point is fewer lookups before you can work,
  not more prose.`

// ProjectContextReminder injects the contents of .tachi.md (if present) on the
// first message of a brand-new conversation. This gives the model awareness of
// the project context without bloating the static system prompt.
type ProjectContextReminder struct{}

func (ProjectContextReminder) Generate(ctx context.Context, rctx Context) []string {
	if !rctx.IsFirstMessage {
		return nil
	}

	// Read .tachi.md from the turn's working directory (see workDir) — the project
	// the session is pointed at, not the one the process happens to sit in.
	data, err := os.ReadFile(filepath.Join(workDir(ctx), ".tachi.md"))
	if err != nil {
		return nil // No .tachi.md — nothing to inject.
	}

	content := string(data)
	if content == "" {
		return nil
	}

	// The rules come FIRST: they are the contract for the file below them, and a
	// reader that stops early has still seen how to treat what follows.
	return []string{
		"## Project Context (.tachi.md)",
		"",
		projectContextRules,
		"",
		content,
	}
}
