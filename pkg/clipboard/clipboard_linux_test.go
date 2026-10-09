//go:build linux

package clipboard

import (
	"context"
	"testing"
)

// TestLinuxRoutes pins the actual command lines: the platform half is the only place the tool names
// and the MIME types of the requests appear, and a typo there is invisible until a real screenshot
// fails to paste — which is exactly the bug this package was written to fix.
func TestLinuxRoutes(t *testing.T) {
	r := reader{run: (&fakeRunner{outputs: map[string][]byte{
		"xclip -selection clipboard -t image/png -o": pngBytes,
		"xclip -selection clipboard -o":              []byte("copied text\n"),
	}}).run}

	if _, mediaType, err := r.image(context.Background()); err != nil || mediaType != "image/png" {
		t.Errorf("image() = %q, %v; want the png through xclip", mediaType, err)
	}
	if text, err := r.text(context.Background()); err != nil || text != "copied text" {
		t.Errorf("text() = %q, %v; want the xclip text", text, err)
	}

	// The Wayland route comes first, and it is the one that answers when both tools are installed:
	// the two return different formats here, so which one was asked is visible in the answer.
	r = reader{run: (&fakeRunner{outputs: map[string][]byte{
		"wl-paste --type image/png --no-newline":     pngBytes,
		"xclip -selection clipboard -t image/png -o": {0xFF, 0xD8, 0xFF, 0x00},
	}}).run}
	if _, mediaType, err := r.image(context.Background()); err != nil || mediaType != "image/png" {
		t.Errorf("image() = %q, %v; want wl-paste's png", mediaType, err)
	}
}
