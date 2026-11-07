package llm

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// noisyPNG builds a large PNG whose bytes cannot be compressed away — flat colour
// would shrink to nothing and hide whether the fit did its job.
func noisyPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	seed := uint32(12345)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			seed = seed*1664525 + 1013904223
			img.SetRGBA(x, y, color.RGBA{R: uint8(seed >> 24), G: uint8(seed >> 16), B: uint8(seed >> 8), A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

func TestFitImageBringsAnOversizedScreenshotUnderBudget(t *testing.T) {
	raw := noisyPNG(t, 3200, 1800)
	if len(raw) <= InlineImageBytes {
		t.Fatalf("test image is %d bytes, not over the %d budget", len(raw), InlineImageBytes)
	}

	fit := FitImage(raw, "image/png")
	if !fit.Changed {
		t.Fatal("an oversized screenshot must be re-encoded")
	}
	if fit.Width > InlineImageEdge || fit.Height > InlineImageEdge {
		t.Errorf("fitted %dx%d, want both sides <= %d", fit.Width, fit.Height, InlineImageEdge)
	}
	if len(fit.Data) > InlineImageBytes {
		t.Errorf("fitted image is %d bytes, still over the %d budget", len(fit.Data), InlineImageBytes)
	}
	if fit.MediaType != "image/png" && fit.MediaType != "image/jpeg" {
		t.Errorf("media type = %q", fit.MediaType)
	}
	if _, _, err := image.Decode(bytes.NewReader(fit.Data)); err != nil {
		t.Errorf("fitted bytes do not decode: %v", err)
	}
}

func TestFitImageIsDeterministic(t *testing.T) {
	raw := noisyPNG(t, 2400, 1200)
	first := FitImage(raw, "image/png")
	second := FitImage(raw, "image/png")
	if !bytes.Equal(first.Data, second.Data) || first.MediaType != second.MediaType {
		t.Fatal("the same bytes must fit to the same output: a rebuilt request has to match the one that was sent")
	}
}

func TestFitImageLeavesAnImageThatAlreadyFitsAlone(t *testing.T) {
	raw := noisyPNG(t, 400, 300)
	fit := FitImage(raw, "image/png")
	if fit.Changed {
		t.Fatalf("a %d-byte %dx%d image should be sent as it is", len(raw), fit.Width, fit.Height)
	}
	if !bytes.Equal(fit.Data, raw) {
		t.Error("the unchanged path must return the original bytes")
	}
}

func TestFitImageKeepsTransparency(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2000, 2000))
	for y := 0; y < 2000; y++ {
		for x := 0; x < 2000; x++ {
			if x < 1000 {
				img.SetRGBA(x, y, color.RGBA{200, 30, 30, 0}) // fully transparent half
				continue
			}
			img.SetRGBA(x, y, color.RGBA{200, 30, 30, 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode: %v", err)
	}

	fit := FitImage(buf.Bytes(), "image/png")
	if !fit.Changed {
		t.Fatal("a 2000px-wide image must be downscaled")
	}
	if fit.MediaType != "image/png" {
		t.Errorf("media type = %q, want image/png: JPEG would turn the transparent half black", fit.MediaType)
	}
}

func TestFitImagePassesThroughBytesItCannotDecode(t *testing.T) {
	raw := bytes.Repeat([]byte{0x42}, int(InlineImageBytes)+10)
	fit := FitImage(raw, "image/png")
	if fit.Changed || !bytes.Equal(fit.Data, raw) {
		t.Error("an undecodable image must be sent untouched")
	}
}
