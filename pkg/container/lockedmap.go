// LockedMap is a generic, mutex-protected map for type-safe concurrent
// access. It replaces the common "sync.Mutex + map[K]V" pattern with a
// single type that encapsulates the lock, eliminating lock/unlock
// boilerplate at call sites while retaining compile-time type safety.
//
// Reads use a read lock (Load/Range/Len), so read-heavy workloads (e.g.
// search hot paths) benefit from concurrent readers.
//
// Prefer LockedMap over sync.Map when:
//   - You want type safety (no interface{} / type assertions)
//   - Keys are frequently added and removed (not write-once-read-many)
//   - You need CompareAndDelete with pointer/reference identity semantics
package container

import "sync"

// LockedMap is a generic concurrent map protected by a read-write mutex.
// The zero value is ready to use; the internal map is lazily initialized on
// first write.
type LockedMap[K comparable, V any] struct {
	mu sync.RWMutex
	m  map[K]V
}

// Load returns the value stored for key, and whether it was present.
func (lm *LockedMap[K, V]) Load(key K) (V, bool) {
	lm.mu.RLock()
	v, ok := lm.m[key]
	lm.mu.RUnlock()
	return v, ok
}

// Len returns the number of entries in the map.
func (lm *LockedMap[K, V]) Len() int {
	lm.mu.RLock()
	n := len(lm.m)
	lm.mu.RUnlock()
	return n
}

// Range calls f for each key/value pair in the map until f returns false.
// The order is unspecified. The map must not be mutated from within f
// (deadlock — the read lock is held).
func (lm *LockedMap[K, V]) Range(f func(key K, val V) bool) {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	for k, v := range lm.m {
		if !f(k, v) {
			break
		}
	}
}

// Store sets the value for key.
func (lm *LockedMap[K, V]) Store(key K, val V) {
	lm.mu.Lock()
	if lm.m == nil {
		lm.m = make(map[K]V)
	}
	lm.m[key] = val
	lm.mu.Unlock()
}

// Delete removes the entry for key. Safe to call on a zero-value Map.
func (lm *LockedMap[K, V]) Delete(key K) {
	lm.mu.Lock()
	delete(lm.m, key)
	lm.mu.Unlock()
}

// LoadOrCompute returns the existing value for key if present; otherwise it
// calls compute (under the lock) and stores+returns the result. The second
// return value is true when the key already existed.
func (lm *LockedMap[K, V]) LoadOrCompute(key K, compute func() V) (V, bool) {
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
func (lm *LockedMap[K, V]) LoadAndDelete(key K) (V, bool) {
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
func (lm *LockedMap[K, V]) CompareAndDelete(key K, old V) bool {
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
