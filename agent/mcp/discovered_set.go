package mcp

import (
	"sync"

	"github.com/monsterxx03/tachi/pkg/set"
)

// DiscoveredSet tracks which MCP tools have been discovered by the LLM
// via the MCPSearchTools tool. Thread-safe. Preserves insertion order
// so that List() returns tools in the order they were discovered.
type DiscoveredSet struct {
	mu    sync.RWMutex
	names set.Set[string]
	order []string // insertion order
}

// NewDiscoveredSet creates an empty discovered set.
func NewDiscoveredSet() *DiscoveredSet {
	return &DiscoveredSet{names: set.New[string]()}
}

// Add marks a tool as discovered. Idempotent.
func (s *DiscoveredSet) Add(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.names.Has(name) {
		s.order = append(s.order, name)
	}
	s.names.Add(name)
}

// Contains reports whether a tool has been discovered.
func (s *DiscoveredSet) Contains(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.names.Has(name)
}

// Remove removes a single tool from the discovered set. Idempotent.
func (s *DiscoveredSet) Remove(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.names.Has(name) {
		s.names.Remove(name)
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
