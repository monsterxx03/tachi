package mcp

import "sync"

// DiscoveredSet tracks which MCP tools have been discovered by the LLM
// via the MCPSearchTools tool. Thread-safe. Preserves insertion order
// so that List() returns tools in the order they were discovered.
type DiscoveredSet struct {
	mu    sync.RWMutex
	names map[string]bool
	order []string // insertion order
}

// NewDiscoveredSet creates an empty discovered set.
func NewDiscoveredSet() *DiscoveredSet {
	return &DiscoveredSet{names: make(map[string]bool)}
}

// Add marks a tool as discovered. Idempotent.
func (s *DiscoveredSet) Add(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.names[name] {
		s.order = append(s.order, name)
	}
	s.names[name] = true
}

// Contains reports whether a tool has been discovered.
func (s *DiscoveredSet) Contains(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.names[name]
}

// Remove removes a single tool from the discovered set. Idempotent.
func (s *DiscoveredSet) Remove(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.names[name] {
		delete(s.names, name)
		// Remove from order slice
		for i, n := range s.order {
			if n == name {
				s.order = append(s.order[:i], s.order[i+1:]...)
				break
			}
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
