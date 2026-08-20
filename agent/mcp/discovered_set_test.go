package mcp

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoveredSet_InsertionOrder(t *testing.T) {
	s := NewDiscoveredSet("")

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
	s := NewDiscoveredSet("")

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
	s := NewDiscoveredSet("")

	assert.False(t, s.Contains("mcp__postgres__query"))

	s.Add("mcp__postgres__query")
	assert.True(t, s.Contains("mcp__postgres__query"))
	assert.False(t, s.Contains("mcp__github__create_pr"))
}

func TestDiscoveredSet_Empty(t *testing.T) {
	s := NewDiscoveredSet("")

	assert.Empty(t, s.List())
	assert.Empty(t, s.List(), "List() should be deterministic for empty set")
}

func TestDiscoveredSet_OrderPreservedAcrossListCalls(t *testing.T) {
	s := NewDiscoveredSet("")

	s.Add("mcp__a__tool")
	s.Add("mcp__b__tool")

	list1 := s.List()
	list2 := s.List()
	assert.Equal(t, list1, list2, "Multiple List() calls should return the same order")
}

func TestDiscoveredSet_Concurrency(t *testing.T) {
	// Basic sanity: no race condition on concurrent Add + List
	s := NewDiscoveredSet("")
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

func TestDiscoveredSet_PersistenceRoundTrip(t *testing.T) {
	// Simulates a process restart: set A discovers tools (persisted on every
	// Add), then a fresh set B on the same path sees them via LoadFromDisk.
	path := filepath.Join(t.TempDir(), "mcp_discovered.json")

	a := NewDiscoveredSet(path)
	a.Add("mcp__postgres__query")
	a.Add("mcp__postgres__list_tables")
	a.Add("mcp__github__create_pr")

	b := NewDiscoveredSet(path)
	loaded, err := b.LoadFromDisk()
	require.NoError(t, err)
	assert.Equal(t, []string{
		"mcp__postgres__query",
		"mcp__postgres__list_tables",
		"mcp__github__create_pr",
	}, loaded, "LoadFromDisk should return tools in discovery order")
}

func TestDiscoveredSet_RemovePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp_discovered.json")

	s := NewDiscoveredSet(path)
	s.Add("mcp__postgres__query")
	s.Add("mcp__postgres__list_tables")
	s.Remove("mcp__postgres__query") // must be reflected on disk

	loaded, err := s.LoadFromDisk()
	require.NoError(t, err)
	assert.Equal(t, []string{"mcp__postgres__list_tables"}, loaded)
}

func TestDiscoveredSet_AddAll(t *testing.T) {
	s := NewDiscoveredSet("")
	s.AddAll([]string{"mcp__a__t1", "mcp__a__t2", "mcp__a__t1"}) // duplicate dropped

	assert.Equal(t, []string{"mcp__a__t1", "mcp__a__t2"}, s.List())
	assert.Equal(t, 2, s.Len())
}

func TestDiscoveredSet_LoadFromDisk_MissingFile(t *testing.T) {
	s := NewDiscoveredSet(filepath.Join(t.TempDir(), "mcp_discovered.json"))

	_, err := s.LoadFromDisk()
	assert.True(t, errors.Is(err, os.ErrNotExist), "missing file should surface os.ErrNotExist, got %v", err)
}

func TestDiscoveredSet_LoadFromDisk_CorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp_discovered.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))

	s := NewDiscoveredSet(path)

	_, err := s.LoadFromDisk()
	assert.Error(t, err, "corrupt file should return an error")
}

func TestDiscoveredSet_LoadFromDisk_PersistenceDisabled(t *testing.T) {
	s := NewDiscoveredSet("") // persistence disabled

	loaded, err := s.LoadFromDisk()
	assert.NoError(t, err)
	assert.Nil(t, loaded)
}

func TestDiscoveredSet_SaveWithoutPersistence(t *testing.T) {
	s := NewDiscoveredSet("")
	s.Add("mcp__a__t1")
	assert.NoError(t, s.Save(), "Save with persistence disabled is a no-op")
}
