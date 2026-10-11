package node

import (
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

// File Parts: bytes crossing the boundary between two machines. There are THREE directions and
// they share one set of rules, because a limit that differs per direction is a limit nobody can
// predict:
//
//   - a peer attaches a file to its message (this node receives it);
//   - a peer's task produced a file and this machine's delegation reads it back;
//   - this node's remote agent hands a file over with SendFile (the caller receives it).
//
// The protocol leaves size, type and storage to the implementation (§6.4), so these are Tachi's
// policy rather than the spec's. They are constants for now — the single place to add config keys
// is the block below.

const (
	// maxFilePartBytes is the largest single file that may cross. `raw` is base64 on the wire (a
	// third larger on top), and what lands is a file this machine's tools can read, so the ceiling
	// is about the transfer rather than about what can then be done with it.
	//
	// It is deliberately close to what a request body can carry (maxRequestBodyBytes): ONE maximum
	// file needs ~134 MiB of body after base64, so the body bound is what a callers hits first if it
	// tries to send two of them in one message. That is a fact worth knowing rather than a bug — a
	// node that accepted two would have to hold both in memory — and the count bound below is what
	// governs a message of ordinary-sized files.
	maxFilePartBytes = 100 << 20

	// maxFilePartsPerMessage bounds how many files ONE message or artifact may carry: without it a
	// peer decides how much work a single accepted task costs.
	maxFilePartsPerMessage = 8

	// maxPartNameBytes bounds the file name taken from the peer, which becomes a name inside a
	// session directory.
	maxPartNameBytes = 120

	// maxRequestBodyBytes caps EVERY request body the remote listener will read. It is a bound on
	// memory, not a statement about files: it sits far above maxFilePartBytes on purpose, because a
	// body may legitimately carry several files plus a long prompt, and a request that is too large
	// anyway must be refused BEFORE the process has allocated it (the guard reads the body to look
	// at it, so without this the size of a request decides how much memory this node spends).
	maxRequestBodyBytes = 256 << 20

	// Reasons a Part-carrying message is refused with. They travel under Tachi's own error domain
	// (the admission guard renders them, §9.1.1), so a caller learns "this file was not taken, and
	// why" instead of the silent drop these three directions used to be.
	reasonPartTooLarge = "PART_TOO_LARGE"
	// reasonPartMediaType is what a ZERO-BYTE Part is refused with (checkPartSize) — a broken
	// transfer rather than a type this node dislikes. The word outlives the type policy this node
	// used to have, and it is on the wire, so it stays as it is.
	reasonPartMediaType = "PART_MEDIA_TYPE"
	reasonPartCount     = "PART_COUNT"
	reasonPartURL       = "PART_URL_UNSUPPORTED"
	// reasonBodyTooLarge is about the WHOLE request rather than one file: the body was bigger than
	// this node reads, so nothing in it was looked at.
	reasonBodyTooLarge = "BODY_TOO_LARGE"
)

// partExtensions names the extension a NAMELESS file of a type is stored with, written out rather
// than asked of the stdlib: `mime.ExtensionsByType` answers in alphabetical order, which would
// store a plain text file as `.conf`. Images are not here because llm already states that
// preference for the same reason (imageExtByMediaType, which is also what a paste is stored as).
var partExtensions = map[string]string{
	"text/plain":       ".txt",
	"text/markdown":    ".md",
	"text/csv":         ".csv",
	"text/html":        ".html",
	"application/json": ".json",
	"application/pdf":  ".pdf",
	"application/zip":  ".zip",
}

// filePart is one file that crossed (or is about to cross) the boundary.
type filePart struct {
	Name      string
	MediaType string
	Bytes     []byte
}

// Size is the number every sentence about this file quotes.
func (f filePart) Size() int64 { return int64(len(f.Bytes)) }

// partRefusal says why one Part was not taken: Reason is the stable word for the wire, Detail the
// sentence a log, a tool result or a caller reads.
type partRefusal struct {
	Reason string
	Detail string
}

func (r *partRefusal) Error() string { return r.Detail }

// filePartsOf splits a Part list into the files it carries and the refusals, which is the ONE
// reading of a message's or an artifact's Parts both ends of every direction use.
//
// Text and `data` Parts are not its business (they are how messages and structured questions are
// carried), and neither is a Part type this version cannot act on: a `url` Part IS a refusal,
// because fetching a URL a peer chose is a network reach this version does not make yet (§6.4) —
// silently ignoring it is what a caller cannot distinguish from an empty answer.
func filePartsOf(parts []*a2a.Part) (files []filePart, refused []*partRefusal) {
	for _, p := range parts {
		if p == nil {
			continue
		}
		switch p.Content.(type) {
		case a2a.Raw:
			if len(files) >= maxFilePartsPerMessage {
				refused = append(refused, &partRefusal{
					Reason: reasonPartCount,
					Detail: fmt.Sprintf("only %d files are taken from one message; the rest were refused",
						maxFilePartsPerMessage),
				})
				return files, refused
			}
			file, refusal := asFilePart(p)
			if refusal != nil {
				refused = append(refused, refusal)
				continue
			}
			files = append(files, file)
		case a2a.URL:
			refused = append(refused, &partRefusal{
				Reason: reasonPartURL,
				Detail: "a file URL was not fetched: this node does not download URLs a peer names yet",
			})
		}
	}
	return files, refused
}

// asFilePart turns one `raw` Part into a filePart, or says why it cannot be taken.
//
// The TYPE is never a reason to refuse: what a peer sends is a file this machine can open with its
// own tools, and a list of blessed types would have this node deciding, on the sender's behalf, what
// is worth carrying. The size and the count are about what the transfer costs, which is this node's
// business (§6.4 leaves the rest to the implementation).
func asFilePart(p *a2a.Part) (filePart, *partRefusal) {
	raw, _ := p.Content.(a2a.Raw)
	bytes := []byte(raw)
	if refusal := checkPartSize(int64(len(bytes))); refusal != nil {
		return filePart{}, refusal
	}
	mediaType := partMediaType(p, bytes)
	if strings.HasPrefix(mediaType, "image/") {
		// An image is the one type this machine goes on to hand a model AS an image, so the bytes are
		// what decides which image it is — a sender that mislabels a PNG as a JPEG gets a PNG. Bytes
		// that are not an image at all are not one: the file is then taken as whatever its bytes read
		// as (text/plain, application/octet-stream) and stored for the model to open, rather than
		// attached as an image part the provider would reject the whole request over. Whether what
		// comes out of this is an image a PROVIDER takes is a separate question, asked where the
		// bytes are handed over (runner.receiveFiles).
		mediaType = partMediaTypeFromBytes(bytes)
	}
	return filePart{Name: partName(p.Filename, mediaType), MediaType: mediaType, Bytes: bytes}, nil
}

// checkPartSize is the size policy on its own.
//
// It is a function rather than two lines inside asFilePart so that the policy can be TESTED without
// materialising the bytes it refuses: a test that allocates a maximum-size file to prove the limit
// works measures the test machine, and at 100 MiB it would measure it slowly.
func checkPartSize(size int64) *partRefusal {
	if size == 0 {
		return &partRefusal{
			Reason: reasonPartMediaType,
			Detail: "an empty file was refused: zero bytes is a broken transfer, not a file",
		}
	}
	if size > maxFilePartBytes {
		return &partRefusal{
			Reason: reasonPartTooLarge,
			Detail: fmt.Sprintf("a file of %s was refused: the limit is %s",
				strutil.HumanBytes(size), strutil.HumanBytes(maxFilePartBytes)),
		}
	}
	return nil
}

// partMediaType answers what this file IS, in the order that trusts the most specific statement
// first: what the sender said, then what the name says, then what the bytes say.
//
// What comes out is a LABEL — what the model is told, and what a nameless file is named by — and no
// type is refused for it (asFilePart). The one thing that acts on it is whether the bytes are handed
// to a model AS an image, and that answer is taken from the bytes further down.
//
// The last two steps are not politeness: a peer may send raw bytes with no mediaType at all. Note
// that `mime.TypeByExtension` answers from the stdlib's own table AND from the host's mime files on
// unix (/etc/mime.types and friends), so `notes.md` labels as text/markdown on Linux and text/plain
// here, and `deploy.sh` as application/x-sh there. That is a description varying by machine, which
// is harmless because nothing is decided from it — but it is exactly why no verdict may ever be.
//
// The signatures are checked before the generic sniff so an image comes out as the image it really
// is. Parameters are dropped ("text/plain; charset=utf-8" is `text/plain`) because the label speaks
// about the type.
func partMediaType(p *a2a.Part, bytes []byte) string {
	if mt := mediaTypeOnly(p.MediaType); mt != "" {
		return mt
	}
	if ext := filepath.Ext(p.Filename); ext != "" {
		if mt, ok := llm.ImageMediaType(ext); ok {
			return mt
		}
		if mt := mediaTypeOnly(mime.TypeByExtension(ext)); mt != "" {
			return mt
		}
	}
	return partMediaTypeFromBytes(bytes)
}

// partMediaTypeFromBytes is the last of those steps on its own: what the BYTES say, signatures first
// (llm's table — the formats a provider actually takes) and the generic sniff after.
//
// asFilePart calls it directly for a file whose type came out as an image, because that is the one
// answer something is decided from and it must therefore be about the bytes rather than about a
// name or a sender's word.
func partMediaTypeFromBytes(bytes []byte) string {
	if mt := llm.ImageMediaTypeForBytes(bytes); mt != "" {
		return mt
	}
	return mediaTypeOnly(http.DetectContentType(bytes))
}

// mediaTypeOnly strips the parameters a media type may carry.
func mediaTypeOnly(mediaType string) string {
	if mediaType == "" {
		return ""
	}
	if parsed, _, err := mime.ParseMediaType(mediaType); err == nil {
		return parsed
	}
	return strings.ToLower(strings.TrimSpace(mediaType))
}

// partName turns the peer's name for a file into a NAME inside a session directory.
//
// The value is untrusted input, so it is not repaired into a path and then used: it is reduced to
// its base, because everything before that is the peer choosing where on THIS machine the bytes
// would land. A name that is empty after that (or absent) gets a generated one — the extension
// matters, since it is what keeps the file openable, and the session layer refuses a separator
// again on its own way in (session.SaveReceivedFile), so neither layer is the one that lets one
// through.
func partName(name, mediaType string) string {
	base := strings.TrimSpace(name)
	base = strings.ReplaceAll(base, `\`, "/") // a Windows-style separator is still a separator
	base = filepath.Base(base)
	if base == "." || base == ".." || base == "/" || base == string(filepath.Separator) {
		base = ""
	}
	if base == "" {
		base = "file" + partExtension(mediaType)
	}
	ext := filepath.Ext(base)
	if len(base) <= maxPartNameBytes {
		return base
	}
	// Too long: keep the head and the extension, which is the part that says how to open it.
	keep := maxPartNameBytes - len(ext)
	if keep < 1 {
		return strutil.TruncateBytes(base, maxPartNameBytes)
	}
	return strutil.TruncatePlain(base[:len(base)-len(ext)], keep) + ext
}

// partExtension is the extension a generated name gets, so a file whose sender named nothing is
// still openable here. The stated tables first (images through llm's, which is the one a paste uses
// too), then whatever the stdlib knows about a `text/*` subtype this node accepts by rule rather
// than by name.
func partExtension(mediaType string) string {
	if ext, ok := llm.ImageExtForMediaType(mediaType); ok {
		return ext
	}
	if ext, ok := partExtensions[mediaType]; ok {
		return ext
	}
	if exts, err := mime.ExtensionsByType(mediaType); err == nil && len(exts) > 0 {
		return exts[0]
	}
	return ""
}
