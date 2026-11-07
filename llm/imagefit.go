package llm

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/jpeg"
	"image/png"

	// Decoders for the formats MediaType knows about. A format we cannot decode is
	// passed through untouched rather than rejected: refusing an image the provider
	// itself may accept would be a regression bought for nothing.
	_ "golang.org/x/image/webp"
	_ "image/gif"

	"golang.org/x/image/draw"
)

// Why this file exists: a browser screenshot is a 3360×1894 PNG of 4.7MB, and base64
// makes that 6.3MB on the wire. Three of them in one history is a 16MB request body,
// and the provider answers 413 — the conversation cannot continue, even though almost
// none of those bytes are worth sending: a vision model reads a picture at roughly
// 1.5k pixels on the long edge, and past that the extra bytes are detail it never
// sees. Downscaling keeps the picture and drops the waste.
const (
	// InlineImageEdge is the longest side an image is sent at.
	InlineImageEdge = 1568

	// InlineImageBytes is the byte budget for ONE image on the wire. A downscaled
	// screenshot lands well under it; a photograph that already fits is left alone.
	InlineImageBytes = 1 << 20

	// InlineImageMinEdge stops the quality loop: below this the picture has to lose
	// detail the model does need before it loses bytes the line does not want.
	InlineImageMinEdge = 320

	// inlineJPEGQuality is for the fallback encode. 82 is where a UI screenshot's text
	// is still crisp and the file is a fifth of the PNG.
	inlineJPEGQuality = 82
)

// FittedImage is an image as it should be SENT: the input unchanged when it already
// fits, a downscaled copy when it does not.
type FittedImage struct {
	Data      []byte
	MediaType string
	Width     int
	Height    int
	// Changed reports whether Data is a re-encode. Callers that only want the bytes
	// can ignore it; callers that describe the image to the model must not.
	Changed bool
}

// FitImage brings raw within the inline budget: at most InlineImageEdge on the long
// side, InlineImageBytes on the wire, re-encoded as PNG or JPEG — whichever is
// smaller, except that an image with transparency stays PNG (JPEG would turn its
// transparent areas into black blocks).
//
// It is deterministic: the same bytes always produce the same output, which is what
// lets a rebuilt request match the one that was sent and keep the provider's prompt
// cache (docs/agents/prompt-cache.md).
//
// The input is returned as-is when it already fits, and when it cannot be decoded —
// rewriting bytes we do not understand could only make them worse.
func FitImage(raw []byte, mediaType string) FittedImage {
	unchanged := FittedImage{Data: raw, MediaType: mediaType}
	if len(raw) == 0 {
		return unchanged
	}

	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return unchanged
	}
	bounds := src.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w <= 0 || h <= 0 {
		return unchanged
	}
	unchanged.Width, unchanged.Height = w, h

	tw, th := fitWithin(w, h, InlineImageEdge)
	if tw == w && th == h && len(raw) <= InlineImageBytes {
		return unchanged
	}

	for attempt := 0; ; attempt++ {
		resized := resizeTo(src, tw, th)
		data, outType := encodeSmallest(resized)
		if len(data) <= InlineImageBytes || tw <= InlineImageMinEdge || attempt >= 3 {
			return FittedImage{Data: data, MediaType: outType, Width: tw, Height: th, Changed: true}
		}
		// Still over budget at this size: give up pixels rather than the picture.
		tw, th = maxInt(tw*3/4, InlineImageMinEdge), maxInt(th*3/4, 1)
	}
}

// fitWithin scales (w,h) down to the longest side edge, keeping the aspect ratio.
// An image already inside the box is returned unchanged.
func fitWithin(w, h, edge int) (int, int) {
	longest := maxInt(w, h)
	if longest <= edge {
		return w, h
	}
	if w >= h {
		return edge, maxInt(h*edge/w, 1)
	}
	return maxInt(w*edge/h, 1), edge
}

// resizeTo redraws src at (w,h). CatmullRom rather than nearest: a screenshot of text
// downscaled by nearest loses strokes and gains jaggies, and the whole point is that
// the model can still read what it is looking at.
func resizeTo(src image.Image, w, h int) image.Image {
	if src.Bounds().Dx() == w && src.Bounds().Dy() == h {
		return src
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
	return dst
}

// encodeSmallest encodes the image both ways it can be sent and keeps the smaller.
// Transparency pins it to PNG; everything else is free to become a JPEG.
func encodeSmallest(img image.Image) ([]byte, string) {
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		return encodeJPEG(img), "image/jpeg"
	}
	if hasAlpha(img) {
		return pngBuf.Bytes(), "image/png"
	}
	jpegData := encodeJPEG(img)
	if len(jpegData) < pngBuf.Len() {
		return jpegData, "image/jpeg"
	}
	return pngBuf.Bytes(), "image/png"
}

func encodeJPEG(img image.Image) []byte {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: inlineJPEGQuality}); err != nil {
		return nil
	}
	return buf.Bytes()
}

// hasAlpha scans for a pixel that is not fully opaque. Screenshots are opaque and pay
// a full scan of their pixels for the answer; that is cheaper than the alternative,
// which is a transparent logo sent as a black rectangle.
func hasAlpha(img image.Image) bool {
	switch m := img.(type) {
	case *image.RGBA:
		for i := 3; i < len(m.Pix); i += 4 {
			if m.Pix[i] != 0xff {
				return true
			}
		}
		return false
	case *image.NRGBA:
		for i := 3; i < len(m.Pix); i += 4 {
			if m.Pix[i] != 0xff {
				return true
			}
		}
		return false
	}
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a != 0xffff {
				return true
			}
		}
	}
	return false
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// FittedImageParts returns parts with every oversized image replaced by its fitted
// copy (base64 in, base64 out, media type kept truthful). Parts that already fit are
// passed through untouched — the returned slice is new, so a caller's own slice is
// never rewritten behind its back.
//
// This is the single door for image bytes entering a request, whatever produced
// them: a tool result, a pasted screenshot, an @-file.
func FittedImageParts(parts []ContentPart) []ContentPart {
	out := parts
	copied := false
	for i := range parts {
		if parts[i].Type != ContentPartImage || parts[i].Data == "" {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(parts[i].Data)
		if err != nil {
			continue
		}
		fit := FitImage(raw, parts[i].MediaType)
		if !fit.Changed {
			continue
		}
		if !copied { // first change: stop sharing the caller's backing array
			out = append([]ContentPart(nil), parts...)
			copied = true
		}
		out[i].Data = base64.StdEncoding.EncodeToString(fit.Data)
		out[i].MediaType = fit.MediaType
	}
	return out
}
