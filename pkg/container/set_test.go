package container

import (
	"sort"
	"testing"
)

func TestNewAndHas(t *testing.T) {
	s := NewSet("a", "b", "c")
	if s.Len() != 3 {
		t.Fatalf("Len = %d, want 3", s.Len())
	}
	for _, v := range []string{"a", "b", "c"} {
		if !s.Has(v) {
			t.Errorf("Has(%q) = false, want true", v)
		}
	}
	if s.Has("d") || s.Contains("") {
		t.Error("Has returned true for missing element")
	}
}

func TestAddRemove(t *testing.T) {
	s := NewSet[int]()
	s.Add(1, 2, 3)
	if !s.Has(2) {
		t.Error("Add failed")
	}
	s.Remove(2)
	if s.Has(2) {
		t.Error("Remove failed")
	}
	s.Remove(99) // no-op
	if s.Len() != 2 {
		t.Errorf("Len = %d, want 2", s.Len())
	}
}

func TestSliceAndRange(t *testing.T) {
	s := NewSet("x", "y", "z")
	got := s.Slice()
	if len(got) != 3 {
		t.Fatalf("Slice len = %d, want 3", len(got))
	}
	sort.Strings(got)
	if got[0] != "x" || got[2] != "z" {
		t.Errorf("Slice = %v", got)
	}

	var ranged []string
	s.Range(func(v string) { ranged = append(ranged, v) })
	if len(ranged) != 3 {
		t.Errorf("Range count = %d, want 3", len(ranged))
	}
}

func TestGenericTypes(t *testing.T) {
	ints := NewSet(1, 2, 3)
	if !ints.Has(2) || ints.Has(4) {
		t.Error("int set mismatch")
	}
	runes := NewSet('a', 'b')
	if !runes.Has('a') {
		t.Error("rune set mismatch")
	}
}

func TestDedupe(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil", nil, nil},
		{"empty", []string{}, []string{}},
		{"single", []string{"a"}, []string{"a"}},
		{"no dupes", []string{"a", "b", "c"}, []string{"a", "b", "c"}},
		{"adjacent dupes", []string{"a", "a", "b"}, []string{"a", "b"}},
		{"scattered dupes", []string{"a", "b", "a", "c", "b"}, []string{"a", "b", "c"}},
		{"all same", []string{"x", "x", "x"}, []string{"x"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Dedupe(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("Dedupe(%v) = %v, want %v", c.in, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("Dedupe(%v) = %v, want %v", c.in, got, c.want)
				}
			}
		})
	}

	// Generic with non-string comparable type.
	ints := Dedupe([]int{1, 2, 1, 3, 2})
	if len(ints) != 3 || ints[0] != 1 || ints[2] != 3 {
		t.Errorf("Dedupe ints = %v", ints)
	}
}
