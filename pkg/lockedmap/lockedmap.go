// Package lockedmap provides a generic, mutex-protected map for type-safe
// concurrent access. It replaces the common "sync.Mutex + map[K]V" pattern
// with a single type that encapsulates the lock, eliminating lock/unlock
// boilerplate at call sites while retaining compile-time type safety.
//
// Prefer lockedmap.Map over sync.Map when:
//   - You want type safety (no interface{} / type assertions)
//   - Keys are frequently added and removed (not write-once-read-many)
//   - You need CompareAndDelete with pointer/reference identity semantics
package lockedmap

import "sync"

// Map is a generic concurrent map protected by a mutex. The zero value is
// ready to use; the internal map is lazily initialized on first write.
type Map[K comparable, V any] struct {
	mu sync.Mutex
	m  map[K]V
}

// Load returns the value stored for key, and whether it was present.
func (lm *Map[K, V]) Load(key K) (V, bool) {
	lm.mu.Lock()
	v, ok := lm.m[key]
	lm.mu.Unlock()
	return v, ok
}

// Store sets the value for key.
func (lm *Map[K, V]) Store(key K, val V) {
	lm.mu.Lock()
	if lm.m == nil {
		lm.m = make(map[K]V)
	}
	lm.m[key] = val
	lm.mu.Unlock()
}

// Delete removes the entry for key. Safe to call on a zero-value Map.
func (lm *Map[K, V]) Delete(key K) {
	lm.mu.Lock()
	delete(lm.m, key)
	lm.mu.Unlock()
}

// LoadOrCompute returns the existing value for key if present; otherwise it
// calls compute (under the lock) and stores+returns the result. The second
// return value is true when the key already existed.
func (lm *Map[K, V]) LoadOrCompute(key K, compute func() V) (V, bool) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	if lm.m == nil {
		lm.m = make(map[K]V)
	}

	v, ok := lm.m[key]
	if ok {
		return v, true
	}
	v = compute()
	lm.m[key] = v
	return v, false
}

// LoadAndDelete atomically loads and deletes the entry for key. Returns the
// value and whether it was present.
func (lm *Map[K, V]) LoadAndDelete(key K) (V, bool) {
	lm.mu.Lock()
	v, ok := lm.m[key]
	if ok {
		delete(lm.m, key)
	}
	lm.mu.Unlock()
	return v, ok
}

// CompareAndDelete deletes the entry for key only if its current value
// equals old (using == comparison). Returns whether the deletion occurred.
func (lm *Map[K, V]) CompareAndDelete(key K, old V) bool {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	v, ok := lm.m[key]
	if !ok {
		return false
	}
	// Use interface coercion for == comparison, which works for both
	// comparable values and pointer types (pointer identity).
	if any(v) != any(old) {
		return false
	}
	delete(lm.m, key)
	return true
}
