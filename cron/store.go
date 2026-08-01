package cron

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/pkg/fileutil"
)

// Store handles cron job persistence to a JSON file.
// Thread-safe: all public methods acquire a mutex.
type Store struct {
	mu   sync.RWMutex
	path string
}

type storeData struct {
	Jobs []*Job `json:"jobs"`
}

// NewStore creates a Store backed by the given file path.
// If the file does not exist, it will be created on first write.
func NewStore(path string) *Store {
	return &Store{path: path}
}

// List returns a copy of all cron jobs.
func (s *Store) List() ([]*Job, error) {
	data, err := s.load()
	if err != nil {
		return nil, err
	}

	// Return a shallow copy so callers can't mutate our internal state.
	jobs := make([]*Job, len(data.Jobs))
	copy(jobs, data.Jobs)
	return jobs, nil
}

// Get returns a copy of the job with the given ID, or nil if not found.
func (s *Store) Get(id string) (*Job, error) {
	data, err := s.load()
	if err != nil {
		return nil, err
	}

	for _, job := range data.Jobs {
		if job.ID == id {
			// Return a copy.
			cp := *job
			return &cp, nil
		}
	}
	return nil, nil
}

// Create adds a new job to the store. Returns an error if a job with the
// same ID already exists.
func (s *Store) Create(job *Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.loadLocked()
	if err != nil {
		return err
	}

	// Check for duplicates.
	for _, j := range data.Jobs {
		if j.ID == job.ID {
			return fmt.Errorf("cron job with ID %q already exists", job.ID)
		}
	}

	// Set timestamps if not already set.
	now := time.Now()
	if job.CreatedAt.IsZero() {
		job.CreatedAt = now
	}
	if job.UpdatedAt.IsZero() {
		job.UpdatedAt = now
	}

	data.Jobs = append(data.Jobs, job)
	return s.saveLocked(data)
}

// Update replaces the job with the given ID in the store.
// Returns an error if the job does not exist.
func (s *Store) Update(job *Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.loadLocked()
	if err != nil {
		return err
	}

	for i, j := range data.Jobs {
		if j.ID == job.ID {
			job.UpdatedAt = time.Now()
			data.Jobs[i] = job
			return s.saveLocked(data)
		}
	}
	return fmt.Errorf("cron job %q not found", job.ID)
}

// Delete removes the job with the given ID from the store.
// Returns an error if the job does not exist.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.loadLocked()
	if err != nil {
		return err
	}

	for i, job := range data.Jobs {
		if job.ID == id {
			data.Jobs = append(data.Jobs[:i], data.Jobs[i+1:]...)
			return s.saveLocked(data)
		}
	}
	return fmt.Errorf("cron job %q not found", id)
}

// load reads the store file and returns the data. Thread-safe via RLock.
func (s *Store) load() (*storeData, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loadLocked()
}

// loadLocked reads the store file. Caller must hold at least RLock.
func (s *Store) loadLocked() (*storeData, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return &storeData{}, nil
		}
		return nil, fmt.Errorf("cron store read: %w", err)
	}

	var sd storeData
	if err := json.Unmarshal(data, &sd); err != nil {
		return nil, fmt.Errorf("cron store parse: %w", err)
	}
	if sd.Jobs == nil {
		sd.Jobs = []*Job{}
	}
	return &sd, nil
}

// saveLocked writes the store data atomically. Caller must hold Lock.
func (s *Store) saveLocked(data *storeData) error {
	if data.Jobs == nil {
		data.Jobs = []*Job{}
	}

	if err := fileutil.AtomicWriteJSONPrivate(s.path, data); err != nil {
		return fmt.Errorf("cron store save: %w", err)
	}
	return nil
}
