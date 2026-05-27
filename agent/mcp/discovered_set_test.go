package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDiscoveredSet_InsertionOrder(t *testing.T) {
	s := NewDiscoveredSet()

	s.Add("mcp__postgres__query")
	s.Add("mcp__postgres__list_tables")
	s.Add("mcp__github__create_pr")

	list := s.List()
	assert.Equal(t, []string{
		"mcp__postgres__query",
		"mcp__postgres__list_tables",
		"mcp__github__create_pr",
	}, list, "List() should return tools in discovery order")
}

func TestDiscoveredSet_IdempotentAdd(t *testing.T) {
	s := NewDiscoveredSet()

	s.Add("mcp__postgres__query")
	s.Add("mcp__github__create_pr")
	s.Add("mcp__postgres__query") // duplicate — should be ignored

	list := s.List()
	assert.Equal(t, []string{
		"mcp__postgres__query",
		"mcp__github__create_pr",
	}, list, "Duplicate Add should not add duplicate entry")
}

func TestDiscoveredSet_Contains(t *testing.T) {
	s := NewDiscoveredSet()

	assert.False(t, s.Contains("mcp__postgres__query"))

	s.Add("mcp__postgres__query")
	assert.True(t, s.Contains("mcp__postgres__query"))
	assert.False(t, s.Contains("mcp__github__create_pr"))
}

func TestDiscoveredSet_Empty(t *testing.T) {
	s := NewDiscoveredSet()

	assert.Empty(t, s.List())
	assert.Empty(t, s.List(), "List() should be deterministic for empty set")
}

func TestDiscoveredSet_OrderPreservedAcrossListCalls(t *testing.T) {
	s := NewDiscoveredSet()

	s.Add("mcp__a__tool")
	s.Add("mcp__b__tool")

	list1 := s.List()
	list2 := s.List()
	assert.Equal(t, list1, list2, "Multiple List() calls should return the same order")
}

func TestDiscoveredSet_Concurrency(t *testing.T) {
	// Basic sanity: no race condition on concurrent Add + List
	s := NewDiscoveredSet()
	done := make(chan bool)

	go func() {
		for range 100 {
			s.Add("mcp__x__tool")
		}
		done <- true
	}()

	go func() {
		for range 100 {
			_ = s.List()
		}
		done <- true
	}()

	<-done
	<-done
	// Just checking no race (go test -race) — no assertion needed
	assert.True(t, true)
}
