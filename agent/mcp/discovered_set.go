package mcp

import "sync"

// DiscoveredSet tracks which MCP tools have been discovered by the LLM
// via the MCPSearchTools tool. Thread-safe.
type DiscoveredSet struct {
	mu    sync.RWMutex
	names map[string]bool
}

// NewDiscoveredSet creates an empty discovered set.
func NewDiscoveredSet() *DiscoveredSet {
	return &DiscoveredSet{names: make(map[string]bool)}
}

// Add marks a tool as discovered. Idempotent.
func (s *DiscoveredSet) Add(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.names[name] = true
}

// Contains reports whether a tool has been discovered.
func (s *DiscoveredSet) Contains(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.names[name]
}

// List returns a copy of all discovered tool names.
func (s *DiscoveredSet) List() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]string, 0, len(s.names))
	for name := range s.names {
		result = append(result, name)
	}
	return result
}
