package lsp

import (
	"testing"
)

// =============================================================================
// Gitignore filter tests
// =============================================================================

// TestFilterGitIgnoredEmpty tests that empty/nil locations pass through.
func TestFilterGitIgnoredEmpty(t *testing.T) {
	if FilterGitIgnored(nil, "/test") != nil {
		t.Fatal("expected nil for nil input")
	}
	if len(FilterGitIgnored([]Location{}, "/test")) != 0 {
		t.Fatal("expected empty for empty input")
	}
}

// TestIsUnderIgnoredDir tests directory-prefix matching.
func TestIsUnderIgnoredDir(t *testing.T) {
	ignored := map[string]bool{
		"/project/node_modules":    true,
		"/project/vendor":          true,
		"/project/.next/cache/web": true,
	}

	tests := []struct {
		path     string
		expected bool
	}{
		{"/project/node_modules/foo.js", true},
		{"/project/node_modules/pkg/index.js", true},
		{"/project/vendor/lib.go", true},
		{"/project/.next/cache/web/pages/index.js", true},
		{"/project/src/main.go", false},
		{"/project/node_modules_backup/foo.js", false}, // not a prefix match
		{"/project/vendor.go", false},
		{"/other/node_modules/foo.js", false}, // different root
	}
	for _, tt := range tests {
		if got := isUnderIgnoredDir(tt.path, ignored); got != tt.expected {
			t.Errorf("isUnderIgnoredDir(%q) = %v, want %v", tt.path, got, tt.expected)
		}
	}
}
