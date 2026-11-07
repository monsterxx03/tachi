package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/llm"
)

// bigScreenshotPNG is the shape of the case this exists for: a browser screenshot whose
// noise defeats compression, so it really is megabytes. Kept under llm.MaxImageSize —
// the Read tool refuses anything above that, and this test is about the fit, not the cap.
func bigScreenshotPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1600, 900))
	seed := uint32(7)
	for y := 0; y < 900; y++ {
		for x := 0; x < 1600; x++ {
			seed = seed*1664525 + 1013904223
			img.SetRGBA(x, y, color.RGBA{uint8(seed >> 24), uint8(seed >> 16), uint8(seed >> 8), 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if buf.Len() <= llm.InlineImageBytes {
		t.Fatalf("fixture is only %d bytes; it needs to be over the inline budget to test anything", buf.Len())
	}
	return buf.Bytes()
}

// A screenshot that would make the next request too large is downscaled on the way
// in, so every later request carries the small copy instead.
func TestAddImagePartsDownscalesAnOversizedScreenshot(t *testing.T) {
	raw := bigScreenshotPNG(t)
	ctx := WithImagePartsCarrier(context.Background())

	parts := AddImageParts(ctx, []llm.ContentPart{{
		Type:      llm.ContentPartImage,
		MediaType: "image/png",
		Data:      base64.StdEncoding.EncodeToString(raw),
	}})

	if len(parts) != 1 {
		t.Fatalf("got %d parts, want 1", len(parts))
	}
	sent, err := base64.StdEncoding.DecodeString(parts[0].Data)
	if err != nil {
		t.Fatalf("sent part is not base64: %v", err)
	}
	if len(sent) > llm.InlineImageBytes {
		t.Errorf("sent image is %d bytes, over the %d budget", len(sent), llm.InlineImageBytes)
	}
	if len(sent) >= len(raw) {
		t.Error("the sent image is not smaller than the file")
	}
	if got := ImagePartsFromCtx(ctx); len(got) != 1 || got[0].Data != parts[0].Data {
		t.Error("the carrier must hold the fitted parts, not the originals")
	}
}

// The Read tool's note has to describe what was SENT: reporting the file's bytes after
// sending a tenth of them is what makes a later 413 impossible to explain.
func TestReadToolImageNoteReportsTheFittedSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shot.png")
	raw := bigScreenshotPNG(t)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	tool := NewReadTool()
	ctx := WithImagePartsCarrier(context.Background())
	result, err := tool.ExecuteContext(ctx, fmt.Sprintf(`{"path": %q}`, path))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(result, "已缩放到") {
		t.Errorf("note = %q, want it to say the image was downscaled", result)
	}
	if strings.Contains(result, fmt.Sprintf("%d base64 chars", len(raw)/3*4)) {
		t.Errorf("note = %q, still quoting the file's own size", result)
	}
}
