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
