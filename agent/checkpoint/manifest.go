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
	// Diff is what this turn CHANGED, counted from the checkpoint's own two trees
	// (where the turn started vs where it ended). Nil means the turn wrote nothing
	// (so there is no pair to compare); Skipped means there should have been one but
	// it could not be taken. It lives here rather than being recomputed by whoever
	// asks because the turn end is the only moment that knows both trees cheaply —
	// and because every reader of a session (desktop, tui, web) then gets the same
	// numbers without running git.
	Diff *TurnDiff `json:"diff,omitempty"`
}

// TurnDiff is one turn's change summary, from the checkpoint's trees.
type TurnDiff struct {
	Files   int `json:"files"`
	Added   int `json:"added"`
	Removed int `json:"removed"`
	// Skipped, when set, is why there are no numbers: the end-of-turn snapshot was
	// refused (a guard tripped on something the turn created, git failed, ...). The
	// turn itself is unaffected — it already happened — so this travels as a reason
	// rather than as an error, and a reader falls back to what the tool calls declared.
	Skipped string `json:"skipped,omitempty"`
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
	// Head is the user's own git HEAD when the snapshot was taken, empty when the
	// root is not a git repository. Recorded because a rewind can restore the
	// FILES but not the HISTORY: a commit made after this point stays in the log,
	// so a preview has to be able to see that it moved (see Manager.committedSince).
	Head string `json:"head,omitempty"`
	// EndRef and EndTree are the same root where the TURN FINISHED (see
	// Manager.SnapshotEnd), empty when the turn has no end state — it wrote nothing,
	// or the end snapshot was refused. The pair (Tree, EndTree) is what makes a
	// turn's changes readable exactly and frozen: `git diff Tree EndTree` is what
	// this turn did, and neither a later turn nor a `git commit` can move it.
	EndRef  string `json:"end_ref,omitempty"`
	EndTree string `json:"end_tree,omitempty"`
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
