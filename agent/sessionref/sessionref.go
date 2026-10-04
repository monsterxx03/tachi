// Package sessionref resolves @session: references and backs the session picker:
// "which conversation is this" is answered from the session store, never from a
// second copy of it.
//
// The store's own reader does the work. ListSessions is memoized — one directory
// walk serves every caller until a write or another process moves the directory
// (see docs/agents/session-store.md) — which is what makes a per-keystroke search
// affordable, and it is why a Source takes a lister rather than walking the disk
// itself.
package sessionref

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/monsterxx03/tachi/agent/atfile"
	"github.com/monsterxx03/tachi/pkg/strutil"
	"github.com/monsterxx03/tachi/session"
)

// DefaultLimit caps how many rows a search returns when the caller names no
// limit, matching the @-file picker's.
const DefaultLimit = 20

// listFunc returns every session in the store, newest first. A caller passes
// *session.FileStore's ListSessions (or the manager's List) so the memoized
// snapshot is shared with every other reader.
type listFunc func() ([]*session.Session, error)

// Source resolves and searches the sessions of one store directory.
type Source struct {
	list listFunc
	dir  string
}

// New returns a Source over the sessions list lists, stored under dir
// (config.SessionDir() for a real one, t.TempDir() in a test).
func New(list listFunc, dir string) *Source { return &Source{list: list, dir: dir} }

// Search ranks the sessions whose title, id or working directory matches query —
// higher first, ties broken by the most recently updated. An empty query keeps
// every session (the picker's opening list). excludeID drops one session from
// the result: the picker opens INSIDE a conversation, and referencing that same
// conversation is never what the reader meant.
//
// A row's Messages stays 0 — the count costs a read of the transcript and belongs
// to the ONE reference that resolved, not to every keystroke.
func (s *Source) Search(query, excludeID string, limit int) []atfile.SessionTarget {
	sessions, err := s.list()
	if err != nil {
		return nil
	}

	type scored struct {
		target atfile.SessionTarget
		score  int
	}
	ranked := make([]scored, 0, len(sessions))
	for _, sess := range sessions {
		if sess.ID == excludeID {
			continue
		}
		score, ok := matchScore(query, sess)
		if !ok {
			continue
		}
		ranked = append(ranked, scored{target: s.target(sess, false), score: score})
	}

	slices.SortStableFunc(ranked, func(a, b scored) int {
		if a.score != b.score {
			return b.score - a.score
		}
		return b.target.UpdatedAt.Compare(a.target.UpdatedAt)
	})

	if limit = normalizeLimit(limit); len(ranked) > limit {
		ranked = ranked[:limit]
	}
	targets := make([]atfile.SessionTarget, len(ranked))
	for i, r := range ranked {
		targets[i] = r.target
	}
	return targets
}

// Resolve returns the session a reference names: a full id, or a unique SUFFIX of
// one. The suffix is the useful half — a store id is a creation timestamp followed
// by a random tail, so sessions made the same day share their whole front, and the
// tail is what a human retypes. An ambiguous suffix resolves to nothing rather
// than to an arbitrary session.
func (s *Source) Resolve(id string) (atfile.SessionTarget, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return atfile.SessionTarget{}, false
	}
	sessions, err := s.list()
	if err != nil {
		return atfile.SessionTarget{}, false
	}

	var suffix *session.Session
	for _, sess := range sessions {
		if sess.ID == id {
			return s.target(sess, true), true
		}
		if strings.HasSuffix(sess.ID, id) {
			if suffix != nil {
				return atfile.SessionTarget{}, false // ambiguous
			}
			suffix = sess
		}
	}
	if suffix == nil {
		return atfile.SessionTarget{}, false
	}
	return s.target(suffix, true), true
}

// target describes one stored session. withCount adds the transcript's record
// count, which costs a read of the file.
func (s *Source) target(sess *session.Session, withCount bool) atfile.SessionTarget {
	t := atfile.SessionTarget{
		ID:         sess.ID,
		Title:      sess.Title,
		UpdatedAt:  sess.UpdatedAt,
		WorkingDir: sess.WorkingDir,
		Transcript: s.transcriptPath(sess.ID),
	}
	if withCount {
		t.Messages = countLines(t.Transcript)
	}
	return t
}

// transcriptPath locates a session's message log. The layout — <dir>/<id>/
// messages.jsonl — belongs to the store; this is its read side for a pointer,
// and the id is a path element that came from the store's own list, never from
// the user's typing.
func (s *Source) transcriptPath(id string) string {
	return filepath.Join(s.dir, id, "messages.jsonl")
}

// matchScore scores one session against the picker's query: the best of its
// title, its id and its working directory. Only the directory's BASE NAME is
// scored — a whole path matches on every ancestor otherwise, so every session of
// one project would rank alike and the query would have narrowed nothing.
func matchScore(query string, sess *session.Session) (int, bool) {
	if strings.TrimSpace(query) == "" {
		return 0, true
	}
	best, ok := strutil.FuzzyScore(query, sess.Title)
	for _, candidate := range []string{sess.ID, filepath.Base(sess.WorkingDir)} {
		if candidate == "" || candidate == "." {
			continue
		}
		if score, hit := strutil.FuzzyScore(query, candidate); hit && (!ok || score > best) {
			best, ok = score, true
		}
	}
	return best, ok
}

// countLines counts the records in a session's messages.jsonl without parsing it:
// one line is one record. A last line with no trailing newline still counts.
func countLines(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	count := 0
	unterminated := false
	buf := make([]byte, 64*1024)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			count += bytes.Count(buf[:n], []byte{'\n'})
			unterminated = buf[n-1] != '\n'
		}
		if err != nil {
			break
		}
	}
	if unterminated {
		count++
	}
	return count
}

// normalizeLimit clamps a search limit to DefaultLimit when unset.
func normalizeLimit(limit int) int {
	if limit <= 0 {
		return DefaultLimit
	}
	return limit
}
