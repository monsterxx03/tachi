package main

// Attachment preview: what the file card behind a SendFile call can show.
//
// An attachment card is rebuilt from the recorded SendFile tool call, so the
// transcript knows the path and nothing else (see desktop/attach.go). Rendering
// it well needs two things the card cannot work out by itself: what KIND of file
// this is (rendered markdown? sandboxed HTML? an image? a PDF for the webview's
// own viewer? a CSV as a table? highlighted source?) and, for the text and table
// kinds, the content. Both are answered here, which keeps the extension tables in
// one place — a matching table in TypeScript would drift the first time either
// side learned a new type.
//
// The card asks for metadata when it mounts (that is what draws the size and
// decides whether a preview is offered at all) and for the content only once the
// user expands the preview: nothing reads a file the user did not ask to see.

import (
	"encoding/csv"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/monsterxx03/tachi/pkg/fileutil"
)

const (
	// maxPreviewBytes caps how much of a text file is handed to the webview: far
	// past any README or source file, small enough that a mis-targeted 200MB log
	// cannot lock the UI up on its first render.
	maxPreviewBytes = 1 << 20 // 1 MiB

	// maxCSVRows caps the table a CSV preview hands over. A 100k-row export would
	// be a 100k-row DOM to lay out, and nobody reads past the first screenful —
	// the rest is one "打开" away. Rows past the cap are reported as truncated
	// rather than silently dropped.
	maxCSVRows = 200
	// maxCSVCols caps how wide a row is kept. Columns are CUT, rows are not: a
	// wide export should still show its first columns, and a ragged row must not
	// disappear from the table.
	maxCSVCols = 40

	// Preview kinds. The frontend switches on these strings; a kind it does not
	// recognise falls back to "no preview here — open the file instead".
	kindImage    = "image"
	kindMarkdown = "markdown"
	kindHTML     = "html"
	kindPDF      = "pdf"
	kindCSV      = "csv"
	kindText     = "text"
	kindBinary   = "binary"
	kindDir      = "dir"
	kindMissing  = "missing"
)

// imageExts are the extensions the WEBVIEW renders inline. Deliberately not
// llm.ImageMediaType, which answers a different question — "what can the MODEL
// be shown": SVG, BMP and AVIF display on screen perfectly well while never
// being attachable to a message.
var imageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
	".svg": true, ".bmp": true, ".avif": true, ".ico": true,
}

// binaryExts are containers a NUL probe can miss — a zip's local file headers
// begin with ASCII, a .docx is a zip around XML — so they are called binary by
// extension instead. The card then offers no preview, only "open".
var binaryExts = map[string]bool{
	".zip": true, ".tar": true, ".gz": true, ".tgz": true,
	".bz2": true, ".xz": true, ".7z": true, ".rar": true,
	".dmg": true, ".pkg": true, ".exe": true, ".dll": true, ".so": true,
	".dylib": true, ".o": true, ".a": true, ".class": true, ".jar": true,
	".wasm": true, ".pyc": true, ".bin": true, ".dat": true,
	".woff": true, ".woff2": true, ".ttf": true, ".otf": true, ".eot": true,
	".doc": true, ".docx": true, ".xls": true, ".xlsx": true,
	".ppt": true, ".pptx": true, ".pages": true, ".numbers": true, ".key": true,
	".sqlite": true, ".db": true,
	".mp3": true, ".wav": true, ".flac": true, ".aac": true, ".m4a": true,
	".ogg": true, ".opus": true, ".mp4": true, ".mov": true, ".avi": true,
	".mkv": true, ".webm": true,
}

// langByExt maps an extension to the highlight.js language a text preview is
// tokenised as. Every name here exists in highlight.js's `common` bundle, which
// is what rehype-highlight registers by default: naming a language outside it
// highlights nothing (the plugin skips it), so entries are kept to grammars that
// will actually colour the code. Two are nearest-neighbour rather than exact —
// no TOML or Vue grammar ships in that bundle — and are noted where they occur.
var langByExt = map[string]string{
	".go": "go",
	".py": "python", ".pyi": "python",
	".js": "javascript", ".mjs": "javascript", ".cjs": "javascript", ".jsx": "javascript",
	".ts": "typescript", ".tsx": "typescript", ".mts": "typescript", ".cts": "typescript",
	".json": "json", ".jsonc": "json", ".jsonl": "json",
	".yaml": "yaml", ".yml": "yaml",
	".toml": "ini", // no TOML grammar in the bundle; INI's shape is the closest
	".ini":  "ini", ".cfg": "ini", ".conf": "ini", ".properties": "ini", ".env": "ini",
	".sh": "bash", ".bash": "bash", ".zsh": "bash", ".fish": "bash",
	".css": "css", ".scss": "scss", ".less": "less",
	".html": "xml", ".htm": "xml", ".xml": "xml", ".plist": "xml", ".svg": "xml",
	".vue": "xml", ".svelte": "xml", // markup-ish, so the XML grammar reads them
	".sql": "sql",
	".rs":  "rust", ".java": "java", ".cs": "csharp", ".rb": "ruby", ".php": "php",
	".swift": "swift", ".kt": "kotlin", ".kts": "kotlin", ".lua": "lua",
	".pl": "perl", ".pm": "perl", ".r": "r",
	".c": "c", ".h": "c",
	".cpp": "cpp", ".cc": "cpp", ".cxx": "cpp", ".hpp": "cpp", ".hh": "cpp", ".hxx": "cpp",
	".m": "objectivec", ".mm": "objectivec",
	".graphql": "graphql", ".gql": "graphql",
	".diff": "diff", ".patch": "diff",
	".mk": "makefile",
}

