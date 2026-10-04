package tui

import (
	"context"
	"os"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/atfile"
	"github.com/monsterxx03/tachi/agent/sessionref"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/fileindex"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

// sessionQueryPrefix is the picker's session namespace — the same spelling the
// expansion scanner splits on, taken from it rather than restated.
const sessionQueryPrefix = atfile.SessionRefPrefix

// maxSessionRowTitle keeps a session row one line wide: a title is written from
// the first user message, so it can be a whole paragraph.
const maxSessionRowTitle = 48

// atMatch is one row of the @-completion popup. Both namespaces produce them —
// files under the working directory, and sessions — so the popup, its keys and
// the insertion have one shape to deal with.
type atMatch struct {
	// Ref is the whole reference the row inserts, '@' included. It comes from the
	// same helpers the expansion scanner reads back (atfile.RefForPath's spelling,
	// atfile.RefForSession), so what the popup offers is what the backend expands.
	Ref string
	// Label is what the row shows: a path, or a session's date and title.
	Label string
	IsDir bool
	// Session marks a @session: row, which is what names the popup's header.
	Session bool
}

// newSessionSource wires the @session: namespace to the agent's session store:
// resolving a reference and searching the popup both read through it, and its
// list is memoized (one snapshot until a write, not one directory walk per
// keystroke — see docs/agents/session-store.md).
//
// nil means the session namespace offers nothing, which is what a frontend with
// no session manager gets.
func newSessionSource(a *agent.AIAgent) *sessionref.Source {
	if a == nil || a.SessionManager() == nil {
		return nil
	}
	dir, err := config.SessionDir()
	if err != nil {
		return nil
	}
	return sessionref.New(a.SessionManager().List, dir)
}

// searchAtFiles searches the working directory for files matching query. An
// empty query lists its immediate entries. The underlying index is cached, so
// this stays cheap enough to run on every keystroke.
func (i *InputArea) searchAtFiles(query string) ([]fileindex.Match, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return i.files.Search(cwd, query, fileindex.DefaultLimit)
}

// atFileRows renders the file matches for query as popup rows.
func (i *InputArea) atFileRows(query string) []atMatch {
	matches, err := i.searchAtFiles(query)
	if err != nil {
		return nil
	}
	rows := make([]atMatch, len(matches))
	for idx, m := range matches {
		rows[idx] = atMatch{Ref: "@" + m.Path, Label: m.Path, IsDir: m.IsDir}
	}
	return rows
}

// atSessionRows renders other conversations as popup rows, ranked by the store
// (a match on the title, the id or the working directory's name).
func (i *InputArea) atSessionRows(query string) []atMatch {
	if i.sessions == nil {
		return nil
	}
	targets := i.sessions.Search(query, i.currentSessionID, fileindex.DefaultLimit)
	rows := make([]atMatch, len(targets))
	for idx, t := range targets {
		rows[idx] = atMatch{Ref: atfile.RefForSession(t.ID), Label: sessionRowLabel(t), Session: true}
	}
	return rows
}

// sessionRowLabel is what a session row shows: when it was last touched and what
// it is called — the two things a reader picks a conversation by.
func sessionRowLabel(t atfile.SessionTarget) string {
	title := t.Title
	if title == "" {
		title = "(untitled)"
	}
	title = strutil.TruncateFitted(title, maxSessionRowTitle)
	if t.UpdatedAt.IsZero() {
		return title
	}
	return t.UpdatedAt.Format(strutil.TimeFormatDateTimeShort) + "  " + title
}

// ExpandAtReferences expands the @-references in a user message for the LLM: text
// files and directories are inlined, images become content parts for multi-modal
// input, binaries are annotated by path, and @session: references become a pointer
// to another conversation's transcript (never its content). The TUI keeps
// displaying the unexpanded text.
func (m *Model) ExpandAtReferences(message string) atfile.Result {
	cwd, _ := os.Getwd()
	result := atfile.Expand(cwd, message, atfile.Options{Sessions: m.input.SessionSource()})
	if result.Refs > 0 {
		m.logger.Info(context.Background(), "at_file: expanded @ references in message",
			"refs", result.Refs, "images", len(result.Images))
	}
	return result
}
