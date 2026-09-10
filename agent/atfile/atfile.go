// Package atfile turns @-file references in a user message into LLM input.
//
// A reference starts with '@' at the beginning of the message or right after
// whitespace and runs up to the next whitespace. References resolve against a
// caller-supplied root — the TUI's working directory, the desktop session's
// working directory — so that a reference always points at the same tree the
// agent's tools operate on. Absolute and ~-rooted references resolve as-is.
//
// A reference expands by kind:
//
//   - directory → a file listing from ripgrep
//   - image     → a base64 content part for multi-modal input
//   - text file → inlined between UNTRUSTED FILE CONTENT markers
//   - binary    → annotated as [@file: path] so the agent reads it with tools
//   - missing   → left verbatim
//
// Only the message handed to the LLM is expanded; frontends keep displaying
// the raw text the user typed.
package atfile

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/llm"
)

const (
	// MaxInlineSize is the largest text file inlined into the message.
	MaxInlineSize = 256 * 1024
	// textProbeSize is how many leading bytes are scanned for NUL to tell text
	// files from binary ones.
	textProbeSize = 8000
	// dirListingTimeout bounds the ripgrep call that expands a directory.
	dirListingTimeout = 10 * time.Second

	// contentBeginFmt/contentEndFmt wrap inlined file content, marking it as
	// untrusted data the model must not follow as instructions.
	contentBeginFmt = "\n\n--- BEGIN UNTRUSTED FILE CONTENT: %s ---\n"
	contentEndFmt   = "\n--- END UNTRUSTED FILE CONTENT: %s ---"
	// imagePlaceholderFmt stands in for an image reference in the text stream.
	imagePlaceholderFmt = "[图片: %s]"
	// oversizedImageFmt annotates an image that is too large to attach, so the
	// model can tell "image I cannot see, and why" from "binary file".
	oversizedImageFmt = "[图片过大未内联: %s（超过 %dMB 上限）]"
	// binaryRefFmt annotates a reference the model must read with tools.
	binaryRefFmt = "[@file: %s]"
)

// Kind describes how an existing path expands.
type Kind int

const (
	// KindMissing means the path does not exist (the reference stays verbatim).
	KindMissing Kind = iota
	// KindDir means the path is a directory (expanded to a file listing).
	KindDir
	// KindImage means the path is a supported image within llm.MaxImageSize.
	KindImage
	// KindImageTooLarge means the path is a supported image beyond
	// llm.MaxImageSize (annotated by path, with the reason).
	KindImageTooLarge
	// KindText means the path is a text file within MaxInlineSize.
	KindText
	// KindBinary means the path is too large or not text (annotated by path).
	KindBinary
)

// Result is the outcome of expanding a message's @-references.
type Result struct {
	// Text is the message with every resolvable reference expanded.
	Text string
	// Images holds the image references as multi-modal content parts, in the
	// order they appeared in the message.
	Images []llm.ContentPart
	// Refs counts the references that resolved to something on disk.
	Refs int
}

// IsRefBoundary reports whether b can separate an @-reference from the text
// around it: a reference starts at the beginning of the message or after any
// whitespace, and ends at the next whitespace. Both the expansion scan and the
// frontends' cursor scans use this single rule.
func IsRefBoundary(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// LastRefStart returns the index of the '@' starting the last reference in s,
// or -1 when s contains no reference. Frontends use it to locate the reference
// the cursor is currently editing.
func LastRefStart(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '@' && (i == 0 || IsRefBoundary(s[i-1])) {
			return i
		}
	}
	return -1
}

// ResolvePath turns a reference into an absolute path: "~" expands to the home
// directory, an absolute reference is used as-is, and a relative one is joined
// to root.
func ResolvePath(root, ref string) string {
	if strings.HasPrefix(ref, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(ref, "~"), "/"))
		}
	}
	if filepath.IsAbs(ref) {
		return filepath.Clean(ref)
	}
	return filepath.Join(root, ref)
}

// Classify reports how an existing path expands, plus the file size in bytes
// (0 for directories and missing paths). Files whose content cannot be probed
// count as binary, so they are annotated rather than silently dropped.
func Classify(path string) (kind Kind, size int64) {
	info, err := os.Stat(path)
	if err != nil {
		return KindMissing, 0
	}
	if info.IsDir() {
		return KindDir, 0
	}
	size = info.Size()

	if _, ok := llm.ImageMediaType(filepath.Ext(path)); ok {
		if size > llm.MaxImageSize {
			return KindImageTooLarge, size
		}
		return KindImage, size
	}
	if size > MaxInlineSize {
		return KindBinary, size
	}
	isText, err := looksLikeText(path)
	switch {
	case err != nil:
		return KindBinary, size
	case isText:
		return KindText, size
	default:
		return KindBinary, size
	}
}

