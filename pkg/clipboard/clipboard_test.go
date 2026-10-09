package clipboard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pngBytes is a real (1×1) PNG, so the magic-number check in ImageMediaTypeForBytes accepts it.
var pngBytes = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D}

// fakeRunner answers clipboard commands from a map, and fails the ones it does not know — a
// stand-in for a machine that has one tool of the several a platform may try.
type fakeRunner struct {
	outputs map[string][]byte
	errs    map[string]error
	calls   []string
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	key := name
	if len(args) > 0 {
		key += " " + strings.Join(args, " ")
	}
	f.calls = append(f.calls, key)
	if err, ok := f.errs[key]; ok {
		return nil, err
	}
	if out, ok := f.outputs[key]; ok {
		return out, nil
	}
	return nil, errors.New("command not found: " + name)
}

// TestImageFromValidatesBytes pins the reason bytes are checked rather than trusted: a tool asked
// for an image type it does not have can still print the clipboard's TEXT (pbpaste does), and text
// handed on as an image would be attached to a request as an unreadable picture.
func TestImageFromValidatesBytes(t *testing.T) {
	cands := []clipboardCmd{{name: "tool", args: []string{"image/png"}}}

	f := &fakeRunner{outputs: map[string][]byte{"tool image/png": []byte("just some text the tool printed")}}
	raw, mediaType, err := reader{run: f.run}.imageFrom(context.Background(), cands)
	if !errors.Is(err, ErrNoImage) {
		t.Fatalf("imageFrom(text) err = %v, want ErrNoImage", err)
	}
	if raw != nil || mediaType != "" {
		t.Errorf("imageFrom(text) = %q, %q; want nothing", raw, mediaType)
	}

	f = &fakeRunner{outputs: map[string][]byte{"tool image/png": pngBytes}}
	raw, mediaType, err = reader{run: f.run}.imageFrom(context.Background(), cands)
	if err != nil || mediaType != "image/png" || len(raw) != len(pngBytes) {
		t.Errorf("imageFrom(png) = %q, %q, %v; want the png bytes", raw, mediaType, err)
	}
}

// TestImageFromSkipsRoutesThatAreNotThere: the candidates are alternatives (Wayland vs X11), so a
// route that is missing or fails is not the answer — the NEXT one is.
func TestImageFromSkipsRoutesThatAreNotThere(t *testing.T) {
	cands := []clipboardCmd{
		{name: "wl-paste", args: []string{"image/png"}},
		{name: "xclip", args: []string{"image/png"}},
	}

	f := &fakeRunner{outputs: map[string][]byte{"xclip image/png": pngBytes}}
	second := reader{run: f.run}
	if _, mediaType, err := second.imageFrom(context.Background(), cands); err != nil || mediaType != "image/png" {
		t.Errorf("imageFrom with only the second route present = %q, %v; want the png", mediaType, err)
	}

	// Neither route can even be run: the report is the failure, not "no image" — the two lead a
	// reader somewhere different (install a tool vs the clipboard simply holds no picture).
	none := reader{run: (&fakeRunner{}).run}
	if _, _, err := none.imageFrom(context.Background(), cands); err == nil || errors.Is(err, ErrNoImage) {
		t.Errorf("imageFrom with no route runnable err = %v, want the command failure", err)
	}
}

// TestTextFromReportsAnEmptyClipboardAsEmpty: the commands ran and printed nothing, which is a
// clipboard without text — not a failure.
func TestTextFromReportsAnEmptyClipboardAsEmpty(t *testing.T) {
	cands := []clipboardCmd{{name: "pbpaste"}}

	text, err := reader{run: (&fakeRunner{outputs: map[string][]byte{"pbpaste": {}}}).run}.textFrom(context.Background(), cands)
	if err != nil || text != "" {
		t.Errorf("textFrom(empty) = %q, %v; want empty with no error", text, err)
	}

	text, err = reader{run: (&fakeRunner{outputs: map[string][]byte{"pbpaste": []byte("hello\n")}}).run}.textFrom(context.Background(), cands)
	if err != nil || text != "hello" {
		t.Errorf("textFrom(text) = %q, %v; want %q (the trailing newline is the tool's own)", text, err, "hello")
	}
}

