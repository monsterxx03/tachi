package llm

import (
	"bytes"
	"strings"
)

// MaxImageSize is the largest image handed to a model as a content part. It is
// the single budget for every path that attaches image bytes — the Read tool and
// @-file expansion — and is deliberately separate from the (much smaller) text
// limits: image bytes never enter the text context.
const MaxImageSize = 5 * 1024 * 1024

// imageMediaTypesByExt maps lowercase file extensions (including the leading
// dot) to MIME types for image formats supported by common LLM providers.
var imageMediaTypesByExt = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

// ImageMediaType returns the MIME type for an image file extension (with the
// leading dot, any case). ok is false when ext is not a supported image format.
//
// This is the single source of truth for the extension → image MIME mapping,
// shared by the Read tool (magic-byte validation on top) and @-file expansion
// (agent/atfile).
func ImageMediaType(ext string) (mimeType string, ok bool) {
	mimeType, ok = imageMediaTypesByExt[strings.ToLower(ext)]
	return mimeType, ok
}

// imageExtByMediaType is the inverse, written out rather than derived so the preferred extension
// for a type is STATED instead of won by a map-order tie-break: image/jpeg has two (".jpg" and
// ".jpeg"), and a paste should be stored as the shorter one.
var imageExtByMediaType = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// ImageMediaTypeForBytes identifies an image from its leading bytes, returning the MIME type of a
// supported format and "" when they are not one.
//
// It is the byte-level half of the extension → type mapping, for the callers that hold BYTES and no
// path: a pasted screenshot never had a name. There is no extension to lean on there, so the bytes
// are the only evidence — and they are what tells an image clipboard apart from a text one, where
// the two arrive through the same channel and both look like "some bytes".
func ImageMediaTypeForBytes(raw []byte) string {
	for _, sig := range imageSignatures {
		if !bytes.HasPrefix(raw, sig.prefix) {
			continue
		}
		// RIFF is a container, not the format: only the WEBP tag four bytes later says which.
		// The length check is what keeps a 4-byte "RIFF" from slicing past the end.
		if sig.mediaType == webpMediaType &&
			(len(raw) < webpTagOffset+4 || !bytes.Equal(raw[webpTagOffset:webpTagOffset+4], []byte("WEBP"))) {
			continue
		}
		return sig.mediaType
	}
	return ""
}

const (
	// webpTagOffset is where the RIFF container's format tag sits: "RIFF" (4) + the size (4).
	webpTagOffset = 8
	// webpMediaType names this one because two places in the scan below have to agree on it.
	webpMediaType = "image/webp"
)

// imageSignatures are the leading bytes of each supported image, in the order they are tried:
// distinct prefixes, so the order only matters for a byte sequence that could be two of them.
var imageSignatures = []struct {
	mediaType string
	prefix    []byte
}{
	{"image/png", []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}},
	{"image/jpeg", []byte{0xFF, 0xD8, 0xFF}},
	{"image/gif", []byte("GIF8")},
	{webpMediaType, []byte("RIFF")},
}

// ImageExtForMediaType returns the extension an image of this MIME type is stored under, for the
// callers that hold BYTES and no path — a pasted screenshot never had a name. ok is false when the
// type is not one of the supported images, which is what lets a paste be refused instead of written
// under a name @-file expansion would treat as a binary file.
//
// Parameters are dropped and the type is lower-cased first: clipboards hand over things like
// "image/PNG" and "image/jpeg;charset=utf-8", and neither changes what the bytes are.
func ImageExtForMediaType(mimeType string) (ext string, ok bool) {
	if i := strings.IndexByte(mimeType, ';'); i >= 0 {
		mimeType = mimeType[:i]
	}
	ext, ok = imageExtByMediaType[strings.ToLower(strings.TrimSpace(mimeType))]
	return ext, ok
}
