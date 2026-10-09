package main

import (
	"strings"
	"testing"
)

// TestOpenURLHandsTheURLToTheSystemLauncher pins the argument, the same way the session-dir probe
// does (see stubOpenFile): the fact worth asserting is WHICH string left the app, not that a
// browser drew a window — and a test that really launched one would open a tab on the machine
// running it, on every run.
func TestOpenURLHandsTheURLToTheSystemLauncher(t *testing.T) {
	calls := stubOpenFile(t)

	const raw = "https://example.com/a?b=c#d"
	if got := (&AgentService{}).OpenURL(raw); got != "ok" {
		t.Fatalf("OpenURL = %q, want ok", got)
	}
	if len(*calls) != 1 || len((*calls)[0]) != 1 || (*calls)[0][0] != raw {
		t.Fatalf("open calls = %q, want the URL handed over verbatim", *calls)
	}
}

// TestOpenURLRefusesAnythingButTheThreeSchemes is the guard that matters: the URL comes from a
// click on markdown a MODEL wrote, and the platform launcher opens whatever it has a handler for.
// `file:` in particular would turn a link in a reply into "open this path on your machine",
// outside every permission rule the file tools answer to — so the allowlist is checked by kind,
// and a refused URL launches nothing.
func TestOpenURLRefusesAnythingButTheThreeSchemes(t *testing.T) {
	for _, bad := range []string{
		"file:///etc/passwd",
		"javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"/etc/passwd",
		"example.com/no-scheme",
	} {
		calls := stubOpenFile(t)
		got := (&AgentService{}).OpenURL(bad)
		if got == "ok" {
			t.Errorf("OpenURL(%q) = ok, want a refusal", bad)
		}
		if len(*calls) != 0 {
			t.Errorf("OpenURL(%q) launched %q, want nothing launched", bad, *calls)
		}
	}
}

// TestOpenURLTakesTheSchemesCaseInsensitively: a scheme is case-insensitive by RFC 3986, and
// `HTTPS://…` is a URL a model can write. The launcher is handed the ORIGINAL string either way —
// normalising it would be a second way for the URL to be wrong.
func TestOpenURLTakesTheSchemesCaseInsensitively(t *testing.T) {
	calls := stubOpenFile(t)
	const raw = "HTTPS://Example.com/Path"
	if got := (&AgentService{}).OpenURL(raw); got != "ok" {
		t.Fatalf("OpenURL(%q) = %q, want ok", raw, got)
	}
	if len(*calls) != 1 || (*calls)[0][0] != raw {
		t.Fatalf("open calls = %q, want %q", *calls, raw)
	}
}

// TestOpenURLTrimsSurroundingSpace: a URL copied out of a wrapped sentence or a markdown link
// title routinely arrives with a leading/trailing space, and the parsed scheme is what decides —
// `" https://x"` parses as NO scheme and would be refused, which reads as "the link does not
// work". The trim is only for the CHECK: what the launcher gets is the trimmed string too, so
// there is one value and no second normalisation.
func TestOpenURLTrimsSurroundingSpace(t *testing.T) {
	calls := stubOpenFile(t)
	if got := (&AgentService{}).OpenURL("  https://example.com/x  "); got != "ok" {
		t.Fatalf("OpenURL = %q, want ok", got)
	}
	if len(*calls) != 1 || (*calls)[0][0] != "https://example.com/x" {
		t.Fatalf("open calls = %q, want the trimmed URL", *calls)
	}
}

// TestOpenURLReasonsAreReadable: the binding's answer is what a refusal says, and it has to name
// the thing that was refused — "not ok" would leave whoever debugs this guessing.
func TestOpenURLReasonsAreReadable(t *testing.T) {
	stubOpenFile(t)
	if got := (&AgentService{}).OpenURL("ftp://example.com/x"); !strings.Contains(got, "ftp") {
		t.Errorf("OpenURL(ftp://…) = %q, want the offending scheme named", got)
	}
}
