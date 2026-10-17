package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// Forking a conversation: copy the HEAD of a session's history into a NEW session
// and leave everything else exactly where it is.
//
// It answers a different question from a rewind (agent/agent_rewind.go): a rewind
// takes the SAME conversation back to a moment, workspace and all, while a fork
// keeps the original conversation — and the disk — untouched and starts a new one
// that ends where the chosen turn begins. "Ask this again, without losing the
// branch I am on."
//
// Three things are therefore copied differently, each on purpose:
//
//   - The checkpoints are NOT copied. A session's manifest and its snapshot store
//     live inside its own directory, so the turns a fork inherits have no file
//     state in the new session: they cannot be rewound and their per-turn numbers
//     (the footer's diff, the chain list) are absent, while the fork's own first
//     turn begins a fresh manifest at turn 1. Handing the fork the SOURCE's
//     manifest instead would claim file states whose refs live in another
//     session's directory — a rewind of the fork would then be file-restoring
//     trees it does not own.
//
//   - The workspace is NOT touched: nothing is restored and nothing is deleted.
//     The fork's first write is snapshotted as its own turn 1, against the tree as
//     it stands then.
//
//   - The usage ledger is not copied, because it is keyed by session id: a fork
//     starts at zero cost ("积分" and the cache-hit rate included). The records
//     themselves carry their own Usage, so the context ring still reads the right
//     size — see desktop's estimateFromMessages.
//
// What the reader is left to be told about is the rest of the session's own
// files: pasted/, subagent/ and oneoff/ stay behind, and a copied artifact
// reminder names a path inside the ORIGINAL session's directory (absolute, so it
// still resolves while that session exists).

// ForkSession copies src's history into a new session and returns it. src is not
// modified in any way.
//
// keepRecords and keepAPI are counts of RECORDS, never turns: the caller gets them
// from the checkpoint that starts the turn being forked at
// (checkpoint.Record.Records/APIRecords), which is exactly the point where that
// turn's records begin — reminder wrapper included, so the new history ends on a
// clean turn boundary rather than with a dangling reminder block.
//
// title is the new session's title as it will be shown. It is passed in rather
// than derived, because the words a reader sees belong to the frontend that shows
// them.
func (s *FileStore) ForkSession(src *Session, keepRecords, keepAPI int, title string) (*Session, error) {
	if src == nil || src.ID == "" {
		return nil, errors.New("session: fork needs a source session")
	}

	msgs, err := s.readHistoryFile(s.messagesPath(src.ID))
	if err != nil {
		return nil, err
	}
	// Byte-for-byte, never a re-append: re-appending through Manager.AppendMessage
	// would RESTAMP every copied record with the current time. The desktop pages a
	// transcript by timestamp (pageSessionMessages keeps strictly older records),
	// so a batch of records sharing one timestamp loses the records between two
	// pages — a silently truncated history.
	head, _, _ := splitJSONLAt(msgs, keepRecords)
	imageRefs := imageRefsIn(head)

	reqs, err := s.readHistoryFile(s.apiRequestsPath(src.ID))
	if err != nil {
		return nil, err
	}
	apiHead, _, _ := splitJSONLAt(reqs, keepAPI)

	fork := forkedSession(src, title)
	dir := s.sessionDir(fork.ID)

	// The session directory is built WHOLE and published by its meta.json, which
	// is written last: ListSessions scans the base directory for meta.json, so a
	// fork interrupted halfway leaves an unreadable directory rather than a session
	// that exists in the list with a truncated conversation. The failure path
	// removes it again — nothing else would ever collect it (the cleanup sweep only
	// walks sessions it can list).
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create session dir: %w", err)
	}
	if err := fileutil.AtomicWriteFilePrivate(s.messagesPath(fork.ID), head); err != nil {
		s.discardFork(dir)
		return nil, fmt.Errorf("write messages.jsonl: %w", err)
	}
	if err := s.copyImageBlobs(src.ID, fork.ID, imageRefs); err != nil {
		s.discardFork(dir)
		return nil, err
	}
	if len(apiHead) > 0 {
		if err := fileutil.AtomicWriteFilePrivate(s.apiRequestsPath(fork.ID), apiHead); err != nil {
			s.discardFork(dir)
			return nil, fmt.Errorf("write api_requests.jsonl: %w", err)
		}
	}
	if err := fileutil.AtomicWriteJSONPrivate(s.metaPath(fork.ID), fork); err != nil {
		s.discardFork(dir)
		return nil, fmt.Errorf("write meta.json: %w", err)
	}

	s.invalidate()
	return fork, nil
}

// readHistoryFile reads one of a session's two history files. A file that does not
// exist is an empty history rather than an error: CreateSession writes
// messages.jsonl but api_requests.jsonl appears on its first write, and a session
// whose history is empty forks into an empty session (the workspace and the title
// are still worth having).
func (s *FileStore) readHistoryFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return data, nil
}

