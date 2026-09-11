package agent

import (
	"testing"
)

func TestNormalizeAdditionalRoots(t *testing.T) {
	tests := []struct {
		name    string
		primary string
		in      []string
		want    []string
		wantErr bool
	}{
		{name: "nil is nil", primary: "/cwd", in: nil},
		{name: "empty is nil", primary: "/cwd", in: []string{}},
		{name: "absolute paths keep their order", primary: "/cwd", in: []string{"/b", "/a"}, want: []string{"/b", "/a"}},
		{name: "empty entry rejected", primary: "/cwd", in: []string{"/a", ""}, wantErr: true},
		{name: "relative entry rejected", primary: "/cwd", in: []string{"relative/path"}, wantErr: true},
		{name: "duplicates and primary dropped, order kept", primary: "/cwd",
			in: []string{"/cwd", "/a", "/b", "/a", "/cwd", "/b"}, want: []string{"/a", "/b"}},
		{name: "cleaned paths dedupe against their cleaned twin", primary: "/cwd",
			in: []string{"/a/b/../c", "/a/c"}, want: []string{"/a/c"}},
		{name: "trailing slash dedupes with the bare path", primary: "/cwd",
			in: []string{"/a/", "/a"}, want: []string{"/a"}},
		{name: "primary is compared after cleaning", primary: "/a/b",
			in: []string{"/a/./b"}, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeAdditionalRoots(tt.primary, tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NormalizeAdditionalRoots(%q, %q) = %q, want an error", tt.primary, tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeAdditionalRoots(%q, %q): %v", tt.primary, tt.in, err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got %q, want %q", got, tt.want)
				}
			}
		})
	}
}

// TestNormalizeAdditionalRootsAcceptsSpacedPaths pins the boundary the desktop
// frontend layers ON TOP of this function rather than inside it: a path with a
// space is a perfectly good workspace root (an editor can hand ACP one), so the
// shared rule must keep accepting it.
func TestNormalizeAdditionalRootsAcceptsSpacedPaths(t *testing.T) {
	got, err := NormalizeAdditionalRoots("/cwd", []string{"/Users/x/My Projects/lib"})
	if err != nil {
		t.Fatalf("spaced root rejected by the shared rule: %v", err)
	}
	if len(got) != 1 || got[0] != "/Users/x/My Projects/lib" {
		t.Errorf("got %q, want the path unchanged", got)
	}
}
