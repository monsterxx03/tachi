package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// writePreviewFile writes content to a temp dir and returns the path.
func writePreviewFile(t *testing.T, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestPreviewFileKind covers the classification table: which renderer each file
// gets, and whether the card offers a preview at all.
func TestPreviewFileKind(t *testing.T) {
	svc := &AgentService{}
	dir := t.TempDir()
	nested := filepath.Join(dir, "sub")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, content []byte) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	tests := []struct {
		name        string
		path        string
		wantKind    string
		wantLang    string
		previewable bool
	}{
		{"markdown", write("report.md", []byte("# hi")), kindMarkdown, "", true},
		{"html", write("report.html", []byte("<p>hi</p>")), kindHTML, "", true},
		{"pdf gets the webview's own viewer", write("paper.pdf", []byte("%PDF-1.4\n1 0 obj\nendobj")), kindPDF, "", true},
		{"csv", write("rows.csv", []byte("a,b\n1,2\n")), kindCSV, "", true},
		{"tsv", write("rows.tsv", []byte("a\tb\n1\t2\n")), kindCSV, "", true},
		{"image", write("shot.png", []byte("\x89PNG\r\n\x1a\n")), kindImage, "", true},
		{"svg counts as an image even though the model cannot be shown it", write("icon.svg", []byte("<svg/>")), kindImage, "", true},
		{"go source", write("main.go", []byte("package main")), kindText, "go", true},
		{"extensionless text", write("NOTES", []byte("remember this")), kindText, "", true},
		{"language from the file name", write("Makefile", []byte("all:\n\tgo build")), kindText, "makefile", true},
		{"nul bytes make it binary", write("dump.out", []byte("a\x00b")), kindBinary, "", false},
		{"archive", write("src.tar.gz", []byte("\x1f\x8b")), kindBinary, "", false},
		{"directory", nested, kindDir, "", false},
		{"missing", filepath.Join(dir, "gone.md"), kindMissing, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vo := svc.PreviewFile(tt.path, false)
			if vo.Kind != tt.wantKind {
				t.Errorf("Kind = %q, want %q", vo.Kind, tt.wantKind)
			}
			if vo.Language != tt.wantLang {
				t.Errorf("Language = %q, want %q", vo.Language, tt.wantLang)
			}
			if vo.Previewable != tt.previewable {
				t.Errorf("Previewable = %v, want %v", vo.Previewable, tt.previewable)
			}
			if vo.Text != "" {
				t.Errorf("metadata call returned text (%d bytes)", len(vo.Text))
			}
			if vo.Name == "" {
				t.Error("Name is empty")
			}
		})
	}
}

// TestPreviewFileContent checks the second stage: which kinds carry text, and
// that asking for metadata alone never does.
func TestPreviewFileContent(t *testing.T) {
	svc := &AgentService{}

	md := writePreviewFile(t, "notes.md", []byte("# 标题\n\n正文\n"))
	vo := svc.PreviewFile(md, true)
	if vo.Kind != kindMarkdown || vo.Text != "# 标题\n\n正文\n" {
		t.Fatalf("markdown content = %q", vo.Text)
	}
	if vo.Truncated {
		t.Error("small file reported as truncated")
	}
	if vo.Size == 0 {
		t.Error("Size is 0")
	}

	// Images and HTML are served from disk by /local, so no text travels with
	// them no matter what the caller asked for.
	for _, path := range []string{
		writePreviewFile(t, "shot.png", []byte("\x89PNG\r\n\x1a\n")),
		writePreviewFile(t, "page.html", []byte("<p>hi</p>")),
	} {
		if vo := svc.PreviewFile(path, true); vo.Text != "" {
			t.Errorf("%s: text = %q, want none", filepath.Base(path), vo.Text)
		}
	}
}

