package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

// receivedDirName is where files that arrived from ANOTHER MACHINE land: beside pasted/, images/,
// oneoff/, subagent/ and rewound/, inside the session's own directory.
//
// Both ends of the boundary the node crosses write here — a file a peer attached to its message,
// and a file a peer produced that this machine's delegation read back — because both are the same
// thing from the conversation's point of view: bytes that came from outside and belong to it. Dying
// with the conversation is the point, and so is NOT landing in the user's workspace: a file there
// shows up in their diff, and one they did not put there is not theirs to clean up.
const receivedDirName = "received"

// SaveReceivedFile writes bytes that came from elsewhere into the session's own directory and
// returns the local path that reads them back.
//
// The NAME is the other machine's word for it, so it is untrusted in two ways, and both are
// refusals rather than repairs:
//
//   - it must be a single path element. `DirFor` already refuses a bad session id; the same rule
//     has to hold for the file name, or a peer writing `../../x` would be choosing a path on this
//     machine instead of a name inside the session.
//   - a name already taken gets a short suffix instead of overwriting. Two peers (or the same peer
//     twice) sending `report.pdf` are two files, and replacing the first silently is data loss
//     nobody would see — the reader gets a valid path either way.
//
// There is no size limit here: what may cross the boundary is the CALLER's policy (per direction,
// with its own refusal to report), and this layer's job is the layout. An empty payload is refused
// because a zero-byte file is a broken transfer, not a file.
func SaveReceivedFile(sessionID, name string, raw []byte) (string, error) {
	if len(raw) == 0 {
		return "", errors.New("session: no bytes to store")
	}
	if name == "" || !fileutil.IsSinglePathElement(name) {
		return "", fmt.Errorf("session: %q is not a usable file name", name)
	}
	dir, err := DirFor(sessionID)
	if err != nil {
		return "", err
	}
	sub := filepath.Join(dir, receivedDirName)
	if err := os.MkdirAll(sub, 0o700); err != nil {
		return "", fmt.Errorf("session: create received dir: %w", err)
	}
	path := filepath.Join(sub, name)
	if _, err := os.Stat(path); err == nil {
		path = filepath.Join(sub, receivedFileName(name))
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", fmt.Errorf("session: write received file: %w", err)
	}
	return path, nil
}

// receivedFileName is a taken name made unique: the original (so the reader still recognises what
// the other machine called it), a short uuid, and the extension last (so it stays openable).
func receivedFileName(name string) string {
	ext := filepath.Ext(name)
	return name[:len(name)-len(ext)] + "-" + strutil.ShortUUID(8) + ext
}
