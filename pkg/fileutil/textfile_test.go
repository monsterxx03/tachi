package fileutil

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestLooksLikeText(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	tests := []struct {
		name    string
		path    string
		want    bool
		wantErr bool
	}{
		{name: "empty file", path: write("empty.txt", nil), want: true},
		{name: "utf8 text", path: write("zh.txt", []byte("你好，world\n")), want: true},
		{name: "text under a binary-looking name", path: write("blob.dat", []byte("plain text\n")), want: true},
		{name: "binary under a text name", path: write("log.log", []byte("head\x00tail"))},
		{name: "nul past the probe window", path: write("late.bin", append(bytes.Repeat([]byte{'x'}, textProbeSize), 0)), want: true},
		{name: "missing", path: filepath.Join(dir, "nope"), wantErr: true},
		{name: "directory", path: dir, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := LooksLikeText(tt.path)
			if (err != nil) != tt.wantErr {
				t.Fatalf("LooksLikeText(%s) error = %v, wantErr %v", tt.name, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("LooksLikeText(%s) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestLooksLikeTextUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read anything")
	}
	path := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(path, []byte("nope"), 0o000); err != nil {
		t.Fatal(err)
	}
	// Unreadable is an error, NOT "binary": the callers word those two
	// differently for the user.
	if looksText, err := LooksLikeText(path); err == nil {
		t.Fatalf("LooksLikeText(unreadable) = %v, nil; want an error", looksText)
	}
}