// TestPreviewFileTruncates covers the size cap, including the rune boundary a
// byte cap can land on.
func TestPreviewFileTruncates(t *testing.T) {
	svc := &AgentService{}

	// ASCII: the cap is exact.
	big := writePreviewFile(t, "big.txt", bytes.Repeat([]byte("a"), maxPreviewBytes+1024))
	vo := svc.PreviewFile(big, true)
	if !vo.Truncated {
		t.Fatal("oversized file not marked truncated")
	}
	if len(vo.Text) != maxPreviewBytes {
		t.Errorf("text = %d bytes, want %d", len(vo.Text), maxPreviewBytes)
	}
	if vo.Size <= int64(maxPreviewBytes) {
		t.Errorf("Size = %d, want the full file size", vo.Size)
	}

	// CJK: three bytes per character, so the cap almost always lands mid-rune.
	cjk := writePreviewFile(t, "big.txt", bytes.Repeat([]byte("中"), maxPreviewBytes/3+16))
	vo = svc.PreviewFile(cjk, true)
	if !utf8.ValidString(vo.Text) {
		t.Error("truncated CJK text ends mid-rune")
	}
	if len(vo.Text) > maxPreviewBytes {
		t.Errorf("text = %d bytes, cap is %d", len(vo.Text), maxPreviewBytes)
	}
}

// TestPreviewFileUnreadable covers the file that is there but cannot be read:
// the kind still describes it, and the card gets a reason.
func TestPreviewFileUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read anything")
	}
	svc := &AgentService{}
	path := writePreviewFile(t, "secret.txt", []byte("nope"))
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	vo := svc.PreviewFile(path, true)
	if vo.Kind != kindText {
		t.Errorf("Kind = %q, want %q (what it would have been)", vo.Kind, kindText)
	}
	if vo.Error == "" {
		t.Error("Error is empty for an unreadable file")
	}
	if vo.Text != "" {
		t.Errorf("Text = %q, want none", vo.Text)
	}
}

// TestParseCSVPreview covers what a real export puts in the file: quoted
// separators, embedded newlines, ragged rows, a European separator, a BOM, and
// the two caps.
func TestParseCSVPreview(t *testing.T) {
	t.Run("quoted separator and embedded newline", func(t *testing.T) {
		rows, more := parseCSVPreview("id,note\n1,\"a, b\"\n2,\"line1\nline2\"\n", false)
		want := [][]string{{"id", "note"}, {"1", "a, b"}, {"2", "line1\nline2"}}
		if !slices.EqualFunc(rows, want, slices.Equal) {
			t.Errorf("rows = %q, want %q", rows, want)
		}
		if more {
			t.Error("more = true for a small table")
		}
	})

	t.Run("separator sniffing", func(t *testing.T) {
		for _, in := range []string{"a;b\n1;2\n", "a|b\n1|2\n", "a,b\n1,2\n"} {
			rows, _ := parseCSVPreview(in, false)
			if len(rows) != 2 || len(rows[0]) != 2 {
				t.Errorf("%q parsed as %q", in, rows)
			}
		}
	})

	t.Run("tsv keeps tabs even though the first line has no comma", func(t *testing.T) {
		rows, _ := parseCSVPreview("a\tb\n1\t2\n", true)
		if len(rows[0]) != 2 {
			t.Errorf("tsv header = %q, want two cells", rows[0])
		}
	})

	t.Run("excel BOM is not part of the first header", func(t *testing.T) {
		rows, _ := parseCSVPreview("\ufeffid,name\n1,tachi\n", false)
		if rows[0][0] != "id" {
			t.Errorf("first header = %q, want %q", rows[0][0], "id")
		}
	})

	t.Run("ragged rows and blank lines", func(t *testing.T) {
		rows, _ := parseCSVPreview("a,b,c\n1,2\n\n", false)
		if len(rows) != 2 {
			t.Fatalf("rows = %q, want header + one row", rows)
		}
		if len(rows[1]) != 2 {
			t.Errorf("short row = %q, want its two cells kept", rows[1])
		}
	})

	t.Run("capped rows report that there was more", func(t *testing.T) {
		var b strings.Builder
		b.WriteString("n\n")
		for i := range maxCSVRows + 10 {
			fmt.Fprintf(&b, "%d\n", i)
		}
		rows, more := parseCSVPreview(b.String(), false)
		if len(rows) != maxCSVRows {
			t.Errorf("rows = %d, want the cap %d", len(rows), maxCSVRows)
		}
		if !more {
			t.Error("more = false with rows left over")
		}
	})

	t.Run("wide rows are cut, not dropped", func(t *testing.T) {
		row := strings.TrimSuffix(strings.Repeat("x,", maxCSVCols+5), ",")
		rows, more := parseCSVPreview(row+"\n", false)
		if len(rows) != 1 || len(rows[0]) != maxCSVCols {
			t.Fatalf("rows = %d cells in %d rows, want %d in 1", len(rows[0]), len(rows), maxCSVCols)
		}
		if !more {
			t.Error("more = false although the row was cut")
		}
	})
}

