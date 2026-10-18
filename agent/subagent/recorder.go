package subagent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/session"
)

// recorder handles writing sub-agent execution messages to a JSONL file
// under <session-dir>/subagent/<shortID>.jsonl.

type recorder struct {
	file   *os.File
	logger *logger.Logger
}

// newRecorder creates a new recorder for the given session and sub-agent.
//
// root is where the session's own files live — session.SubagentDir resolves and creates the
// subagent/ directory inside it — and the caller takes it from the agent that owns the
// conversation (Agent.SessionBaseDir). It is deliberately not a process-global default: a session
// written through an explicit store would leave its transcripts behind in the real ~/.tachi, where
// nothing that reads the session ever looks.
func newRecorder(root, sessionID, shortID string, l *logger.Logger) (*recorder, error) {
	dir, err := session.SubagentDir(root, sessionID)
	if err != nil {
		return nil, err
	}

	path := filepath.Join(dir, shortID+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}

	return &recorder{file: f, logger: l}, nil
}

// record appends a session.Message as a JSON line to the sub-agent's file.
func (r *recorder) record(msg *session.Message) error {
	msg.Timestamp = time.Now()
	// A sub-agent transcript is display-only, and this writer owns its own file instead of going
	// through the session store that stores image bytes — an image part here would reference
	// bytes that do not exist. Its text already says an image was returned.
	session.DropUnstoredImageParts(msg)
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = r.file.Write(append(data, '\n'))
	return err
}

// close closes the underlying file.
func (r *recorder) close() error {
	return r.file.Close()
}