// langByBase names the extensionless files worth colouring, matched on the whole
// base name.
var langByBase = map[string]string{
	"Makefile": "makefile", "makefile": "makefile", "GNUmakefile": "makefile",
	"Gemfile": "ruby", "Rakefile": "ruby",
	".bashrc": "bash", ".zshrc": "bash", ".bash_profile": "bash", ".profile": "bash",
}

// FilePreviewVO describes one attachment to its card. It is served in two
// stages: metadata alone when the card mounts, then the same struct with Text
// filled once the user expands the preview.
type FilePreviewVO struct {
	Path string `json:"path"`
	Name string `json:"name"`
	// Kind decides how the card renders the file (one of the kind constants).
	Kind string `json:"kind"`
	// Size is the file's size in bytes (0 for directories and missing paths), for
	// the card's "README.md · 12.4 KB" line.
	Size int64 `json:"size"`
	// Language is the highlight.js language for text kinds, "" when the file is
	// plain text (or not text at all).
	Language string `json:"language"`
	// Previewable says whether the card should offer a preview. Derived here
	// rather than in the frontend so the kind table stays in one place.
	Previewable bool `json:"previewable"`
	// Text is the file's content, only for the text kinds and only when the
	// caller asked for content. Images, HTML and PDF are served straight from
	// disk by the /local asset handler instead, which is what lets a large file
	// stream rather than arrive as one string.
	Text string `json:"text"`
	// Rows is the parsed table of a CSV preview (row 0 is the header, per the
	// spreadsheet convention). The parsing lives here rather than in the
	// frontend: quoted fields, embedded newlines and European ";" separators are
	// what a real export contains, and encoding/csv already knows all of it.
	Rows [][]string `json:"rows"`
	// Truncated marks the content as the beginning of a larger file: Text past
	// maxPreviewBytes, a table past maxCSVRows.
	Truncated bool `json:"truncated"`
	// Error is set when the file is there but could not be read; Kind then still
	// says what it would have been.
	Error string `json:"error"`
}

// PreviewFile describes the attachment at path and, when withContent is set,
// returns the content of its text and table kinds.
//
// It never fails outright: a missing file, a directory and an unreadable file are
// all ordinary in a transcript that remembers a SendFile from an earlier run, and
// each has a sentence the card can render. A deleted report.md comes back as
// kindMissing, not as an error the UI would have to invent wording for.
func (s *AgentService) PreviewFile(path string, withContent bool) FilePreviewVO {
	vo := FilePreviewVO{Path: path, Name: filepath.Base(path)}

	info, err := os.Stat(path)
	if err != nil {
		vo.Kind = kindMissing
		if !errors.Is(err, fs.ErrNotExist) {
			vo.Error = err.Error()
		}
		return vo
	}
	if info.IsDir() {
		vo.Kind = kindDir
		return vo
	}

	vo.Size = info.Size()
	vo.Kind, vo.Language, err = previewKind(path)
	if err != nil {
		vo.Error = err.Error()
	}
	// Previewable is decided here rather than in the frontend, so the kind table
	// stays in one place. A file we could not read offers no preview either: the
	// card shows the reason instead of an empty frame.
	vo.Previewable = vo.Kind != kindBinary && vo.Error == ""
	if !withContent || !vo.Previewable || !needsContent(vo.Kind) {
		return vo
	}

	text, truncated, err := readPreviewText(path, info.Size())
	if err != nil {
		vo.Error, vo.Previewable = err.Error(), false
		return vo
	}
	vo.Truncated = truncated

	if vo.Kind == kindCSV {
		rows, more := parseCSVPreview(text, strings.EqualFold(filepath.Ext(path), ".tsv"))
		// The table IS the preview, so the text does not travel as well — it is
		// the larger half of the payload for no additional information.
		vo.Rows, vo.Text, vo.Truncated = rows, "", vo.Truncated || more
		return vo
	}

	vo.Text = text
	return vo
}

// needsContent reports which kinds are rendered from text the frontend receives.
// The others (image, HTML, PDF) are fetched from disk by /local at render time,
// so their content never travels through the IPC channel.
func needsContent(kind string) bool {
	return kind == kindMarkdown || kind == kindText || kind == kindCSV
}

