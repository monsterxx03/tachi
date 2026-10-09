package clipboard

import "context"

// Linux reads the clipboard with the tool the display server actually has: wl-paste for Wayland,
// xclip for X11. Both are optional packages, so the routes are tried in order and each one that is
// missing or fails is skipped — the machine has one of them or the read reports ErrUnsupported.
//
// Two candidates per tool, because there are two ways an image gets onto a clipboard: a screenshot
// tool offers image/png, while a copied picture may only be offered as image/jpeg. Asking for a
// type the selection does not have fails, so trying both costs one extra process on a keystroke and
// is the difference between "no image" and the image.
var (
	imageSources = []clipboardCmd{
		{name: "wl-paste", args: []string{"--type", "image/png", "--no-newline"}},
		{name: "wl-paste", args: []string{"--type", "image/jpeg", "--no-newline"}},
		{name: "xclip", args: []string{"-selection", "clipboard", "-t", "image/png", "-o"}},
		{name: "xclip", args: []string{"-selection", "clipboard", "-t", "image/jpeg", "-o"}},
	}
	textSources = []clipboardCmd{
		// --no-newline: wl-paste appends one of its own, and it is not part of what was copied.
		{name: "wl-paste", args: []string{"--no-newline"}},
		{name: "xclip", args: []string{"-selection", "clipboard", "-o"}},
	}
)

func (r reader) image(ctx context.Context) ([]byte, string, error) {
	return r.imageFrom(ctx, imageSources)
}

func (r reader) text(ctx context.Context) (string, error) {
	return r.textFrom(ctx, textSources)
}
