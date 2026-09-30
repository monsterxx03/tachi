package session

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newBlobTestStore returns a store plus a session to append to.
func newBlobTestStore(t *testing.T) (*FileStore, string) {
	t.Helper()
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	id := GenerateID()
	if err := store.CreateSession(&Session{ID: id, Title: "blobs"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return store, id
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// TestAppendMessage_StoresImagePartsAsBlobs is the write half of the bargain: a record never
// carries image bytes (a conversation with screenshots in it would be unreadable JSONL, and the
// line scanner's buffer is what breaks first), and the reference it leaves behind really does
// resolve to the bytes that were sent.
func TestAppendMessage_StoresImagePartsAsBlobs(t *testing.T) {
	store, id := newBlobTestStore(t)
	msg := &Message{
		Type: MessageTypeToolResult, Name: "ReadFile", Result: "[Image: a.png]",
		Parts: []Part{
			{Type: PartTypeImage, MediaType: "image/png", Data: b64("png bytes")},
		},
	}
	if err := store.AppendMessage(id, msg); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	raw, err := os.ReadFile(store.messagesPath(id))
	if err != nil {
		t.Fatalf("read messages.jsonl: %v", err)
	}
	if strings.Contains(string(raw), b64("png bytes")) {
		t.Errorf("the record carries the image inline: %s", raw)
	}
	if !strings.Contains(string(raw), `"ref":"`) {
		t.Errorf("the record carries no blob reference: %s", raw)
	}

	msgs, err := store.LoadMessages(id)
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	if len(msgs) != 1 || len(msgs[0].Parts) != 1 {
		t.Fatalf("expected one record with one part, got %+v", msgs)
	}
	part := msgs[0].Parts[0]
	if !isImageRef(part.Ref) {
		t.Fatalf("expected a content-hash reference, got %q", part.Ref)
	}
	if part.Data != "" {
		t.Errorf("a loaded record must come back with a reference, not bytes: %q", part.Data)
	}
	stored, err := store.LoadImage(id, part.Ref)
	if err != nil {
		t.Fatalf("LoadImage: %v", err)
	}
	if string(stored) != "png bytes" {
		t.Errorf("blob holds %q, want the bytes that were sent", stored)
	}
}

// TestAppendMessage_DedupesIdenticalImages: the same screenshot pasted, then read back by a
// tool, is one blob — content addressing is what makes repeat storage free.
func TestAppendMessage_DedupesIdenticalImages(t *testing.T) {
	store, id := newBlobTestStore(t)
	for i := 0; i < 2; i++ {
		if err := store.AppendMessage(id, &Message{
			Type:  MessageTypeUser,
			Parts: []Part{{Type: PartTypeImage, MediaType: "image/png", Data: b64("same image")}},
		}); err != nil {
			t.Fatalf("AppendMessage %d: %v", i, err)
		}
	}

	entries, err := os.ReadDir(filepath.Join(store.sessionDir(id), imagesDirName))
	if err != nil {
		t.Fatalf("read images dir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("expected one blob for two identical images, got %d: %v", len(entries), entries)
	}
}

// TestAppendMessage_UnstorableImageIsDropped: a turn's tool result must survive a broken
// attachment. The record is written without the part — the shape every pre-existing record
// already has — rather than the append failing and the turn's work disappearing.
func TestAppendMessage_UnstorableImageIsDropped(t *testing.T) {
	store, id := newBlobTestStore(t)
	if err := store.AppendMessage(id, &Message{
		Type: MessageTypeToolResult, Result: "output",
		Parts: []Part{
			{Type: PartTypeText, Text: "kept"},
			{Type: PartTypeImage, MediaType: "image/png", Data: "not base64!!"},
			{Type: PartTypeImage, MediaType: "image/png", Data: ""},
		},
	}); err != nil {
		t.Fatalf("AppendMessage must not fail over an unusable image: %v", err)
	}

	msgs, err := store.LoadMessages(id)
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	if len(msgs[0].Parts) != 1 || msgs[0].Parts[0].Type != PartTypeText {
		t.Errorf("expected only the text part to survive, got %+v", msgs[0].Parts)
	}
}

// TestLoadImage_RefusesAnythingButAHash: the reference is a file name taken from a record, so a
// corrupt or hostile record must not be able to name a file outside the blob directory.
func TestLoadImage_RefusesAnythingButAHash(t *testing.T) {
	store, id := newBlobTestStore(t)
	for _, ref := range []string{"", "../../../etc/passwd", "/etc/passwd", strings.Repeat("z", imageRefLen), strings.Repeat("a", imageRefLen-1)} {
		if _, err := store.LoadImage(id, ref); err == nil {
			t.Errorf("LoadImage(%q) must be refused", ref)
		}
	}
}

// TestLoadImage_MissingBlobIsAnError keeps the read side honest: a conversion degrades from this
// error (dropping the part it cannot rebuild), and it can only do that if it is told.
func TestLoadImage_MissingBlobIsAnError(t *testing.T) {
	store, id := newBlobTestStore(t)
	if _, err := store.LoadImage(id, strings.Repeat("a", imageRefLen)); err == nil {
		t.Error("expected an error for a reference with no blob behind it")
	}
}

// TestDropUnstoredImageParts covers the writers that own their own file (the subagent and
// one-off transcripts): they cannot store bytes, so they must not publish a part that claims an
// image no reader could produce. A part that already has a reference is left alone.
func TestDropUnstoredImageParts(t *testing.T) {
	msg := &Message{Parts: []Part{
		{Type: PartTypeText, Text: "kept"},
		{Type: PartTypeImage, MediaType: "image/png", Data: b64("no store here")},
		{Type: PartTypeImage, MediaType: "image/png", Ref: strings.Repeat("a", imageRefLen)},
	}}
	DropUnstoredImageParts(msg)

	if len(msg.Parts) != 2 {
		t.Fatalf("expected the unstored image to go and the rest to stay, got %+v", msg.Parts)
	}
	if msg.Parts[0].Type != PartTypeText || msg.Parts[1].Ref == "" {
		t.Errorf("wrong parts survived: %+v", msg.Parts)
	}
}

// TestDeleteSession_RemovesBlobs: a session's images live inside its directory, so retention
// cannot leave them behind.
func TestDeleteSession_RemovesBlobs(t *testing.T) {
	store, id := newBlobTestStore(t)
	if err := store.AppendMessage(id, &Message{
		Type:  MessageTypeUser,
		Parts: []Part{{Type: PartTypeImage, MediaType: "image/png", Data: b64("png bytes")}},
	}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if err := store.DeleteSession(id); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := os.Stat(store.sessionDir(id)); !os.IsNotExist(err) {
		t.Errorf("session directory survived deletion: %v", err)
	}
}
