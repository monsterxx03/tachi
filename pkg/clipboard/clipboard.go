// Package clipboard reads the system clipboard's image — or its text — for a frontend that has to
// do the reading itself.
//
// Why this exists at all: a terminal's paste channel carries TEXT (bracketed paste), so a pasted
// screenshot never reaches the application as bytes, whatever the terminal does with the gesture.
// The only route to an image on the clipboard is to ask the OS for it, which is what Read does.
//
// Two mechanisms cover the platforms, because no single one does:
//
//   - a COMMAND that prints the clipboard's bytes (Linux's wl-paste / xclip, macOS's pbpaste for
//     text): cheap, and the bytes are validated by magic number because a tool that finds nothing
//     may still print its own noise (or the clipboard's text) instead of failing.
//   - an INTERPRETER SCRIPT that writes the image to a file (macOS): pbpaste handles text only —
//     its -Prefer takes txt/rtf/ps, and asking it for an image quietly yields nothing — while
//     osascript is always present and its JavaScript bridge can ask AppKit for the pasteboard.
//
// Read prefers the image: a clipboard holding one holds it because the reader just produced it
// (a screenshot), and a screenshot is also the only thing here a frontend cannot get any other way.
package clipboard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/llm"
)

// ErrNoImage reports a clipboard that holds no image: it has text, something unsupported, or
// nothing at all. It is the ordinary answer to "did the reader paste a picture?" — not a failure,
// and the reason a caller can stay quiet about it.
var ErrNoImage = errors.New("clipboard: no image")

// ErrUnsupported reports that this platform has no way to read the clipboard from a process. It is
// distinct from ErrNoImage so a caller can tell "there is nothing to paste" from "pasting is not
// possible here" in a log line.
var ErrUnsupported = errors.New("clipboard: reading the clipboard is unsupported on this platform")

// readTimeout bounds one read. The clipboard tools are local and instant, but a misbehaving
// selection owner (an X11 client holding the selection and never answering) would otherwise block
// the read — and this runs on a keystroke, where a hang is visible.
const readTimeout = 3 * time.Second

// Snapshot is what the clipboard held when it was read: at most one of the two fields is filled,
// and image wins (see Read).
type Snapshot struct {
	// Image is the image's bytes and MediaType its MIME type (one of llm's supported images).
	Image     []byte
	MediaType string
	// Text is the clipboard's text, "" when it holds none.
	Text string
}

// Empty reports whether the clipboard held anything pasteable.
func (s Snapshot) Empty() bool { return len(s.Image) == 0 && s.Text == "" }

// Read returns the clipboard's contents: its image when it has one, else its text.
//
// A nil error with an empty Snapshot is the honest answer to an empty clipboard; an error means the
// clipboard could not be read at all, which is worth a log line and nothing more.
func Read(ctx context.Context) (Snapshot, error) { return defaultReader().read(ctx) }

// clipboardCmd is one way to ask the OS for the clipboard's bytes.
type clipboardCmd struct {
	name string
	args []string
}

// runner runs one clipboard command and returns its stdout. Injectable so the parsing and the
// validation below — the parts that can be wrong — are testable without a clipboard.
type runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// reader is the zero-value-is-real thing: a reader{} runs the commands for real, and a test fills
// in run to hand back bytes it chose.
type reader struct{ run runner }

func defaultReader() reader { return reader{} }

// exec runs one clipboard command through the injected runner when there is one.
func (r reader) exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	if r.run != nil {
		return r.run(ctx, name, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		// The stderr text is what names the real reason (no DISPLAY, no selection owner), and it is
		// only read when the command FAILED — a successful run's stderr is AppKit chatter.
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(errOut.String()))
	}
	return out.Bytes(), nil
}

// read is the one place the image-first policy, the time bound and the "empty is not an error"
// rule live, so every caller (both paste gestures of the TUI, and whatever comes next) agrees.
func (r reader) read(ctx context.Context) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	return readFrom(ctx, r.image, r.text)
}

