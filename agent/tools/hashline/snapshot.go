package hashline

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// SnapshotEntry stores a file snapshot with its hash tag and full SHA-256 hash.
type SnapshotEntry struct {
	Tag       string // 4-hex short tag (e.g. "a1f0"), for LLM communication
	FullHash  string // full SHA-256 hex string, real version credential
	Timestamp time.Time
}

// SnapshotStore maintains file version history using a two-layer hash design:
//
//	First layer: 4-hex short tag (e.g. "a1f0") — LLM-visible friendly label.
//	Second layer: Full SHA-256 (64 hex chars) — real version credential.
//
// Internal structure: path → { tag → SnapshotEntry }
// This allows tracking multiple versions of the same file by their short tags.
type SnapshotStore struct {
	mu        sync.RWMutex
	snapshots map[string]map[string]SnapshotEntry // path → tag → entry
	order     map[string][]string                 // path → ordered list of tags (newest last)
}

// NewSnapshotStore creates a new SnapshotStore.
func NewSnapshotStore() *SnapshotStore {
	return &SnapshotStore{
		snapshots: make(map[string]map[string]SnapshotEntry),
		order:     make(map[string][]string),
	}
}

// Record stores a snapshot of the file content and returns its 4-hex tag.
// If the exact same content was already recorded for this path, returns
// the existing tag. Otherwise computes a new tag and stores the entry.
func (s *SnapshotStore) Record(path, content string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Compute SHA-256 once and derive both full hash and short tag from it.
	h := sha256.Sum256([]byte(content))
	fullHash := hex.EncodeToString(h[:])

	// Check if this exact content was already recorded under any tag
	if entries, ok := s.snapshots[path]; ok {
		for _, entry := range entries {
			if entry.FullHash == fullHash {
				return entry.Tag
			}
		}
	}
	// Compute tag, resolving collisions
	tag := resolveTag(s.snapshots[path], h, fullHash)

	if s.snapshots[path] == nil {
		s.snapshots[path] = make(map[string]SnapshotEntry)
	}

	s.snapshots[path][tag] = SnapshotEntry{
		Tag:       tag,
		FullHash:  fullHash,
		Timestamp: time.Now(),
	}
	s.order[path] = append(s.order[path], tag)

	// Cap history to prevent unbounded growth
	const maxHistory = 20
	if len(s.order[path]) > maxHistory {
		prune := len(s.order[path]) - maxHistory
		for i := range prune {
			delete(s.snapshots[path], s.order[path][i])
		}
		s.order[path] = s.order[path][prune:]
	}

	return tag
}

// Verify checks that the current file content matches the snapshot identified
// by the given tag. Uses FullHash comparison for collision-free verification.
// currentContent is the actual file content read from disk.
func (s *SnapshotStore) Verify(path, tag, currentContent string) error {
	s.mu.RLock()
	entries, pathOk := s.snapshots[path]
	entry, tagOk := entries[tag]
	s.mu.RUnlock()

	if !pathOk || !tagOk {
		return ErrSnapshotRequired(path)
	}

	currentHash := computeFullHash(currentContent)
	if currentHash != entry.FullHash {
		currentTag := ComputeTag(currentContent)
		return ErrTagMismatch(path, tag, currentTag)
	}
	return nil
}

// Invalidate removes all snapshots for the given path, forcing a fresh
// Record on the next read.
func (s *SnapshotStore) Invalidate(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.snapshots, path)
	delete(s.order, path)
}

// InvalidateTag removes a specific tag version for the given path.
func (s *SnapshotStore) InvalidateTag(path, tag string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entries, ok := s.snapshots[path]; ok {
		delete(entries, tag)
		// Clean up from order list
		if order, ok := s.order[path]; ok {
			for i, t := range order {
				if t == tag {
					s.order[path] = append(order[:i], order[i+1:]...)
					break
				}
			}
		}
	}
}

// GetTag returns the latest tag for path, or empty string if not recorded.
func (s *SnapshotStore) GetTag(path string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	order := s.order[path]
	if len(order) == 0 {
		return ""
	}
	return order[len(order)-1]
}

// GetEntry returns the snapshot entry for a specific path and tag.
// Returns nil if not found.
func (s *SnapshotStore) GetEntry(path, tag string) *SnapshotEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if entries, ok := s.snapshots[path]; ok {
		if entry, ok := entries[tag]; ok {
			return &entry
		}
	}
	return nil
}

// computeFullHash returns the full SHA-256 hex string.
func computeFullHash(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

// resolveTag computes a unique tag for the given content hash, handling collisions
// by expanding to more hex characters if the tag already exists for a different
// content version.
// h is the SHA-256 hash of the content, fullHash is its hex encoding.
func resolveTag(entries map[string]SnapshotEntry, h [32]byte, fullHash string) string {
	tag := hex.EncodeToString(h[:2])
	if entries == nil {
		return tag
	}

	for bytes := 2; bytes <= 32; bytes++ {
		if existing, exists := entries[tag]; !exists || existing.FullHash == fullHash {
			return tag
		}
		// Collision: expand tag by one more byte (2 hex chars)
		tag = hex.EncodeToString(h[:bytes+1])
	}

	// Extremely unlikely: all 32-byte tags collide. Use full SHA-256 hex.
	return fullHash
}
