package lockedmap

import (
	"sync"
	"testing"
)

func TestMap_StoreLoad(t *testing.T) {
	var m Map[string, int]

	// Load from empty map.
	v, ok := m.Load("a")
	if ok || v != 0 {
		t.Fatalf("empty Load: got (%v, %v), want (0, false)", v, ok)
	}

	m.Store("a", 42)
	v, ok = m.Load("a")
	if !ok || v != 42 {
		t.Fatalf("Store+Load: got (%v, %v), want (42, true)", v, ok)
	}

	// Overwrite.
	m.Store("a", 100)
	v, ok = m.Load("a")
	if !ok || v != 100 {
		t.Fatalf("overwrite Load: got (%v, %v), want (100, true)", v, ok)
	}
}

func TestMap_Delete(t *testing.T) {
	var m Map[string, int]

	// Delete on empty map (should not panic).
	m.Delete("nonexistent")

	m.Store("a", 1)
	v, ok := m.Load("a")
	if !ok || v != 1 {
		t.Fatalf("before delete: got (%v, %v), want (1, true)", v, ok)
	}

	m.Delete("a")
	v, ok = m.Load("a")
	if ok || v != 0 {
		t.Fatalf("after delete: got (%v, %v), want (0, false)", v, ok)
	}
}

func TestMap_LoadOrCompute(t *testing.T) {
	var m Map[string, int]

	// Compute on empty map.
	v, existed := m.LoadOrCompute("a", func() int { return 10 })
	if existed || v != 10 {
		t.Fatalf("first compute: got (%v, %v), want (10, false)", v, existed)
	}
	v, _ = m.Load("a")
	if v != 10 {
		t.Fatalf("first compute + Load: got %v, want 10", v)
	}

	// Existing key — compute must NOT be called.
	called := false
	v, existed = m.LoadOrCompute("a", func() int {
		called = true
		return 99
	})
	if !existed || v != 10 {
		t.Fatalf("second compute: got (%v, %v), want (10, true)", v, existed)
	}
	if called {
		t.Fatal("compute was called for existing key")
	}
}

func TestMap_LoadAndDelete(t *testing.T) {
	var m Map[string, int]

	// From empty.
	v, ok := m.LoadAndDelete("a")
	if ok || v != 0 {
		t.Fatalf("empty LoadAndDelete: got (%v, %v), want (0, false)", v, ok)
	}

	m.Store("a", 42)
	v, ok = m.LoadAndDelete("a")
	if !ok || v != 42 {
		t.Fatalf("LoadAndDelete: got (%v, %v), want (42, true)", v, ok)
	}

	// Verify it's gone.
	v, ok = m.Load("a")
	if ok || v != 0 {
		t.Fatalf("after LoadAndDelete: got (%v, %v), want (0, false)", v, ok)
	}
}

func TestMap_CompareAndDelete(t *testing.T) {
	var m Map[string, int]

	// On empty map.
	if m.CompareAndDelete("a", 1) {
		t.Fatal("CompareAndDelete on empty should be false")
	}

	m.Store("a", 42)

	// Wrong value — should not delete.
	if m.CompareAndDelete("a", 0) {
		t.Fatal("CompareAndDelete with wrong value should be false")
	}
	v, ok := m.Load("a")
	if !ok || v != 42 {
		t.Fatalf("value changed after failed CompareAndDelete: (%v, %v)", v, ok)
	}

	// Correct value — should delete.
	if !m.CompareAndDelete("a", 42) {
		t.Fatal("CompareAndDelete with correct value should be true")
	}
	_, ok = m.Load("a")
	if ok {
		t.Fatal("key should be deleted after successful CompareAndDelete")
	}
}

func TestMap_CompareAndDelete_Pointer(t *testing.T) {
	// Verify pointer identity comparison (used by threadActivations).
	type obj struct{ n int }
	var m Map[string, *obj]

	a := &obj{n: 1}
	b := &obj{n: 1} // same value, different pointer

	m.Store("x", a)

	// Different pointer with same fields — should NOT delete.
	if m.CompareAndDelete("x", b) {
		t.Fatal("CompareAndDelete with different pointer should be false")
	}
	if _, ok := m.Load("x"); !ok {
		t.Fatal("key should still exist")
	}

	// Same pointer — should delete.
	if !m.CompareAndDelete("x", a) {
		t.Fatal("CompareAndDelete with same pointer should be true")
	}
	if _, ok := m.Load("x"); ok {
		t.Fatal("key should be deleted")
	}
}

func TestMap_ZeroValueUsable(t *testing.T) {
	// The zero value of Map must be usable without explicit initialization.
	var m Map[string, bool]

	// Store (lazy init).
	m.Store("a", true)
	v, ok := m.Load("a")
	if !ok || !v {
		t.Fatalf("zero value Store+Load: got (%v, %v)", v, ok)
	}

	var m2 Map[int, string]
	m2.Store(1, "hello")
	s, ok := m2.Load(1)
	if !ok || s != "hello" {
		t.Fatalf("zero value string map: got (%q, %v)", s, ok)
	}
}

func TestMap_Concurrent(t *testing.T) {
	// Basic race test: N goroutines Store + Load + Delete concurrently.
	var m Map[int, int]
	const (
		numGoroutines = 20
		opsPerRoutine = 200
	)

	var wg sync.WaitGroup
	wg.Add(numGoroutines)
	for g := range numGoroutines {
		go func(base int) {
			defer wg.Done()
			for i := range opsPerRoutine {
				key := base*opsPerRoutine + i
				m.Store(key, key*10)
			}
			for i := range opsPerRoutine {
				key := base*opsPerRoutine + i
				m.Load(key)
			}
			for i := range opsPerRoutine {
				key := base*opsPerRoutine + i
				m.Delete(key)
			}
		}(g)
	}
	wg.Wait()

	// After all deletes, Load for the last key written by each goroutine
	// should return not-found. But since we deleted all keys, none should
	// exist in normal circumstances.
	// We only check that Load doesn't panic.
	_, _ = m.Load(0)
}

func TestMap_LoadOrCompute_ConcurrentRace(t *testing.T) {
	// Verify that concurrent LoadOrCompute calls for the same key produce
	// exactly one value from compute, and all callers get the same result.
	var m Map[int, int]
	const numGoroutines = 50

	var wg sync.WaitGroup
	results := make([]int, numGoroutines)
	wg.Add(numGoroutines)
	for g := range numGoroutines {
		go func(idx int) {
			defer wg.Done()
			v, _ := m.LoadOrCompute(1, func() int { return idx })
			results[idx] = v
		}(g)
	}
	wg.Wait()

	// All results must be the same value.
	first := results[0]
	for i := 1; i < numGoroutines; i++ {
		if results[i] != first {
			t.Fatalf("inconsistent LoadOrCompute results: %d != %d at index %d", first, results[i], i)
		}
	}
}