// TestPreviewFileCSV is the card's path: a CSV comes back as a table, not as text.
func TestPreviewFileCSV(t *testing.T) {
	svc := &AgentService{}
	path := writePreviewFile(t, "rows.csv", []byte("name,score\n张,91\n李,88\n"))

	vo := svc.PreviewFile(path, true)
	if vo.Kind != kindCSV {
		t.Fatalf("Kind = %q, want %q", vo.Kind, kindCSV)
	}
	if len(vo.Rows) != 3 || vo.Rows[2][0] != "李" {
		t.Errorf("Rows = %q", vo.Rows)
	}
	if vo.Text != "" {
		t.Errorf("Text = %q, want none (the table is the preview)", vo.Text)
	}
	if vo.Truncated {
		t.Error("Truncated = true for a small table")
	}

	// Metadata alone still carries no table.
	if vo := svc.PreviewFile(path, false); len(vo.Rows) != 0 {
		t.Errorf("metadata call returned %d rows", len(vo.Rows))
	}
}

// TestPreviewFilePDF covers the kind served entirely by the webview.
func TestPreviewFilePDF(t *testing.T) {
	svc := &AgentService{}
	path := writePreviewFile(t, "paper.pdf", []byte("%PDF-1.4\n%%EOF\n"))
	vo := svc.PreviewFile(path, true)
	if vo.Kind != kindPDF || !vo.Previewable {
		t.Fatalf("Kind = %q previewable = %v, want pdf/previewable", vo.Kind, vo.Previewable)
	}
	if vo.Text != "" || len(vo.Rows) != 0 {
		t.Error("a PDF should not ship content over the IPC channel")
	}
}

// TestLocalAssetPath covers the route parsing behind /local.
func TestLocalAssetPath(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"/local?p=/tmp/a.png", "/tmp/a.png"},
		{"/local/Users/me/report.html", "/Users/me/report.html"},
		{"/local/Users/me/a%20b.txt", "/Users/me/a b.txt"},
		{"/local/Users/me/../me/c.txt", "/Users/me/c.txt"},
		{"/local", ""},
		{"/local/", ""},
		{"/local?p=", ""},
		{"/index.html", ""},
		{"/assets/app.js", ""},
		{"/localx/a.txt", ""},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tt.url, nil)
			if got := localAssetPath(r); got != tt.want {
				t.Errorf("localAssetPath(%s) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

// TestAssetHandlerServesLocalFiles is the end-to-end point of the path route:
// a previewed HTML document's OWN relative references have to come back to the
// same handler, which only works if the disk path is in the URL path.
func TestAssetHandlerServesLocalFiles(t *testing.T) {
	dir := t.TempDir()
	html := filepath.Join(dir, "report.html")
	if err := os.WriteFile(html, []byte("<p>hi</p>"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A sibling asset with a space in its name, referenced relatively from the
	// document above.
	asset := filepath.Join(dir, "chart one.js")
	if err := os.WriteFile(asset, []byte("chart()"), 0o644); err != nil {
		t.Fatal(err)
	}

	handler := assetHandler(assets)
	get := func(rawURL string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, rawURL, nil))
		return rec
	}

	// Query form (markdown images).
	if rec := get("/local?p=" + url.QueryEscape(html)); rec.Code != http.StatusOK || rec.Body.String() != "<p>hi</p>" {
		t.Errorf("query form: %d %q", rec.Code, rec.Body.String())
	}

	// Path form: the document itself...
	docURL := "/local" + strings.ReplaceAll(html, " ", "%20")
	if rec := get(docURL); rec.Code != http.StatusOK || rec.Body.String() != "<p>hi</p>" {
		t.Errorf("path form: %d %q", rec.Code, rec.Body.String())
	}

	// ...and the relative reference the browser derives from it.
	relURL := strings.TrimSuffix(docURL, "report.html") + "chart%20one.js"
	if rec := get(relURL); rec.Code != http.StatusOK || rec.Body.String() != "chart()" {
		t.Errorf("relative asset: %d %q", rec.Code, rec.Body.String())
	}

	// Anything that is not /local still goes to the embedded frontend.
	if rec := get("/no-such-asset.js"); rec.Code == http.StatusOK {
		t.Error("unknown asset served from the local file route")
	}
}
