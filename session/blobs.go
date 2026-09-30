package session

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/monsterxx03/tachi/pkg/logger"
)

// imagesDirName is where a session's image blobs live: beside pasted/, subagent/, oneoff/ and
// rewound/, inside the session's own directory. A blob belongs to the conversation that sent the
// image and dies with it — DeleteSession removes the whole directory — and it never lands in the
// user's workspace, where it would show up in their diff.
const imagesDirName = "images"

// imageRefLen is the length of an image reference: a SHA-256 in hex. Checking it is not
// decoration — a reference is a file name taken from a record, and a corrupt or hostile record
// must not be able to name a file outside the blob directory.
const imageRefLen = sha256.Size * 2

// imagesDir returns the session's blob directory.
func (s *FileStore) imagesDir(id string) string {
	return filepath.Join(s.sessionDir(id), imagesDirName)
}

// imagePath resolves one blob reference to its path, refusing anything that is not a well-formed
// reference (see imageRefLen).
func (s *FileStore) imagePath(id, ref string) (string, error) {
	if !isImageRef(ref) {
		return "", fmt.Errorf("not an image reference: %q", ref)
	}
	return filepath.Join(s.imagesDir(id), ref), nil
}

func isImageRef(ref string) bool {
	if len(ref) != imageRefLen {
		return false
	}
	for i := 0; i < len(ref); i++ {
		c := ref[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// LoadImage returns the bytes of the blob a record's image part refers to.
//
// It is the read half of AppendMessage's bargain: whatever a conversion needs to rebuild an
// image part comes from here, and a failure is reported rather than guessed at. Callers that
// cannot resolve an image (the file is gone, the record was copied from another machine) are
// expected to degrade to the text the record still carries — see
// agent.ConvertSessionToLLMMessages.
func (s *FileStore) LoadImage(id, ref string) ([]byte, error) {
	path, err := s.imagePath(id, ref)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read image blob: %w", err)
	}
	return data, nil
}

// materializeImages moves every inline image part of msg into the session's blob directory and
// leaves a reference behind. This is the store's half of "a record on disk never carries image
// bytes", and the only place a blob is ever written — every writer of a session record goes
// through AppendMessage, so no path can store an image part without its bytes.
//
// It never fails the record. A part whose bytes cannot be stored (damaged base64, an
// unwritable directory) is dropped and reported: losing a turn's tool result — which is what
// failing the append would mean — is the worse outcome, and every consumer already has to cope
// with a record that has no parts at all (that is what every pre-existing record looks like).
func (s *FileStore) materializeImages(id string, msg *Message) {
	if len(msg.Parts) == 0 {
		return
	}

	kept := make([]Part, 0, len(msg.Parts))
	for _, part := range msg.Parts {
		if part.Type != PartTypeImage {
			kept = append(kept, part)
			continue
		}
		// Already a reference: the record came back off disk (or through a writer that
		// stored its blob earlier) and its bytes are the read path's business.
		if part.Data == "" && part.Ref != "" {
			kept = append(kept, part)
			continue
		}
		ref, err := s.storeImage(id, part.Data)
		if err != nil {
			logger.New("session").Warn(context.Background(), "session: image part not stored",
				"session", id, "media_type", part.MediaType, "err", err)
			continue
		}
		part.Data = ""
		part.Ref = ref
		kept = append(kept, part)
	}
	msg.Parts = kept
}

// storeImage writes base64 image data into the session's blob directory, content-addressed, and
// answers with the reference to put on the record. The same image sent twice — a screenshot
// pasted and then re-read, a tool returning a chart on every call — costs one blob.
func (s *FileStore) storeImage(id, data string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return "", fmt.Errorf("decode image data: %w", err)
	}
	if len(raw) == 0 {
		return "", fmt.Errorf("empty image data")
	}

	sum := sha256.Sum256(raw)
	ref := hex.EncodeToString(sum[:])
	path, err := s.imagePath(id, ref)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(path); err == nil {
		return ref, nil // already stored; content-addressed, so it IS this image
	}

	// A UNIQUE temporary name rather than fileutil.AtomicWriteFile's fixed "<path>.tmp": the
	// blob name is a content hash, so two writers of the same image (two frontends on one
	// session, the same image in two turns) would otherwise share one temp file and could
	// publish a half-written blob over a good one.
	if err := os.MkdirAll(s.imagesDir(id), 0o700); err != nil {
		return "", fmt.Errorf("create images dir: %w", err)
	}
	tmp, err := os.CreateTemp(s.imagesDir(id), "tmp-*")
	if err != nil {
		return "", fmt.Errorf("create temp blob: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("write blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("close blob: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("publish blob: %w", err)
	}
	return ref, nil
}

// DropUnstoredImageParts removes image parts whose bytes never reached the blob store, for the
// writers that own their JSONL file instead of going through this store: the subagent and
// one-off transcripts. Those are display-only records (nothing converts them back into a
// request), and they cannot hold bytes — they serialize Part with Data dropped — so a part that
// survived would claim an image no reader can produce. The text stays: it already says an image
// was there.
func DropUnstoredImageParts(msg *Message) {
	if len(msg.Parts) == 0 {
		return
	}
	kept := make([]Part, 0, len(msg.Parts))
	for _, part := range msg.Parts {
		if part.Type == PartTypeImage && part.Ref == "" {
			continue
		}
		kept = append(kept, part)
	}
	msg.Parts = kept
}
