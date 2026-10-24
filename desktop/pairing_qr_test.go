package main

import (
	"strings"
	"testing"
)

// TestPairingQRCodeRenders pins the one thing that makes "scan instead of type" work: the payload
// becomes an actual block-character code, not an empty string. The content itself is verified by the
// phone (qr.test.ts) and by construction — this is the "did the library/API survive" half.
func TestPairingQRCodeRenders(t *testing.T) {
	payload := `{"v":1,"url":"http://192.168.1.23:8787","token":"ERbup6_DqxSrmNLVD52JQCCPREeySXnWXuuJr0h3FTQ"}`
	code, err := pairingQRCode(payload)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.TrimSpace(code) == "" {
		t.Fatal("empty QR code")
	}
	lines := strings.Split(strings.TrimRight(code, "\n"), "\n")
	// A version-4-ish code is ~33 modules; half-block rows plus the quiet zone make anything under
	// ten rows a sign the renderer stopped working the way we use it.
	if len(lines) < 10 {
		t.Fatalf("QR code has only %d lines", len(lines))
	}
	width := len([]rune(lines[0]))
	for i, line := range lines {
		if got := len([]rune(line)); got != width {
			t.Fatalf("line %d is %d wide, want %d (ragged code)", i, got, width)
		}
	}
	if !strings.Contains(code, "█") {
		t.Fatal("no block characters in the rendered code")
	}
}
