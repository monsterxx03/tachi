package clipboard

import (
	"context"
	"os"
)

// macOS reads the pasteboard through osascript, because no command-line tool there can hand over
// an image: pbpaste reads TEXT only (its -Prefer accepts txt/rtf/ps, so asking for a picture yields
// nothing at all), a third-party helper like pngpaste may simply not be installed, and no CLI route
// exists otherwise. osascript is always present on macOS, and its JavaScript bridge reaches the
// same AppKit pasteboard a GUI app would.
//
// osascriptCommand is the interpreter and the language flag, spelled out because the script is
// passed inline: "JavaScript" is osascript's bridge to AppKit (JXA).
var osascriptCommand = []string{"osascript", "-l", "JavaScript"}

// darwinImageScript writes the clipboard's image to the path in argv[0] and prints what happened.
//
// PNG first, then JPEG, then TIFF — the three ways an image reaches this pasteboard: a screenshot
// (⌃⌘⇧3/4/5) puts PNG there, "copy image" in a browser tends to leave TIFF only. TIFF is the one
// type llm does not support, so it is CONVERTED here (AppKit is right there) instead of being
// written out and refused by the caller. A 1×1 test image is the one thing AppKit's TIFF
// conversion chokes on — not a shape a clipboard image ever has.
//
// argv[0] is interpolated by osascript, not by this script, and the path is one we made ourselves.
const darwinImageScript = `function run(argv) {
  ObjC.import('AppKit');
  ObjC.import('Foundation');
  var pb = $.NSPasteboard.generalPasteboard;
  var data = pb.dataForType('public.png');
  if (data.isNil() || data.length === 0) data = pb.dataForType('public.jpeg');
  if (data.isNil() || data.length === 0) {
    var tiff = pb.dataForType('public.tiff');
    if (!tiff.isNil() && tiff.length > 0) {
      data = $.NSBitmapImageRep.imageRepWithData(tiff)
        .representationUsingTypeProperties($.NSBitmapImageFileTypePNG, $());
    }
  }
  if (data.isNil() || data.length === 0) return '` + scriptWritesImage + `';
  if (!data.writeToFileAtomically(argv[0], true)) return 'write-failed';
  return 'ok';
}`

// image reads the pasteboard's image, if it has one.
func (r reader) image(ctx context.Context) ([]byte, string, error) {
	// The script writes to a file, so the name has to be reserved first: two pastes in the same
	// second must not share a path, and the file is ours alone either way.
	f, err := os.CreateTemp("", "tachi-clip-*.png")
	if err != nil {
		return nil, "", err
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		os.Remove(path)
		return nil, "", err
	}
	defer os.Remove(path)

	return r.scriptImage(ctx, osascriptCommand, darwinImageScript, path)
}

// text reads the pasteboard's text: pbpaste is the tool for that, and it is text-only by design
// (see the note above), which is exactly the half wanted here.
func (r reader) text(ctx context.Context) (string, error) {
	return r.textFrom(ctx, []clipboardCmd{{name: "pbpaste"}})
}