// Expand rewrites every @-reference in message. References that do not resolve
// are left exactly as the user typed them.
func Expand(root, message string) Result {
	var b strings.Builder
	var images []llm.ContentPart
	refs := 0

	for i := 0; i < len(message); {
		if message[i] != '@' || (i > 0 && !IsRefBoundary(message[i-1])) {
			b.WriteByte(message[i])
			i++
			continue
		}

		j := i + 1
		for j < len(message) && !IsRefBoundary(message[j]) {
			j++
		}
		ref := message[i+1 : j]
		i = j

		if ref == "" {
			b.WriteByte('@')
			continue
		}
		if expandRef(&b, root, ref, &images) {
			refs++
		}
	}

	return Result{Text: b.String(), Images: images, Refs: refs}
}

// RefForPath returns the @-reference text for a path on disk — relative to root
// when the path lives under it, absolute otherwise. ok is false when the path
// does not exist.
//
// Note: a path containing whitespace produces a reference that the expansion
// scanner cannot read back (it would stop at the first space). Callers may
// still insert it; the reference is then passed through verbatim.
func RefForPath(root, path string) (ref string, ok bool) {
	if _, err := os.Stat(path); err != nil {
		return "", false
	}
	if rel, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(rel, "..") {
		return "@" + filepath.ToSlash(rel), true
	}
	return "@" + filepath.ToSlash(path), true
}

// RefsForPaths maps existing paths to a space-separated reference list, as
// inserted into the input area when files are dropped onto it. Paths that do
// not exist are skipped; ok is false when none of them do.
func RefsForPaths(root string, paths []string) (refs string, ok bool) {
	var out []string
	for _, p := range paths {
		ref, exists := RefForPath(root, p)
		if exists {
			out = append(out, ref)
		}
	}
	if len(out) == 0 {
		return "", false
	}
	return strings.Join(out, " "), true
}

// expandRef writes the expansion of a single reference to b, appending any
// image part to images. It reports whether the reference resolved to an
// existing path.
func expandRef(b *strings.Builder, root, ref string, images *[]llm.ContentPart) bool {
	fullPath := ResolvePath(root, ref)

	switch kind, _ := Classify(fullPath); kind {
	case KindDir:
		listing := dirListing(fullPath)
		if listing == "" {
			b.WriteString("@" + ref)
			return false
		}
		writeContent(b, ref, listing)
		return true
	case KindImage:
		part, ok := readImagePart(fullPath)
		if !ok {
			b.WriteString("@" + ref)
			return false
		}
		*images = append(*images, part)
		fmt.Fprintf(b, imagePlaceholderFmt, ref)
		return true
	case KindText:
		content, ok := readTextContent(fullPath)
		if !ok {
			b.WriteString("@" + ref)
			return false
		}
		writeContent(b, ref, content)
		return true
	case KindImageTooLarge:
		fmt.Fprintf(b, oversizedImageFmt, ref, llm.MaxImageSize/(1024*1024))
		return true
	case KindBinary:
		fmt.Fprintf(b, binaryRefFmt, ref)
		return true
	default:
		b.WriteString("@" + ref)
		return false
	}
}

// writeContent inlines file content between the UNTRUSTED markers, keeping the
// reference itself visible in front of the block.
func writeContent(b *strings.Builder, label, content string) {
	b.WriteString("@" + label)
	fmt.Fprintf(b, contentBeginFmt, label)
	b.WriteString(content)
	fmt.Fprintf(b, contentEndFmt, label)
}

// looksLikeText reports whether a file looks like text: no NUL byte within the
// first textProbeSize bytes.
func looksLikeText(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	buf := make([]byte, textProbeSize)
	n, err := f.Read(buf)
	// An empty file (immediate EOF) is text; any other read error is not.
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return !bytes.Contains(buf[:n], []byte{0}), nil
}

// readImagePart reads a supported image file as a base64 content part.
func readImagePart(path string) (llm.ContentPart, bool) {
	mimeType, ok := llm.ImageMediaType(filepath.Ext(path))
	if !ok {
		return llm.ContentPart{}, false
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() > llm.MaxImageSize {
		return llm.ContentPart{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return llm.ContentPart{}, false
	}
	return llm.ContentPart{
		Type:      llm.ContentPartImage,
		MediaType: mimeType,
		Data:      base64.StdEncoding.EncodeToString(data),
	}, true
}

// readTextContent reads a text file within MaxInlineSize.
func readTextContent(path string) (string, bool) {
	info, err := os.Stat(path)
	if err != nil || info.Size() > MaxInlineSize {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(data), true
}

// dirListing lists the files under dir (gitignore-aware, .git excluded) so the
// model sees the shape of a referenced directory without reading every file.
func dirListing(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), dirListingTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "rg", "--files", "--hidden", "--glob", "!.git")
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		return ""
	}

	var files []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(output)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			files = append(files, filepath.ToSlash(line))
		}
	}
	if len(files) == 0 {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Directory contains %d files:\n", len(files))
	for _, f := range files {
		fmt.Fprintf(&b, "  %s\n", f)
	}
	return b.String()
}
