package llm

import "testing"

// TestImageMediaTypeForBytes is the check a caller with no file name has to lean on: the bytes are
// the only evidence of what a paste or a download is, and the near-misses matter more than the hits
// — a RIFF container that is not WebP, a truncated file, and plain text all have to come back "".
func TestImageMediaTypeForBytes(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		want string
	}{
		{"png", []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0x00}, "image/png"},
		{"jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0}, "image/jpeg"},
		{"gif87a", []byte("GIF87a\x00"), "image/gif"},
		{"gif89a", []byte("GIF89a\x00"), "image/gif"},
		{"webp", []byte("RIFF\x24\x00\x00\x00WEBPVP8 "), "image/webp"},
		{"riff but not webp", []byte("RIFF\x24\x00\x00\x00AVI LIST"), ""},
		// A bare "RIFF" is 4 bytes: the WebP tag sits at offset 8, and reading it unguarded is an
		// out-of-range slice — a real paste feeding this a truncated file would panic the process.
		{"truncated riff", []byte("RIFF"), ""},
		{"short png prefix", []byte{0x89, 'P', 'N'}, ""},
		{"text", []byte("@/Users/me/.tachi/session/pasted/paste-1.png"), ""},
		{"empty", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ImageMediaTypeForBytes(c.raw); got != c.want {
				t.Errorf("ImageMediaTypeForBytes(%q) = %q, want %q", c.raw, got, c.want)
			}
		})
	}
}
