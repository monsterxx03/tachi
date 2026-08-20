package mcp

import (
	"sync"

	"github.com/monsterxx03/tachi/pkg/container"
	"github.com/monsterxx03/tachi/pkg/fileutil"
)

// discoveredFileVersion is the schema version of the persisted discovered
// tools file. Bump when the on-disk format changes.
const discoveredFileVersion = 1

// discoveredFile is the JSON structure persisted to disk. It records the
// full tool names ("mcp__server__tool") the LLM has loaded via
// MCPSearchTools, so a restart can restore them without re-searching.
type discoveredFile struct {
	Version int      `json:"version"`
	Tools   []string `json:"tools"`
}

// DiscoveredSet tracks which MCP tools have been discovered by the LLM
// via the MCPSearchTools tool. Thread-safe. Preserves insertion order
// so that List() returns tools in the order they were discovered.
//
// Optional persistence: when constructed with a non-empty persistPath, every
// Add/Remove writes the current tool list to disk (atomic write), and
// LoadFromDisk reads it back. Persistence is best-effort — a failed save is
// surfaced via Save (callers may log it) and never breaks the in-memory
// state; the next mutation retries.
type DiscoveredSet struct {
	mu          sync.RWMutex
	names       container.Set[string]
	order       []string // insertion order
	persistPath string   // empty = persistence disabled
}

// NewDiscoveredSet creates an empty discovered set. A non-empty persistPath
// enables on-disk persistence: every mutation is written to path, and
// LoadFromDisk reads the previously persisted tool names.
func NewDiscoveredSet(persistPath string) *DiscoveredSet {
	return &DiscoveredSet{names: container.NewSet[string](), persistPath: persistPath}
}

// Add marks a tool as discovered. Idempotent. When persistence is enabled,
// the updated list is written to disk (best-effort; Save surfaces failures).
func (s *DiscoveredSet) Add(name string) {
	s.mu.Lock()
	if !s.names.Has(name) {
		s.order = append(s.order, name)
	}
	s.names.Add(name)
	_ = s.saveLocked()
	s.mu.Unlock()
}

// addNoPersist marks a tool as discovered without writing to disk. Used for
// batch initialization (auto-load tools in PopulateFromConnect) so that
// intermediate writes never clobber the persisted history before
// restoreDiscoveredFromDisk merges it back.
func (s *DiscoveredSet) addNoPersist(name string) {
	s.mu.Lock()
	if !s.names.Has(name) {
		s.order = append(s.order, name)
	}
	s.names.Add(name)
	s.mu.Unlock()
}

// AddAll marks multiple tools as discovered, preserving argument order.
// Idempotent per name. Persists once after all names are added, rather than
// once per name — used when restoring a batch from disk.
func (s *DiscoveredSet) AddAll(names []string) {
	if len(names) == 0 {
		return
	}
	s.mu.Lock()
	for _, name := range names {
		if !s.names.Has(name) {
			s.order = append(s.order, name)
		}
		s.names.Add(name)
	}
	_ = s.saveLocked()
	s.mu.Unlock()
}

// Contains reports whether a tool has been discovered.
func (s *DiscoveredSet) Contains(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.names.Has(name)
}

// Remove removes a single tool from the discovered set. Idempotent.
// When persistence is enabled, the updated list is written to disk.
func (s *DiscoveredSet) Remove(name string) {
	s.mu.Lock()
	s.removeLocked(name)
	_ = s.saveLocked()
	s.mu.Unlock()
}

// removeNoPersist removes a tool from the discovered set without writing to
// disk. Used during restore cleanup, where stale names are dropped from the
// in-memory set first and persisted once afterwards.
func (s *DiscoveredSet) removeNoPersist(name string) {
	s.mu.Lock()
	s.removeLocked(name)
	s.mu.Unlock()
}

// removeLocked removes name from the set and order slice. Caller must hold
// the lock. No-op when the name is absent.
func (s *DiscoveredSet) removeLocked(name string) {
	if !s.names.Has(name) {
		return
	}
	s.names.Remove(name)
	for i, n := range s.order {
		if n == name {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// List returns a copy of all discovered tool names in discovery order.
func (s *DiscoveredSet) List() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]string, len(s.order))
	copy(result, s.order)
	return result
}

// Len returns the number of discovered tools.
func (s *DiscoveredSet) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.names.Len()
}

// Save writes the current tool list to the persistence path. Returns nil
// when persistence is disabled (nothing to do). Callers (e.g. the manager)
// should log the error; the in-memory state stays authoritative.
func (s *DiscoveredSet) Save() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.saveLocked()
}

// saveLocked writes the current state to persistPath. Caller must hold the
// lock. A nil persistPath is a no-op; a failed write is silently dropped
// here and surfaced via Save for callers that want to log it.
func (s *DiscoveredSet) saveLocked() error {
	if s.persistPath == "" {
		return nil
	}
	data, err := fileutil.MarshalJSON(discoveredFile{
		Version: discoveredFileVersion,
		Tools:   s.order,
	})
	if err != nil {
		return err
	}
	return fileutil.AtomicWriteFilePrivate(s.persistPath, data)
}

// LoadFromDisk reads the persisted tool names from the persistence path
// WITHOUT modifying the in-memory set. Returns nil when persistence is
// disabled (nothing to read). A missing file returns an os.ErrNotExist error
// (fresh install); a corrupt file returns a parse error — callers should log
// and continue with an empty list.
//
// Callers are expected to filter the returned names (e.g. against the
// deferred pool) before adding them back via AddAll.
func (s *DiscoveredSet) LoadFromDisk() ([]string, error) {
	s.mu.RLock()
	path := s.persistPath
	s.mu.RUnlock()

	if path == "" {
		return nil, nil
	}

	var f discoveredFile
	if err := fileutil.ReadJSON(path, &f); err != nil {
		return nil, err
	}
	if f.Version > discoveredFileVersion {
		// Unknown newer schema — ignore rather than risk misreading it.
		return nil, nil
	}
	return f.Tools, nil
}
