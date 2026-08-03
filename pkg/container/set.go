package container

// Set is a generic set of comparable values, implemented as a map with empty
// struct values (zero memory per element).
type Set[T comparable] map[T]struct{}

// NewSet returns a set containing the given items (may be empty).
func NewSet[T comparable](items ...T) Set[T] {
	s := make(Set[T], len(items))
	s.Add(items...)
	return s
}

// Add inserts the given items into the set.
func (s Set[T]) Add(items ...T) {
	for _, it := range items {
		s[it] = struct{}{}
	}
}

// Has reports whether v is in the set.
func (s Set[T]) Has(v T) bool {
	_, ok := s[v]
	return ok
}

// Contains is an alias of Has.
func (s Set[T]) Contains(v T) bool {
	return s.Has(v)
}

// Remove deletes v from the set (no-op if absent).
func (s Set[T]) Remove(v T) {
	delete(s, v)
}

// Len returns the number of elements in the set.
func (s Set[T]) Len() int {
	return len(s)
}

// Slice returns the elements as a slice in unspecified order.
func (s Set[T]) Slice() []T {
	out := make([]T, 0, len(s))
	for v := range s {
		out = append(out, v)
	}
	return out
}

// Range calls fn for each element. Iteration order is unspecified.
func (s Set[T]) Range(fn func(v T)) {
	for v := range s {
		fn(v)
	}
}

// Dedupe returns items with duplicates removed, preserving the order of first
// occurrences. Later duplicates are dropped, so ["a","b","a","c"] → ["a","b","c"].
// Items of fewer than two elements are returned unchanged.
func Dedupe[T comparable](items []T) []T {
	if len(items) < 2 {
		return items
	}
	seen := make(Set[T], len(items))
	out := make([]T, 0, len(items))
	for _, it := range items {
		if seen.Has(it) {
			continue
		}
		seen.Add(it)
		out = append(out, it)
	}
	return out
}
