package agent

import (
	"context"
	"encoding/base64"

	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/session"
)

// RecordParts converts the live content parts of a message into the form a session record keeps.
//
// It is pure: the bytes ride along on Part.Data and the store (session.FileStore.AppendMessage)
// is what puts them in the session's images/ directory and leaves a reference behind. That
// division is deliberate — a record site in the agent loop has no idea where a session's files
// live, and the store is the one place that does.
//
// A message with no image part yields nil. Parts exist so an IMAGE survives a reload, and a
// record carrying parts without one would rebuild the request in a different SHAPE than it was
// sent in (a plain string becomes a parts array), which is the very prefix change this whole
// path exists to prevent. An image part with no bytes is left out for the same reason: every
// producer drops empty images before they get this far, and recording one would store a
// reference nobody can ever resolve.
func RecordParts(parts []llm.ContentPart) []session.Part {
	hasImage := false
	for i := range parts {
		if parts[i].Type == llm.ContentPartImage && parts[i].Data != "" {
			hasImage = true
			break
		}
	}
	if !hasImage {
		return nil
	}

	out := make([]session.Part, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case llm.ContentPartText:
			out = append(out, session.Part{Type: session.PartTypeText, Text: p.Text})
		case llm.ContentPartImage:
			if p.Data == "" {
				continue
			}
			out = append(out, session.Part{
				Type:      session.PartTypeImage,
				MediaType: p.MediaType,
				Data:      p.Data,
			})
		}
	}
	return out
}

// rebuildParts resolves a record's stored parts back into the parts to send.
//
// Verbatim, in order, with no rules of its own: the array is what went to the provider, and
// every piece of it is restored from the record rather than re-derived (a re-derived text part,
// for instance, would quietly disagree with the one that was sent the moment anything upstream
// changed shape). An image part that cannot be resolved is dropped and reported — the record's
// text still says an image was there, and a part we cannot produce is worse than none, because
// the request would claim bytes that are not in it.
func rebuildParts(parts []session.Part, images *partImages) []llm.ContentPart {
	if len(parts) == 0 {
		return nil
	}

	out := make([]llm.ContentPart, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case session.PartTypeText:
			out = append(out, llm.ContentPart{Type: llm.ContentPartText, Text: p.Text})
		case session.PartTypeImage:
			data, ok := images.base64For(p)
			if !ok {
				continue
			}
			out = append(out, llm.ContentPart{
				Type:      llm.ContentPartImage,
				MediaType: p.MediaType,
				Data:      data,
			})
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// partImages resolves the image blobs one conversion refers to, at most once each: the same
// screenshot is routinely sent twice in a conversation (pasted, then read back by the ReadFile
// tool), and both records point at the same blob.
//
// A missing blob is reported once per reference, not once per record — the failure is a
// property of the session directory, and repeating it for every copy of the image would bury
// it.
type partImages struct {
	resolver ImageResolver
	cache    map[string]string // ref → base64, as it will be sent
	warned   map[string]bool
}

func newPartImages(resolver ImageResolver) *partImages {
	return &partImages{resolver: resolver, cache: map[string]string{}, warned: map[string]bool{}}
}

// base64For returns the base64 payload of one image part, false when it cannot be produced.
func (pi *partImages) base64For(p session.Part) (string, bool) {
	// The record's own bytes win when it has them: a record that has not been through the
	// store yet (an in-memory session manager, a record being converted inside the run that
	// produced it) carries its image inline, and asking a resolver for a blob that does not
	// exist would drop an image we are holding.
	if p.Data != "" {
		return p.Data, true
	}
	if p.Ref == "" {
		pi.warn("agent: image part carries no stored bytes, dropping it", p)
		return "", false
	}
	if cached, ok := pi.cache[p.Ref]; ok {
		return cached, true
	}
	if pi.resolver == nil {
		pi.warn("agent: no image resolver for this conversion, dropping image part", p)
		return "", false
	}
	raw, err := pi.resolver.LoadImage(p.Ref)
	if err != nil {
		pi.warn("agent: image blob unreadable, dropping image part", p, "err", err)
		return "", false
	}
	encoded := base64.StdEncoding.EncodeToString(raw)
	pi.cache[p.Ref] = encoded
	return encoded, true
}

// warn logs a dropped image part once per reference: a conversion that silently loses an image
// would otherwise be indistinguishable from a conversation that never had one.
func (pi *partImages) warn(msg string, p session.Part, attrs ...any) {
	if p.Ref != "" {
		if pi.warned[p.Ref] {
			return
		}
		pi.warned[p.Ref] = true
	}
	all := append([]any{"media_type", p.MediaType}, attrs...)
	logger.New("agent").Warn(context.Background(), msg, all...)
}
