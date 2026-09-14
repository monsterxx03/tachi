package checkpoint

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	manifestName = "manifest.json"
	manifestVer  = 1
)

// Manifest is a session's checkpoint index. It lives beside the snapshots it
// points at (<session>/checkpoints/manifest.json) and is deliberately NOT part
// of the conversation: a session record would have to be understood by
// ConvertSessionToLLMMessages, and that file's byte-exact reconstruction is
// what the prompt-cache invariant rests on. Internal bookkeeping stays out.
type Manifest struct {
	Version     int      `json:"version"`
	Checkpoints []Record `json:"checkpoints"`
}

// Record is one checkpoint: where the conversation stood, and what the files
// looked like, at the start of a user turn.
type Record struct {
	Turn int `json:"turn"`
	// Records and APIRecords are the lengths of messages.jsonl and
	// api_requests.jsonl at this point. Rewinding truncates BOTH (the request
	// log has to describe the conversation that is still there) but not the
	// usage ledger: tokens were really spent, and a rewind is not a refund.
	Records    int       `json:"records"`
	APIRecords int       `json:"api_records"`
	At         time.Time `json:"at"`
	// UserText is the prompt that started the turn, so a rewind can put it back
	// in the input box (what both Claude Code and Pi do) instead of making the
	// reader retype it.
	UserText string `json:"user_text,omitempty"`
	// Roots holds one entry per workspace root covered by this checkpoint.
	Roots []RootState `json:"roots,omitempty"`
	// Skipped, when set, is why the file snapshot was refused (a guard tripped,
	// git is missing, ...). The conversation half of the checkpoint still
	// exists; a rewind to it must say that no files were restored rather than
	// silently reporting success.
	Skipped string `json:"skipped,omitempty"`
	// Irreversible lists side effects inside this turn that a rewind cannot
	// take back — a git commit being the important one (detected by watching
	// the user's own HEAD move), plus anything the caller knows about.
	Irreversible []string `json:"irreversible,omitempty"`
}

// RootState is one root's snapshot within a checkpoint.
type RootState struct {
	// RootIndex is the root's position in the manager's ordered root set; the
	// repository directory and the ref name are both derived from it, so it is
	// recorded rather than re-derived from the path (a path that no longer
	// resolves would silently fall back to index 0 and touch the wrong repo).
	RootIndex int    `json:"root_index"`
	Root      string `json:"root"`
	Ref       string `json:"ref"`
	Tree      string `json:"tree,omitempty"`
}

// findIndex returns the record for a turn together with its position, for the
// callers that update a record in place (Snapshot fills in the file half of a
// boundary Begin already recorded).
func (m *Manifest) findIndex(turn int) (int, Record, bool) {
	for i, r := range m.Checkpoints {
		if r.Turn == turn {
			return i, r, true
		}
	}
	return 0, Record{}, false
}

// find returns the record for a turn.
func (m *Manifest) find(turn int) (Record, bool) {
	for _, r := range m.Checkpoints {
		if r.Turn == turn {
			return r, true
		}
	}
	return Record{}, false
}

// prune drops the oldest checkpoints beyond retain and returns the removed
// turns, whose refs the caller deletes.
func (m *Manifest) prune(retain int) []Record {
	if retain <= 0 || len(m.Checkpoints) <= retain {
		return nil
	}
	cut := len(m.Checkpoints) - retain
	removed := append([]Record(nil), m.Checkpoints[:cut]...)
	m.Checkpoints = append([]Record(nil), m.Checkpoints[cut:]...)
	return removed
}

// loadManifest reads the manifest, treating a missing file as an empty one: the
// manager is created when a session starts, long before anything is recorded,
// and must not do filesystem work until the first snapshot actually needs it.
func loadManifest(dir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		if os.IsNotExist(err) {
			return &Manifest{Version: manifestVer}, nil
		}
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", manifestName, err)
	}
	return &m, nil
}

// saveManifest writes the manifest atomically: a torn manifest would make the
// session's history of checkpoints unreadable, and a rewind is exactly the
// moment a reader can least afford that.
func saveManifest(dir string, m *Manifest) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, manifestName+".tmp")
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, manifestName))
}
