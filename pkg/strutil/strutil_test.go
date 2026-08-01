package strutil

import "testing"

func TestTruncate(t *testing.T) {
	cases := []struct {
		name string
		s    string
		max  int
		want string
	}{
		{"short string passthrough", "short", 120, "short"},
		{"exact length passthrough", "abcdefghij", 10, "abcdefghij"},
		{"truncate ascii", "abcdefghij", 5, "abcde..."},
		{"max zero", "abc", 0, ""},
		{"max negative", "abc", -1, ""},
		{"empty string", "", 5, ""},
		// Multi-byte: must never split a rune mid-sequence.
		{"chinese within limit", "中文测试", 10, "中文测试"},
		{"chinese truncated", "中文测试中文测试", 4, "中文测试..."},
		{"mixed ascii and chinese", "a中b文c", 3, "a中b..."},
		{"emoji surrogate pair", "a😀b😀c", 4, "a😀b😀..."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Truncate(tc.s, tc.max); got != tc.want {
				t.Errorf("Truncate(%q, %d) = %q, want %q", tc.s, tc.max, got, tc.want)
			}
		})
	}
}

func TestTruncatePlain(t *testing.T) {
	cases := []struct {
		name string
		s    string
		max  int
		want string
	}{
		{"short passthrough", "short", 120, "short"},
		{"ascii truncate", "abcdefghij", 5, "abcde"},
		{"chinese truncate", "中文测试中文", 3, "中文测"},
		{"max zero", "abc", 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TruncatePlain(tc.s, tc.max); got != tc.want {
				t.Errorf("TruncatePlain(%q, %d) = %q, want %q", tc.s, tc.max, got, tc.want)
			}
		})
	}
}

func TestTruncateFitted(t *testing.T) {
	cases := []struct {
		name string
		s    string
		max  int
		want string
	}{
		{"short passthrough", "hello", 10, "hello"},
		{"exact length passthrough", "hello", 5, "hello"},
		{"ascii fitted", "hello world", 8, "hello w…"},
		{"ellipsis counts toward max", "abcdefghij", 5, "abcd…"},
		{"max one", "abcdef", 1, "…"},
		{"max one short", "a", 1, "a"},
		{"chinese fitted", "中文测试中文", 4, "中文测…"},
		{"max zero", "abc", 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TruncateFitted(tc.s, tc.max)
			if got != tc.want {
				t.Errorf("TruncateFitted(%q, %d) = %q, want %q", tc.s, tc.max, got, tc.want)
			}
			// Result must never exceed max runes.
			if tc.max > 0 && len([]rune(got)) > tc.max {
				t.Errorf("TruncateFitted(%q, %d) result has %d runes, want <= %d", tc.s, tc.max, len([]rune(got)), tc.max)
			}
		})
	}
}

func TestFirstLine(t *testing.T) {
	cases := []struct {
		name string
		s    string
		want string
	}{
		{"no newline", "single line", "single line"},
		{"first line", "first\nsecond\nthird", "first"},
		{"empty", "", ""},
		{"leading newline", "\nsecond", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FirstLine(tc.s); got != tc.want {
				t.Errorf("FirstLine(%q) = %q, want %q", tc.s, got, tc.want)
			}
		})
	}
}

func TestFirstLineOrTruncate(t *testing.T) {
	cases := []struct {
		name string
		s    string
		max  int
		want string
	}{
		{"no newline", "single line", 120, "single line"},
		{"takes first line", "first line\nsecond line", 120, "first line"},
		{"first line truncated", "a very long first line here", 10, "a very lon..."},
		{"multiline truncated rune-safe", "中文第一行\n第二行", 4, "中文第一..."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FirstLineOrTruncate(tc.s, tc.max); got != tc.want {
				t.Errorf("FirstLineOrTruncate(%q, %d) = %q, want %q", tc.s, tc.max, got, tc.want)
			}
		})
	}
}
