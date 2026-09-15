package llm

import "strings"

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
