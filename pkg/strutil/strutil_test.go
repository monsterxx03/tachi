package strutil

import (
	"strings"
	"testing"
)

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

func TestIsCJK(t *testing.T) {
	cases := []struct {
		r    rune
		want bool
	}{
		{'中', true},   // CJK Unified Ideographs
		{'文', true},   // CJK Unified Ideographs
		{'㐀', true},   // Extension A (0x3400)
		{'あ', true},   // Hiragana
		{'カ', true},   // Katakana
		{'한', true},   // Hangul Syllables
		{'豈', true},   // Compatibility Ideographs (0xF900)
		{'𠀀', true},   // Extension B (0x20000)
		{'⺀', true},   // Radicals Supplement (0x2E80)
		{'a', false},  // ASCII
		{'1', false},  // digit
		{'\n', false}, // control
		{'é', false},  // Latin-1
		{'😀', false},  // emoji
	}
	for _, tc := range cases {
		if got := IsCJK(tc.r); got != tc.want {
			t.Errorf("IsCJK(%q) = %v, want %v", tc.r, got, tc.want)
		}
	}
}

func TestShortUUID(t *testing.T) {
	for _, n := range []int{4, 8, 12} {
		got := ShortUUID(n)
		if len(got) != n {
			t.Errorf("ShortUUID(%d) len = %d, want %d", n, len(got), n)
		}
		for _, c := range got {
			if !strings.ContainsRune("0123456789abcdef", c) {
				t.Errorf("ShortUUID(%d) = %q has non-hex char %q", n, got, c)
			}
		}
	}
	a, b := ShortUUID(8), ShortUUID(8)
	if a == b {
		t.Errorf("ShortUUID collision: %q", a)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1024 * 1024, "1.0 MB"},
		{3 * 1024 * 1024, "3.0 MB"},
		{1024 * 1024 * 1024, "1.0 GB"},
		{2 * 1024 * 1024 * 1024, "2.0 GB"},
	}
	for _, tc := range cases {
		if got := HumanBytes(tc.n); got != tc.want {
			t.Errorf("HumanBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"", 0, ""},
		{"simple.txt", 0, "simple.txt"},
		{"a/b:c*d?e", 0, "a_b_c_d_e"},
		{"\"<>| ", 0, "_____"},
		{"hello world 你好", 0, "hello_world_你好"},
		{"abcdefghij", 5, "abcde"},
	}
	for _, tc := range cases {
		if got := SanitizeFilename(tc.in, tc.max); got != tc.want {
			t.Errorf("SanitizeFilename(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
		}
	}
}

func TestFormatMagnitude(t *testing.T) {
	cases := []struct {
		in   int64
		div  int64
		unit string
		want string
	}{
		{1_000, 1_000, "k", "1k"},
		{7_300, 1_000, "k", "7.3k"},
		{9_999, 1_000, "k", "10k"},
		{12_000, 1_000, "k", "12k"},
		{99_999, 1_000, "k", "100k"},
		{150_000, 1_000, "k", "150k"},
		{1_234_567, 1_000_000, "m", "1.2m"},
	}
	for _, c := range cases {
		if got := FormatMagnitude(c.in, c.div, c.unit); got != c.want {
			t.Errorf("FormatMagnitude(%d, %d, %q) = %q, want %q", c.in, c.div, c.unit, got, c.want)
		}
	}
}

func TestFormatTPS(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{85, "85/s"},
		{999, "999/s"},
		{1_000, "1k/s"},
		{7_300, "7.3k/s"},
		{9_999, "10k/s"},
		{12_000, "12k/s"},
		{150_000, "150k/s"},
		{1_234_567, "1.2m/s"},
	}
	for _, c := range cases {
		if got := FormatTPS(c.in); got != c.want {
			t.Errorf("FormatTPS(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSplitBy(t *testing.T) {
	cases := []struct {
		name string
		in   string
		sep  string
		want []string
	}{
		// Comma-separated lists
		{"empty list", "", ",", nil},
		{"whitespace only list", "  , , ", ",", nil},
		{"single item", "foo", ",", []string{"foo"}},
		{"basic list", "a,b,c", ",", []string{"a", "b", "c"}},
		{"trimmed list", " a , b , c ", ",", []string{"a", "b", "c"}},
		{"empty entries dropped", "a,,b, ,c", ",", []string{"a", "b", "c"}},
		{"unicode preserved", "中文, english", ",", []string{"中文", "english"}},
		// Newline-separated lines
		{"empty lines", "", "\n", nil},
		{"whitespace only lines", " \n\n  ", "\n", nil},
		{"single line", "foo", "\n", []string{"foo"}},
		{"basic lines", "a\nb\nc", "\n", []string{"a", "b", "c"}},
		{"blank lines dropped", "a\n\nb\n\nc", "\n", []string{"a", "b", "c"}},
		{"trimmed lines", " a \n\tb\n c ", "\n", []string{"a", "b", "c"}},
		{"leading trailing newlines", "\na\nb\n", "\n", []string{"a", "b"}},
		{"crlf", "a\r\nb\r\nc", "\n", []string{"a", "b", "c"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SplitBy(c.in, c.sep)
			if len(got) != len(c.want) {
				t.Fatalf("SplitBy(%q, %q) = %#v, want %#v", c.in, c.sep, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("SplitBy(%q, %q) = %#v, want %#v", c.in, c.sep, got, c.want)
				}
			}
		})
	}
}
