package dream

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent/memory"
	"github.com/monsterxx03/tachi/pkg/debuglog"
)

// HalfLifeDays is the decay half-life in days. Facts decay to 0.5 after this
// many days without reinforcement, to 0.25 after 2×, etc.
const HalfLifeDays = 7

// CalculateDecay computes the decay factor based on time elapsed since the
// last reinforcement. Uses exponential decay with HalfLifeDays half-life:
//
//	decay = exp(-ln(2) × elapsed / halfLife)
//
// Returns 1.0 if lastReinforced is zero (brand new, not yet decayed).
func CalculateDecay(lastReinforced time.Time) float64 {
	if lastReinforced.IsZero() {
		return 1.0
	}
	elapsed := time.Since(lastReinforced)
	halfLife := time.Duration(HalfLifeDays) * 24 * time.Hour
	return math.Exp(-math.Ln2 * elapsed.Seconds() / halfLife.Seconds())
}

// ScanTopicFacts scans all .md files in memoryRoot/topics/, extracts fact
// blocks (separated by ---), and merges their state with existingStates.
//
// - New facts (not in existingStates) are initialized with decay=1.0.
// - Existing facts retain their reinforcement count but have superseded
//   status updated from the block content and decay recalculated.
// - Facts in existingStates not found in topic files are excluded
//   (they were removed/consolidated by the dream agent).
func ScanTopicFacts(memoryRoot string, existingStates map[string]*memory.FactState, logger *debuglog.Logger) map[string]*memory.FactState {
	result := make(map[string]*memory.FactState)
	now := time.Now()

	topicsDir := filepath.Join(memoryRoot, "topics")
	entries, err := os.ReadDir(topicsDir)
	if err != nil {
		if logger != nil {
			logger.Log("ScanTopicFacts: read topics dir: %v", err)
		}
		return result
	}

	seen := make(map[string]bool)

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		topicFile := entry.Name()
		path := filepath.Join(topicsDir, topicFile)

		content, err := os.ReadFile(path)
		if err != nil {
			if logger != nil {
				logger.Log("ScanTopicFacts: read %s: %v", path, err)
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
			seen[id] = true

			superseded := strings.Contains(strings.ToLower(block), "状态: superseded") ||
				strings.Contains(strings.ToLower(block), "status: superseded")

			if existing, ok := existingStates[id]; ok {
				// Clone to avoid mutating the caller's map.
				cloned := *existing
				cloned.Superseded = superseded
				if !cloned.LastReinforced.IsZero() {
					cloned.Decay = CalculateDecay(cloned.LastReinforced)
				}
				result[id] = &cloned
			} else {
				// New fact: initialize with full strength.
				result[id] = &memory.FactState{
					ID:              id,
					TopicFile:       topicFile,
					Decay:           1.0,
					Reinforcements:  0,
					LastReinforced:  time.Time{}, // zero → not yet reinforced
					CreatedAt:       now,
					Superseded:      superseded,
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
			if !seen[id] {
				removedCount++
			}
		}
		logger.Log("ScanTopicFacts: %d total, %d new, %d removed, %d existing",
			len(result), newCount, removedCount, len(result)-newCount)
	}

	return result
}
