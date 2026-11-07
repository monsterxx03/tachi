package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/monsterxx03/tachi/pkg/fileutil"
)

// TrimResult is what TrimToolResults did: how many records were rewritten, and
// where the untouched original went.
//
// The original is never deleted. A trim throws away text, and the only thing that
// makes throwing it away safe is that the text is still there when someone asks
// for it — the same bargain a rewind makes with the tail it cuts.
type TrimResult struct {
	Trimmed int
	Sidecar string // empty when nothing was rewritten
}

// trimmedDirName holds the pre-trim copy of a session's messages.jsonl, beside
// rewound/ and subagent/. It lives INSIDE the session directory on purpose: what a
// trim removed belongs to the session it was removed from, and dies with it.
const trimmedDirName = "trimmed"

// TrimToolResults rewrites the Result of the named tool_result records — keyed by
// tool call id, valued by the replacement text — and moves the untouched file into
// the session's trimmed/ directory under tag.
//
// Only the target lines are re-marshaled. Every other line is copied byte for byte,
// and that is the property the whole operation rests on: a stored record is what the
// next request is rebuilt from, so a rewrite that re-serialized an untouched record
// (an `any` field re-encoded, a float rounded) would change the conversation's
// prefix and invalidate the provider's prompt cache from the first such line to the
// end (docs/agents/prompt-cache.md). A trim may only change what it was asked to
// change.
//
// Parts are dropped with the text: an image a result carried is part of what the
// trim exists to stop sending. Its blob stays under images/ — a trim deletes nothing
// — it is simply no longer referenced.
func (s *FileStore) TrimToolResults(id string, replacements map[string]string, tag string) (TrimResult, error) {
	if len(replacements) == 0 {
		return TrimResult{}, nil
	}
	src := s.messagesPath(id)
	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return TrimResult{}, nil // nothing recorded yet
		}
		return TrimResult{}, fmt.Errorf("read messages.jsonl: %w", err)
	}

	var out bytes.Buffer
	out.Grow(len(data))
	trimmed := 0
	rest := data
	for len(rest) > 0 {
		line := rest
		hasNewline := false
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line, rest, hasNewline = rest[:i], rest[i+1:], true
		} else {
			rest = nil
		}
		if rewritten, ok := trimResultLine(line, replacements); ok {
			out.Write(rewritten)
			trimmed++
		} else {
			out.Write(line)
		}
		if hasNewline {
			out.WriteByte('\n')
		}
	}

	if trimmed == 0 {
		return TrimResult{}, nil
	}

	dst := filepath.Join(s.sessionDir(id), trimmedDirName, "messages-"+tag+".jsonl")
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return TrimResult{}, fmt.Errorf("create trimmed dir: %w", err)
	}
	// Sidecar BEFORE the rewrite, the same order truncateJSONL uses: a crash in
	// between leaves the original stored twice rather than losing the text the trim
	// was supposed to keep.
	if err := fileutil.AtomicWriteFilePrivate(dst, data); err != nil {
		return TrimResult{}, fmt.Errorf("write sidecar: %w", err)
	}
	if err := fileutil.AtomicWriteFilePrivate(src, out.Bytes()); err != nil {
		return TrimResult{}, fmt.Errorf("rewrite messages.jsonl: %w", err)
	}
	return TrimResult{Trimmed: trimmed, Sidecar: dst}, nil
}

// trimResultLine rewrites one JSONL line when it is a tool_result record the caller
// named, and reports whether it did.
//
// A line that does not parse is left exactly as it is: this function is not a
// validator, and a record it cannot read is not one it may drop.
func trimResultLine(line []byte, replacements map[string]string) ([]byte, bool) {
	if len(line) == 0 {
		return nil, false
	}
	var msg Message
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil, false
	}
	if msg.Type != MessageTypeToolResult {
		return nil, false
	}
	replacement, ok := replacements[msg.ToolCallID]
	if !ok || msg.ToolCallID == "" {
		return nil, false
	}
	msg.Result = replacement
	msg.Parts = nil
	rewritten, err := json.Marshal(&msg)
	if err != nil {
		return nil, false
	}
	return rewritten, true
}