// readFrom is read's policy with the platform's two halves handed in, so the policy itself — which
// is the part with a decision in it — can be exercised without a clipboard to read.
func readFrom(
	ctx context.Context,
	image func(context.Context) ([]byte, string, error),
	text func(context.Context) (string, error),
) (Snapshot, error) {
	raw, mediaType, imageErr := image(ctx)
	if imageErr == nil {
		return Snapshot{Image: raw, MediaType: mediaType}, nil
	}
	// No image: the clipboard's text is still a paste. The reader pressed paste; text is simply the
	// half the terminal would have handled by itself had it been able to.
	clipText, textErr := text(ctx)
	if clipText != "" {
		return Snapshot{Text: clipText}, nil
	}
	// Nothing pasteable. A clipboard we COULD read (however empty) is not a failure — only one we
	// could not read at all is, and that is what the image route's error says.
	if errors.Is(imageErr, ErrNoImage) {
		return Snapshot{}, nil
	}
	if textErr != nil && !errors.Is(textErr, ErrUnsupported) {
		return Snapshot{}, textErr
	}
	return Snapshot{}, imageErr
}

// imageFrom asks each candidate in turn for the clipboard's bytes and returns the first that
// yields a real image.
//
// A candidate that FAILS is skipped rather than reported: these are alternative routes (Wayland vs
// X11), and a route that is not there is not an error. What is left over is ErrNoImage, except when
// not one of them could even be run — then the routes are missing, and the last command's error
// says which one was tried last.
func (r reader) imageFrom(ctx context.Context, candidates []clipboardCmd) ([]byte, string, error) {
	var lastErr error
	ran := false
	for _, c := range candidates {
		out, err := r.exec(ctx, c.name, c.args...)
		if err != nil {
			lastErr = err
			continue
		}
		ran = true
		// Validated rather than trusted: a tool asked for an image type it cannot find may print
		// the clipboard's TEXT instead of failing (pbpaste does exactly that), and text passed on
		// as image bytes would be attached to a request as an unreadable picture.
		if mediaType := llm.ImageMediaTypeForBytes(out); mediaType != "" {
			return out, mediaType, nil
		}
	}
	if !ran && lastErr != nil {
		return nil, "", lastErr
	}
	return nil, "", ErrNoImage
}

// textFrom is imageFrom's other half: the clipboard's text from the first candidate that produces
// any. An empty string with a nil error means the clipboard holds no text — the commands ran, there
// was nothing to print.
func (r reader) textFrom(ctx context.Context, candidates []clipboardCmd) (string, error) {
	var lastErr error
	ran := false
	for _, c := range candidates {
		out, err := r.exec(ctx, c.name, c.args...)
		if err != nil {
			lastErr = err
			continue
		}
		ran = true
		// Trailing newlines are the tools' own doing (wl-paste appends one by default), not part of
		// what the reader copied.
		if text := strings.TrimRight(string(out), "\r\n"); text != "" {
			return text, nil
		}
	}
	if !ran && lastErr != nil {
		return "", lastErr
	}
	return "", nil
}

// scriptWritesImage is the token a script prints when the clipboard holds no image. The script
// decides it (see clipboard_darwin.go) and this end only has to recognise it: the alternative is
// guessing, since "produced no file" reads the same whether the clipboard was empty or the write
// itself failed.
const scriptWritesImage = "no-image"

// scriptImage runs an interpreter script that has been asked to write the clipboard's image to
// outPath, then reads back and validates what it wrote. interpreter is the command and its own
// flags (the platform file names it); the script and the path are appended.
//
// The bytes travel through a FILE rather than stdout because that is what the macOS bridge can do
// with pasteboard data: its JavaScript-typed proxy is not a byte stream a shell can pass along.
func (r reader) scriptImage(ctx context.Context, interpreter []string, script, outPath string) ([]byte, string, error) {
	args := append(append([]string{}, interpreter[1:]...), "-e", script, outPath)
	out, err := r.exec(ctx, interpreter[0], args...)
	if err != nil {
		return nil, "", err
	}
	if bytes.Contains(out, []byte(scriptWritesImage)) {
		return nil, "", ErrNoImage
	}
	return readWrittenImage(outPath)
}

// readWrittenImage reads back what a script wrote and refuses anything that is not an image: the
// script has no business writing anything else, so a file of another shape is a malfunction worth
// an error rather than a silent ErrNoImage.
func readWrittenImage(path string) ([]byte, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		return nil, "", fmt.Errorf("clipboard: script wrote no image to %s: %w", path, err)
	}
	mediaType := llm.ImageMediaTypeForBytes(raw)
	if mediaType == "" {
		return nil, "", fmt.Errorf("clipboard: script wrote %s, which is not a supported image", path)
	}
	return raw, mediaType, nil
}