// previewKind classifies an existing regular file: how to render it, which
// language to colour it as, and — when its content cannot be read — why. That
// error is not fatal: the file still exists, the card just cannot show it.
//
// Everything without a dedicated renderer is decided by CONTENT, not by
// extension: an extension alone cannot tell a README named "NOTES" from a
// machine dump named "notes".
func previewKind(path string) (kind, language string, err error) {
	name := filepath.Base(path)
	ext := strings.ToLower(filepath.Ext(name))

	switch {
	case imageExts[ext]:
		return kindImage, "", nil
	case ext == ".md" || ext == ".markdown":
		return kindMarkdown, "", nil
	case ext == ".html" || ext == ".htm" || ext == ".xhtml":
		return kindHTML, "", nil
	case ext == ".pdf":
		return kindPDF, "", nil
	case ext == ".csv" || ext == ".tsv":
		return kindCSV, "", nil
	case binaryExts[ext]:
		return kindBinary, "", nil
	}

	text, err := fileutil.LooksLikeText(path)
	switch {
	case err != nil:
		// No dedicated renderer, and no readable content to probe. Report it as
		// text carrying the reason: everything this far down the table is text as
		// far as anyone can tell, and "unreadable: permission denied" is
		// actionable where "this type has no preview" would be a lie.
		return kindText, "", err
	case !text:
		return kindBinary, "", nil
	}
	if lang, ok := langByBase[name]; ok {
		return kindText, lang, nil
	}
	return kindText, langByExt[ext], nil // "" = no grammar; the preview stays plain text
}

// readPreviewText reads at most maxPreviewBytes of the file at path. A file that
// shrinks between the caller's stat and this read simply yields less text.
func readPreviewText(path string, size int64) (text string, truncated bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()

	limit := size
	if limit > maxPreviewBytes {
		limit, truncated = maxPreviewBytes, true
	}
	buf, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return "", false, err
	}
	if truncated {
		// A cap that lands mid-rune (most likely in CJK text, where a character
		// is three bytes) would be delivered as a replacement character; drop the
		// incomplete tail so the preview ends on a real one.
		buf = trimPartialRune(buf)
	}
	return string(buf), truncated, nil
}

// trimPartialRune drops an incomplete UTF-8 sequence at the end of b, if any.
func trimPartialRune(b []byte) []byte {
	for range utf8.UTFMax - 1 {
		if len(b) == 0 {
			return b
		}
		// An invalid tail decodes as (RuneError, 1); a small final rune decodes
		// as (RuneError, 3), which is a character in its own right and stays.
		if r, size := utf8.DecodeLastRune(b); r != utf8.RuneError || size > 1 {
			return b
		}
		b = b[:len(b)-1]
	}
	return b
}

// parseCSVPreview reads CSV content into the table the card renders, capped at
// maxCSVRows × maxCSVCols. Parsing lives here rather than in the frontend because
// encoding/csv already handles the two things naive splitting gets wrong: a
// quoted field containing the separator, and a quoted newline inside a field.
//
// It cannot fail. With FieldsPerRecord = -1 and LazyQuotes set, the reader accepts
// whatever a text file holds — ragged rows, hand-made quoting, an unterminated
// quote, a stray NUL — so a mislabelled .csv still previews as a table instead of
// leaving an empty frame. Row 0 is the header (the spreadsheet convention, and
// what every other CSV viewer shows); the bool result means "there was more than
// the table shows": further rows, or a row too wide to keep whole.
func parseCSVPreview(text string, tab bool) (rows [][]string, more bool) {
	// A BOM is what Excel writes in front of a UTF-8 export; left in place it
	// becomes part of the first header cell.
	text = strings.TrimPrefix(text, "\ufeff")

	r := csv.NewReader(strings.NewReader(text))
	r.Comma = ','
	if tab {
		r.Comma = '\t'
	} else {
		r.Comma = sniffComma(text)
	}
	r.FieldsPerRecord = -1
	r.LazyQuotes = true

	for {
		rec, err := r.Read()
		if err != nil {
			// io.EOF ends the table, and nothing else is reachable with the
			// settings above — but a reader that returned one would still leave
			// the rows read so far, which is what the card wants.
			return rows, more
		}
		if len(rec) == 1 && strings.TrimSpace(rec[0]) == "" {
			continue // blank line — a trailing newline is the normal case
		}
		if len(rows) == maxCSVRows {
			return rows, true
		}
		if len(rec) > maxCSVCols {
			rec, more = rec[:maxCSVCols], true
		}
		rows = append(rows, rec)
	}
}

// sniffComma picks the separator of a CSV whose name does not say: an export from
// a European locale uses ';', and some tools write '|'. Counting the candidates on
// the first line is enough — the one appearing most often outside quotes is the
// separator, and a single-column file simply finds none and keeps ','.
func sniffComma(text string) rune {
	line := text
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		line = text[:i]
	}
	best, bestCount := ',', 0
	for _, c := range []rune{',', ';', '\t', '|'} {
		if n := strings.Count(line, string(c)); n > bestCount {
			best, bestCount = c, n
		}
	}
	return best
}
