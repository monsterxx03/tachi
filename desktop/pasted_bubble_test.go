package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/session"
)

// TestUserRecordImagesPointAtTheBlobs pins the reload half of "a pasted picture stays a picture":
// the record on disk holds an image part with only a REFERENCE (the bytes live in the session's
// images/ directory), and the payload the transcript rebuilds from has to name the file it can
// draw — otherwise a reloaded conversation showed the pasted image's @-reference as text and the
// picture itself vanished at the first restart.
//
// It goes through the same two functions the real path uses (pageSessionMessages → resolver →
// buildSessionMessages), because the fact being pinned is that the hop between them resolves.
func TestUserRecordImagesPointAtTheBlobs(t *testing.T) {
	_, svc, sid, dir := newDeleteApp(t)
	store := svc.desk.sessionStore()
	if store == nil {
		t.Fatal("no session store")
	}

	// A real 1x1 PNG, as an image part on the way IN (bytes, no reference yet).
	const pngB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg=="
	img := &session.Message{
		Type:    session.MessageTypeUser,
		Content: "这张图是什么\n\n@/tmp/paste-1.png",
		Parts: []session.Part{
			{Type: session.PartTypeText, Text: "这张图是什么"},
			{Type: session.PartTypeImage, MediaType: "image/png", Data: pngB64},
		},
	}
	if err := store.AppendMessage(sid, img); err != nil {
		t.Fatalf("append: %v", err)
	}

	raw, err := store.LoadMessages(sid)
	if err != nil || len(raw) == 0 {
		t.Fatalf("load: %v (%d records)", err, len(raw))
	}
	sms := buildSessionMessages(raw, 0, nil, svc.sessionImagePath(sid))
	if len(sms) != 1 {
		t.Fatalf("got %d payload messages, want 1", len(sms))
	}
	if len(sms[0].Images) != 1 {
		t.Fatalf("images = %v, want the one image part", sms[0].Images)
	}
	path := sms[0].Images[0]
	// Inside the session's own images/ directory — the blob, not the file the reference named.
	blobDir := filepath.Join(dir, "images") + string(filepath.Separator)
	if !strings.HasPrefix(path, blobDir) {
		t.Errorf("image path %q is not under %q", path, blobDir)
	}
	want, err := base64.StdEncoding.DecodeString(pngB64)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the payload points at a file that is not there: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("blob holds %d bytes, want the pasted %d", len(got), len(want))
	}
}

// TestUserRecordImagesSkipsWhatIsGone: a record can name an image the disk no longer has (a record
// copied from another machine, a blob deleted by hand). The payload has to leave it out rather than
// hand the transcript a path that draws an empty frame.
func TestUserRecordImagesSkipsWhatIsGone(t *testing.T) {
	missing := strings.Repeat("a", 64) // a well-formed reference, as session.isImageRef demands
	rec := session.Message{
		Type:  session.MessageTypeUser,
		Parts: []session.Part{{Type: session.PartTypeImage, Ref: missing}},
	}
	resolve := func(ref string) string {
		if ref == missing {
			return "" // the resolver's answer for "no such blob"
		}
		return "/somewhere/" + ref
	}
	if got := userRecordImages(rec, resolve); len(got) != 0 {
		t.Errorf("images = %v, want none", got)
	}
	// And no resolver at all (a one-off transcript) is not a crash.
	if got := userRecordImages(rec, nil); got != nil {
		t.Errorf("images = %v, want nil without a resolver", got)
	}
}
