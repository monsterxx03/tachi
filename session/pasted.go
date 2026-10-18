package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

// pastedDirName is where a session's pasted images live: beside images/, oneoff/, subagent/ and
// rewound/, inside the session's own directory. A screenshot belongs to the conversation it was
// sent in and dies with it — deleting the session removes the whole directory — and it must not
// land in the user's workspace, where it would show up in their diff.
const pastedDirName = "pasted"

// ErrNoSessionID reports a question about per-session state when there is no session to ask about.
// Named so a frontend can say it in its own words: the desktop's guard messages are the reader's
// language, the rest of this package's errors are not.
var ErrNoSessionID = errors.New("session: no session id")

// ErrNoSessionDir reports that the caller cannot say WHERE a session's files live — its store
// holds it somewhere it cannot name (see Manager.BaseDir). Named because every caller that writes
// a session's own files needs the same refusal: joining an empty root with an id gives a RELATIVE
// path, and a pasted image landing next to the process's working directory is worse than one that
// is refused.
var ErrNoSessionDir = errors.New("session: no session directory")

// DirFor resolves the directory a session's own files live in: root/<id>.
//
// It is the ONE statement of that layout, which is why callers ask it rather than join a root
// themselves — and why the id, which can arrive from a webview, must be a single path element:
// a name like "../../other-session" would otherwise point somewhere else entirely, and the caller
// is one file write — or one `open` — away from touching it.
//
// The root is a PARAMETER, not config.SessionDir(): it is the directory the store that HOLDS the
// session keeps it in (session.Manager.BaseDir), so a session written through an explicit store —
// a node's per-context manager, a test's temp store — keeps its own files beside it instead of
// scattering them into the real ~/.tachi.
func DirFor(root, id string) (string, error) {
	if root == "" {
		return "", ErrNoSessionDir
	}
	if id == "" {
		return "", ErrNoSessionID
	}
	if !fileutil.IsSinglePathElement(id) {
		return "", fmt.Errorf("session: %q is not a session id", id)
	}
	return filepath.Join(root, id), nil
}

// SavePastedImage writes an image's bytes into the session's own directory, under pasted/, and
// returns the path an @-reference can point at.
//
// The bytes arrive with no name — a screenshot on the clipboard is not a file — so the name is
// generated: the time for the eye and a short uuid for uniqueness, since a millisecond timestamp
// alone collides when two pastes land in the same one and a lost screenshot is not a failure the
// reader could notice.
//
// An EMPTY sessionID stores under the OS temp directory instead: a frontend can hold a paste before
// any conversation exists (a fresh TUI opens one with its first turn). Nothing durable is lost by
// that — this file is only the carrier the @-reference of that turn needs, and the bytes themselves
// are recorded with the message when it is sent (the store's images/ blobs), so the image still
// survives a reload from the record.
//
// The size budget is enforced here as well as by the callers, which check it first so they can say
// so while the screenshot is still on the clipboard: an oversized file would sit in the session
// forever and never be seen by anyone (expansion annotates it as too large and attaches nothing).
//
// root is where the session's files live — the directory the store that holds the conversation
// keeps it in (session.Manager.BaseDir); see DirFor for why it is a parameter and not a lookup.
func SavePastedImage(root, sessionID, ext string, raw []byte) (string, error) {
	if len(raw) == 0 {
		return "", errors.New("session: no image bytes to store")
	}
	if int64(len(raw)) > llm.MaxImageSize {
		return "", fmt.Errorf("session: image is %s, over the %s limit",
			strutil.HumanBytes(int64(len(raw))), strutil.HumanBytes(llm.MaxImageSize))
	}
	path, err := pastedPath(root, sessionID, ext)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// pastedPath resolves where one paste will be written, creating the directory that holds it.
//
// With no session the path comes from CreateTemp: the temp directory's contents are not this
// package's to name, and reserving the file is what keeps two pastes in the same second apart.
func pastedPath(root, sessionID, ext string) (string, error) {
	if sessionID == "" {
		f, err := os.CreateTemp("", "tachi-paste-*"+ext)
		if err != nil {
			return "", err
		}
		path := f.Name()
		if err := f.Close(); err != nil {
			return "", err
		}
		return path, nil
	}
	dir, err := DirFor(root, sessionID)
	if err != nil {
		return "", err
	}
	sub := filepath.Join(dir, pastedDirName)
	if err := os.MkdirAll(sub, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(sub, pastedFileName(ext)), nil
}

// pastedFileName is the generated name of one pasted image: the prefix (so the file is recognisable
// among the session's other files), the time, and a short uuid.
func pastedFileName(ext string) string {
	return "paste-" + time.Now().Format("20060102-150405") + "-" + strutil.ShortUUID(8) + ext
}
