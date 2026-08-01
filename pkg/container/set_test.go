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
