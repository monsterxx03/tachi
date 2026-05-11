package subagent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/debuglog"
	"github.com/monsterxx03/tachi/session"
)

// recorder handles writing sub-agent execution messages to a JSONL file
// under <session-dir>/subagent/<shortID>.jsonl.
type recorder struct {
	file   *os.File
	logger *debuglog.Logger
}

// newRecorder creates a new recorder for the given session and sub-agent.
func newRecorder(sessionID, shortID string) (*recorder, error) {
	sessionDir, err := config.SessionDir()
	if err != nil {
		return nil, err
	}

	dir := filepath.Join(sessionDir, sessionID, "subagent")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}

	path := filepath.Join(dir, shortID+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}

	return &recorder{file: f, logger: debuglog.DefaultLogger}, nil
}

// record appends a session.Message as a JSON line to the sub-agent's file.
func (r *recorder) record(msg *session.Message) error {
	msg.Timestamp = time.Now()
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