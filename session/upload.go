package session

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/monsterxx03/tachi/pkg/fileutil"
)

// uploadsDirName is where files a FRONTEND SENDS (a phone's photo or document) land: beside
// pasted/, inside the session's own directory.
//
// It is a different directory from pasted/ for one reason: a paste arrives with no name (a
// screenshot on the clipboard is bytes), while an upload arrives WITH one, and the name is part of
// what the reader sent ("发票.pdf"). Two layouts, because the two things are not the same thing —
// and both die with the conversation, since neither belongs in the user's workspace.
const uploadsDirName = "uploads"

// maxUploadNameRunes caps a stored name. The name survives into the @-reference that carries the
// file into the conversation, and a reference ends at the next whitespace anyway — so a very long
// one is unusable long before the filesystem would object.
const maxUploadNameRunes = 80

// ErrNoUploadName reports an upload whose name cannot be made into a single path element at all
// (empty, "." or ".."). Named because the caller has a sentence to say about it.
var ErrNoUploadName = errors.New("session: upload has no usable file name")

// UploadDir resolves the directory a session's uploaded files live in — <root>/<id>/uploads — and
// creates it. The id rule is DirFor's: it can arrive from a phone, and it names one directory.
func UploadDir(root, sessionID string) (string, error) {
	dir, err := DirFor(root, sessionID)
	if err != nil {
		return "", err
	}
	sub := filepath.Join(dir, uploadsDirName)
	if err := os.MkdirAll(sub, 0o700); err != nil {
		return "", err
	}
	return sub, nil
}

// SaveUploadedFile writes a file a frontend sent into the session's own directory, under
// uploads/, and returns the stored path — the path an @-reference points at.
//
// The NAME is kept as far as it can be: it is what the sender called the file, and the phone shows
// it back to them. What cannot be kept is everything that would make it name something other than
// one file in one directory — any directory part is dropped, and the result must still be a single
// path element (the same rule DirFor applies to the session id, for the same reason: this name
// comes from a client). A name already taken is suffixed rather than overwritten, so two photos
// called IMG_0001.jpg both survive; losing one silently is not a failure anyone could notice.
//
// root is the directory the store that HOLDS the session keeps it in (session.Manager.BaseDir) —
// see DirFor, which is also where the session-id rule is stated.
func SaveUploadedFile(root, sessionID, name string, raw []byte) (string, error) {
	if len(raw) == 0 {
		return "", errors.New("session: no bytes to store")
	}
	safe, err := UploadFileName(name)
	if err != nil {
		return "", err
	}
	dir, err := UploadDir(root, sessionID)
	if err != nil {
		return "", err
	}
	p, err := uniquePath(dir, safe)
	if err != nil {
		return "", err
	}
	// O_EXCL: uniquePath picked a free name, and a concurrent upload picking the same one must make
	// this write fail rather than have the two share a file.
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return p, nil
}

// UploadFileName turns a client's file name into one that names a single file in one directory.
//
// Directory parts are dropped rather than refused: a phone's picker hands over a path
// (`.../DCIM/Camera/IMG_0001.jpg`) and the base name is exactly what the sender means. Empty,
// "." and ".." are refused instead, because a sanitised version of those would be a name nobody
// sent — "file" standing in for a photo is more confusing than being told it had no name.
func UploadFileName(name string) (string, error) {
	// A Windows-style path can reach us too (a phone's picker on some platforms reports one), so
	// both separators are treated as separators before the base is taken.
	base := path.Base(strings.ReplaceAll(strings.TrimSpace(name), "\\", "/"))
	base = strings.Map(func(r rune) rune {
		switch r {
		case 0, '\r', '\n', '\t':
			return -1
		}
		return r
	}, base)
	base = strings.TrimSpace(base)
	switch base {
	case "", ".", "..", "/":
		return "", ErrNoUploadName
	}
	if utf8.RuneCountInString(base) > maxUploadNameRunes {
		base = truncateName(base, maxUploadNameRunes)
	}
	if base == "" || !fileutil.IsSinglePathElement(base) {
		return "", ErrNoUploadName
	}
	return base, nil
}

// truncateName shortens a name while keeping its extension, since the extension is what tells a
// later reader (and @-expansion) what kind of file it is.
func truncateName(name string, limit int) string {
	ext := filepath.Ext(name)
	if utf8.RuneCountInString(ext) >= limit {
		return string([]rune(name)[:limit])
	}
	stem := strings.TrimSuffix(name, ext)
	keep := limit - utf8.RuneCountInString(ext)
	return string([]rune(stem)[:keep]) + ext
}

// uniquePath answers a free path in dir for name: the name itself, or with "-1", "-2", … inserted
// before the extension until one does not exist.
func uniquePath(dir, name string) (string, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 0; i < 1000; i++ {
		candidate := name
		if i > 0 {
			candidate = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		p := filepath.Join(dir, candidate)
		if !fileutil.Exists(p) {
			return p, nil
		}
	}
	return "", errors.New("session: too many uploads with this name")
}
