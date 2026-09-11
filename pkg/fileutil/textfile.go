package fileutil

import (
	"bytes"
	"errors"
	"io"
	"os"
)

// textProbeSize is how many leading bytes LooksLikeText scans for NUL.
const textProbeSize = 8 * 1024

// LooksLikeText reports whether the file at path looks like text, i.e. it has
// no NUL byte within the first textProbeSize bytes. An empty file is text.
//
// The error is non-nil only when the content cannot be read at all (missing
// file, permission denied, a directory) — deliberately kept apart from "it is
// binary", because the two call for different words: a user who attached a file
// they can no longer read wants to be told that, not told it has no preview.
//
// This probes CONTENT rather than the extension on purpose: a .log holding a
// binary dump and a .dat holding a README both exist, and the callers that
// matter (attaching a file to a message, previewing one on screen) have to
// decide by what is actually in the bytes.
func LooksLikeText(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	buf := make([]byte, textProbeSize)
	n, err := f.Read(buf)
	// A short read at EOF is the normal case for a file smaller than the probe
	// window; anything else means the content is unreadable.
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return !bytes.Contains(buf[:n], []byte{0}), nil
}