// discardFork removes a session directory whose fork did not finish. Best effort:
// the directory is invisible to every reader already (no meta.json means
// scanSessions skips it), so a failure here costs disk, not correctness.
func (s *FileStore) discardFork(dir string) {
	if err := os.RemoveAll(dir); err != nil {
		logger.New("session").Warn(context.Background(),
			"session: could not remove an unfinished fork", "dir", dir, "err", err)
	}
}

// forkedSession builds the new session's meta as a WHITELIST copy of the source's.
//
// A whole-record copy is the tempting shortcut and it is wrong, because the fields
// that describe a session's SITUATION are not the conversation:
//
//   - ThreadID: the channel layer finds a session by it (FindByThreadID). Copying
//     it would make an incoming channel message land in the fork.
//   - CompactedChildID/CompactedParentID/CompactedParentTitle: the desktop renders
//     those links as a compaction CHAIN — the parent loses its own row and appears
//     folded under its child as 「压缩前」 — and the agent refuses a rewind on a
//     session that was compacted onwards. A fork is a sibling, not a successor.
//   - Kind/SourceNodeID: a subsystem owns such a session's turns; the fork is an
//     ordinary conversation the reader drives, so it must not claim a kind it was
//     never granted (agent/managed.go authorizes by it).
//   - LogicalID: it names the conversation compaction continues INTO. The fork is a
//     different conversation that happens to share a prefix.
//   - Pinned: a pin is a promise about a row the reader keeps at hand; the source
//     keeps its own and the fork arrives unpinned with the newest timestamp.
//
// What IS copied is what makes the fork the same place to work: the provider and
// thinking level (otherwise the copied prefix stops matching the provider's prompt
// cache on the first request), the roots and the project binding (otherwise the
// conversation's paths do not resolve), and SkipDream, which is the reader's own
// standing choice about that conversation. SkipApproval is the same kind of
// standing choice — a branch of a conversation being worked in 「无需审批」 stays
// that way — and it is the one field here that a COMPACTION does not carry (see
// agent/compact.go): a compacted conversation is a different one.
func forkedSession(src *Session, title string) *Session {
	now := time.Now()
	return &Session{
		ID:             GenerateID(),
		Title:          title,
		ProviderName:   src.ProviderName,
		WorkingDir:     src.WorkingDir,
		AdditionalDirs: slices.Clone(src.AdditionalDirs),
		ProjectID:      src.ProjectID,
		ThinkingLevel:  src.ThinkingLevel,
		SkipDream:      src.SkipDream,
		SkipApproval:   src.SkipApproval,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}

// imageRefsIn collects the image blobs the kept records refer to. Message.Parts
// holds only a reference (the bytes live in the session's images/ directory), so a
// fork that copies the records and not the blobs silently loses the images: the
// conversion degrades the part away and the model stops being able to see an image
// it was in fact given.
//
// A line only has image parts if it has a parts array at all, and the vast majority
// of records have neither — hence the cheap byte test before unmarshalling. A line
// that does not parse is skipped rather than failing the fork: the copy is
// byte-exact either way, and the only thing lost would be a blob for a record no
// reader could decode.
func imageRefsIn(head []byte) []string {
	var refs []string
	seen := make(map[string]bool)
	for _, line := range bytes.Split(head, []byte("\n")) {
		if len(line) == 0 || !bytes.Contains(line, []byte(`"parts"`)) {
			continue
		}
		var msg Message
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		for _, part := range msg.Parts {
			if part.Type != PartTypeImage || part.Ref == "" || seen[part.Ref] {
				continue
			}
			seen[part.Ref] = true
			refs = append(refs, part.Ref)
		}
	}
	return refs
}

// copyImageBlobs copies the blobs those references name from one session's blob
// directory to another's. Blobs are content-addressed, so the destination name IS
// the source name and there is nothing to translate.
//
// A blob that cannot be read is reported and skipped, not fatal: it is already
// missing (or unresolvable) in the SOURCE session, where the same part degrades the
// same way, and failing the fork over it would take the whole conversation away
// from the reader who came to re-ask a question.
func (s *FileStore) copyImageBlobs(srcID, dstID string, refs []string) error {
	for _, ref := range refs {
		// imagePath validates the reference: a record's ref is a file name, and a
		// corrupt or hostile one must not be able to name a file outside the dir.
		srcPath, err := s.imagePath(srcID, ref)
		if err != nil {
			logger.New("session").Warn(context.Background(),
				"session: fork skipped an unusable image reference", "session", srcID, "ref", ref, "err", err)
			continue
		}
		data, err := os.ReadFile(srcPath)
		if err != nil {
			logger.New("session").Warn(context.Background(),
				"session: fork could not read an image blob", "session", srcID, "ref", ref, "err", err)
			continue
		}
		dstPath, err := s.imagePath(dstID, ref)
		if err != nil {
			return fmt.Errorf("resolve image blob: %w", err)
		}
		if err := fileutil.WriteFilePrivate(dstPath, data); err != nil {
			return fmt.Errorf("copy image blob: %w", err)
		}
	}
	return nil
}
