package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/pkg/logger"
)

// pauseStore persists provider pause state to a JSON file. It is shared by
// the WebSearch and WebFetch tools: each tool instance points at its own
// file (config.WebSearchPausePath / config.WebFetchPausePath), and a quota
// pause survives across runs and propagates process-wide. Access is
// serialized by a package-level mutex; writes are atomic (tmp + rename), so
// a corrupt file never blocks fetches/searches.
type pauseStore struct {
	path string
}

// websearchPauseRecord describes a provider paused due to quota exhaustion.
type websearchPauseRecord struct {
	PausedAt    time.Time `json:"paused_at"`
	ResumeAfter time.Time `json:"resume_after"`
	Reason      string    `json:"reason"`
}

type websearchPauseData struct {
	Providers map[string]websearchPauseRecord `json:"providers"`
}

// pauseStoreMu serializes pause-file reads and writes across all tool
// instances in this process.
var pauseStoreMu sync.Mutex

// pausedProviders returns providers still paused at now. Entries whose
// resume time has passed are considered active again and pruned from the file.
func (s *pauseStore) pausedProviders(now time.Time) map[string]websearchPauseRecord {
	pauseStoreMu.Lock()
	defer pauseStoreMu.Unlock()

	data := s.load()
	out := make(map[string]websearchPauseRecord, len(data.Providers))
	expired := false
	for name, rec := range data.Providers {
		if now.Before(rec.ResumeAfter) {
			out[name] = rec
		} else {
			expired = true
		}
	}
	if expired {
		data.Providers = out
		if err := s.save(data); err != nil {
			logger.Default().Warn(context.Background(), "provider pause state: failed to prune expired pauses",
				"path", s.path, "err", err)
		}
	}
	return out
}

// pause marks a provider paused until resumeAfter (the start of the next
// billing cycle). Existing pause records for other providers are preserved.
func (s *pauseStore) pause(provider, reason string, resumeAfter time.Time) {
	pauseStoreMu.Lock()
	defer pauseStoreMu.Unlock()

	data := s.load()
	if data.Providers == nil {
		data.Providers = map[string]websearchPauseRecord{}
	}
	data.Providers[provider] = websearchPauseRecord{
		PausedAt:    time.Now(),
		ResumeAfter: resumeAfter,
		Reason:      reason,
	}
	if err := s.save(data); err != nil {
		logger.Default().Warn(context.Background(), "provider pause state: failed to persist pause",
			"provider", provider, "path", s.path, "err", err)
	}
}

// load reads the pause file; a missing or unreadable file yields empty state.
func (s *pauseStore) load() *websearchPauseData {
	data, err := os.ReadFile(s.path)
	if err != nil {
		// Missing file or unreadable state — treat as no pauses rather than
		// blocking every search.
		return &websearchPauseData{}
	}
	var pd websearchPauseData
	if err := json.Unmarshal(data, &pd); err != nil {
		return &websearchPauseData{}
	}
	if pd.Providers == nil {
		pd.Providers = map[string]websearchPauseRecord{}
	}
	return &pd
}

// save writes the pause file atomically.
func (s *pauseStore) save(data *websearchPauseData) error {
	if data.Providers == nil {
		data.Providers = map[string]websearchPauseRecord{}
	}
	serialized, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	tmpPath := s.path + ".tmp"
	if err := os.WriteFile(tmpPath, serialized, 0600); err != nil {
		return err
	}
	return os.Rename(tmpPath, s.path)
}