// TestScriptImageReadsWhatTheScriptWrote covers the macOS mechanism end to end minus osascript
// itself: the bytes come back from the file the script wrote, and a script with nothing to write
// says so through its token instead of leaving a stale file behind.
func TestScriptImageReadsWhatTheScriptWrote(t *testing.T) {
	dir := t.TempDir()

	writeScript := func(raw []byte, token string) runner {
		return func(_ context.Context, name string, args ...string) ([]byte, error) {
			// Last arg is the path the script was told to write to (the real script sees it as
			// argv[0]); the file itself only appears when there is an image.
			path := args[len(args)-1]
			if len(raw) > 0 {
				if err := os.WriteFile(path, raw, 0o600); err != nil {
					return nil, err
				}
			}
			return []byte(token), nil
		}
	}

	// A composite literal can't be the receiver of a call inside an if statement, so the scripted
	// reader is built through a function. Each case writes to its OWN path: a file left by the
	// previous one would answer for a script that wrote nothing.
	scripted := func(raw []byte, token string) reader { return reader{run: writeScript(raw, token)} }
	pathFor := func(name string) string { return filepath.Join(dir, name) }

	raw, mediaType, err := scripted(pngBytes, "ok").scriptImage(context.Background(), []string{"osascript", "-l", "JavaScript"}, "script", pathFor("clip.png"))
	if err != nil || mediaType != "image/png" || len(raw) != len(pngBytes) {
		t.Fatalf("scriptImage(ok) = %q, %v; want the png bytes", mediaType, err)
	}

	if _, _, err := scripted(nil, scriptWritesImage).scriptImage(context.Background(), []string{"osascript", "-l", "JavaScript"}, "script", pathFor("none.png")); !errors.Is(err, ErrNoImage) {
		t.Errorf("scriptImage(%s) err = %v, want ErrNoImage", scriptWritesImage, err)
	}

	// The script claimed success and wrote nothing: that is a malfunction, not an empty clipboard.
	if _, _, err := scripted(nil, "ok").scriptImage(context.Background(), []string{"osascript", "-l", "JavaScript"}, "script", pathFor("gone.png")); err == nil || errors.Is(err, ErrNoImage) {
		t.Errorf("scriptImage(ok, no file) err = %v, want a real error", err)
	}

	// Neither is a file that is there but is not an image: the bytes are validated, not assumed.
	if _, _, err := scripted([]byte("not a picture"), "ok").scriptImage(context.Background(), []string{"osascript", "-l", "JavaScript"}, "script", pathFor("junk.png")); err == nil {
		t.Errorf("scriptImage(junk) err = %v, want an error", err)
	}
}

// TestReadPrefersTheImage pins the choice Read documents: a clipboard that holds both (a browser
// copy can) pastes the image, which is the half the terminal cannot deliver.
func TestReadPrefersTheImage(t *testing.T) {
	hasImage := func(context.Context) ([]byte, string, error) { return pngBytes, "image/png", nil }
	hasText := func(context.Context) (string, error) { return "the page's text", nil }

	snap, err := readFrom(context.Background(), hasImage, hasText)
	if err != nil || len(snap.Image) == 0 || snap.Text != "" {
		t.Fatalf("readFrom() = %+v, %v; want the image", snap, err)
	}

	noImage := func(context.Context) ([]byte, string, error) { return nil, "", ErrNoImage }
	snap, err = readFrom(context.Background(), noImage, hasText)
	if err != nil || snap.Text != "the page's text" || len(snap.Image) != 0 {
		t.Fatalf("readFrom() = %+v, %v; want the text", snap, err)
	}
}

// TestReadOfAnEmptyClipboardIsNotAnError: nothing to paste is the ordinary outcome of pressing
// paste with nothing copied, and an error there would put a message on screen for it.
func TestReadOfAnEmptyClipboardIsNotAnError(t *testing.T) {
	noImage := func(context.Context) ([]byte, string, error) { return nil, "", ErrNoImage }
	noText := func(context.Context) (string, error) { return "", nil }

	snap, err := readFrom(context.Background(), noImage, noText)
	if err != nil || !snap.Empty() {
		t.Errorf("readFrom() = %+v, %v; want an empty snapshot with no error", snap, err)
	}

	// A route that is not there AT ALL is reported, so a broken read is not silently equal to an
	// empty clipboard — while an image failure alone (with text readable) still is not.
	unsupported := func(context.Context) ([]byte, string, error) { return nil, "", ErrUnsupported }
	noTextRoute := func(context.Context) (string, error) { return "", ErrUnsupported }
	if _, err := readFrom(context.Background(), unsupported, noTextRoute); !errors.Is(err, ErrUnsupported) {
		t.Errorf("readFrom(unsupported) err = %v, want ErrUnsupported", err)
	}
	if _, err := readFrom(context.Background(), noImage, noTextRoute); err != nil {
		t.Errorf("readFrom(no image, no text route) err = %v, want no error", err)
	}
}
