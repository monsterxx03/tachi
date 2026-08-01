package dream

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent/memory"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/pkg/set"
)

// HalfLifeDays is the decay half-life in days. Facts decay to 0.5 after this
// many days without reinforcement, to 0.25 after 2×, etc.
const HalfLifeDays = 7

// CalculateDecay computes the decay factor based on time elapsed since the
// last reinforcement, falling back to the fact's creation time when it has
// never been reinforced. The fallback matters: without it, facts that were
// never recalled would stay at full strength forever — the decay system
// would only apply to facts recalled at least once.
//
//	decay = exp(-ln(2) × elapsed / halfLife)
//
// Returns 1.0 if both timestamps are zero (brand new, not yet decayed).
func CalculateDecay(lastReinforced, createdAt time.Time) float64 {
	ref := lastReinforced
	if ref.IsZero() {
		ref = createdAt
	}
	if ref.IsZero() {
		return 1.0
	}
	elapsed := time.Since(ref)
	if elapsed < 0 {
		return 1.0 // clock skew — treat as brand new
	}
	halfLife := time.Duration(HalfLifeDays) * 24 * time.Hour
	return math.Exp(-math.Ln2 * elapsed.Seconds() / halfLife.Seconds())
}

// ScanTopicFacts scans all .md files in memoryRoot/topics/, extracts fact
// blocks (separated by ---), and merges their state with existingStates.
//
//   - New facts (not in existingStates) are initialized with decay=1.0.
//   - Existing facts retain their reinforcement count but have superseded
//     status updated from the block content and decay recalculated.
//   - Facts in existingStates not found in topic files are excluded
//     (they were removed/consolidated by the dream agent).
func ScanTopicFacts(memoryRoot string, existingStates map[string]*memory.FactState, logger *logger.Logger) map[string]*memory.FactState {
	result := make(map[string]*memory.FactState)
	now := time.Now()

	topicsDir := filepath.Join(memoryRoot, "topics")
	entries, err := os.ReadDir(topicsDir)
	if err != nil {
		if logger != nil {
			logger.Error(context.Background(), "ScanTopicFacts: read topics dir failed", err)
		}
		return result
	}

	seen := set.New[string]()

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		topicFile := entry.Name()
		path := filepath.Join(topicsDir, topicFile)

		content, err := os.ReadFile(path)
		if err != nil {
			if logger != nil {
				logger.Error(context.Background(), "ScanTopicFacts: read file failed", err, "path", path)
			}
			continue
		}

		blocks := memory.SplitByHR(string(content))
		for _, block := range blocks {
			block = strings.TrimSpace(block)
			if block == "" {
				continue
			}

			id := memory.FactID(topicFile, block)
			seen.Add(id)

			superseded := strings.Contains(strings.ToLower(block), "状态: superseded") ||
				strings.Contains(strings.ToLower(block), "status: superseded")

			if existing, ok := existingStates[id]; ok {
				// Clone to avoid mutating the caller's map.
				cloned := *existing
				cloned.Superseded = superseded
				cloned.Decay = CalculateDecay(cloned.LastReinforced, cloned.CreatedAt)
				result[id] = &cloned
			} else {
				// New fact: initialize with full strength.
				result[id] = &memory.FactState{
					ID:             id,
					TopicFile:      topicFile,
					Decay:          1.0,
					Reinforcements: 0,
					LastReinforced: time.Time{}, // zero → not yet reinforced
					CreatedAt:      now,
					Superseded:     superseded,
				}
			}
		}
	}

	// Log stats.
	if logger != nil {
		newCount := 0
		removedCount := 0
		for id := range result {
			if _, existed := existingStates[id]; !existed {
				newCount++
			}
		}
		for id := range existingStates {
			if !seen.Has(id) {
				removedCount++
			}
		}
		logger.Info(context.Background(), "ScanTopicFacts: scan complete", "total", len(result), "new", newCount, "removed", removedCount, "existing", len(result)-newCount)
	}

	return result
}
