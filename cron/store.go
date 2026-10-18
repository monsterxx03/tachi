package cron

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/flock"
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

	// Return deep copies so callers can't mutate our internal state (or a
	// desktop target another reader is resolving).
	jobs := make([]*Job, len(data.Jobs))
	for i, job := range data.Jobs {
		jobs[i] = job.Clone()
	}
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
			return job.Clone(), nil
		}
	}
	return nil, nil
}

// write runs fn while holding both this process's mutex (the caller takes it)
// and the cross-process file lock. The lock file lives beside the store and is
// never removed (see pkg/flock): two entry points — a channel process and the
// desktop — write the same crons.json, and "read, change, write" without a
// kernel lock loses one of the two changes.
//
// Callers must hold s.mu; fn is allowed to call loadLocked/saveLocked.
func (s *Store) write(fn func() error) error {
	if s.path == "" {
		return fn()
	}
	lock, err := flock.Acquire(context.Background(), s.path+".lock", 5*time.Second)
	if err != nil {
		return fmt.Errorf("cron store lock: %w", err)
	}
	defer func() { _ = lock.Release() }()
	return fn()
}

// Create adds a new job to the store. Returns an error if a job with the
// same ID already exists.
func (s *Store) Create(job *Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.write(func() error {
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

		data.Jobs = append(data.Jobs, job.Clone())
		return s.saveLocked(data)
	})
}

// Update replaces the job with the given ID in the store.
// Returns an error if the job does not exist.
func (s *Store) Update(job *Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.write(func() error {
		data, err := s.loadLocked()
		if err != nil {
			return err
		}

		for i, j := range data.Jobs {
			if j.ID == job.ID {
				stored := job.Clone()
				stored.UpdatedAt = time.Now()
				job.UpdatedAt = stored.UpdatedAt
				data.Jobs[i] = stored
				return s.saveLocked(data)
			}
		}
		return fmt.Errorf("cron job %q not found", job.ID)
	})
}

// UpdateFunc is a targeted read-modify-write: fn receives the LATEST copy of
// the job under the cross-process lock, so a field it does not touch cannot be
// clobbered by a concurrent edit from another process or the UI. It is the
// shape a first-run binding needs (only the target changes).
func (s *Store) UpdateFunc(id string, fn func(*Job) error) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out *Job
	err := s.write(func() error {
		data, err := s.loadLocked()
		if err != nil {
			return err
		}
		for i, j := range data.Jobs {
			if j.ID != id {
				continue
			}
			cp := j.Clone()
			if err := fn(cp); err != nil {
				return err
			}
			cp.UpdatedAt = time.Now()
			data.Jobs[i] = cp
			if err := s.saveLocked(data); err != nil {
				return err
			}
			out = cp.Clone()
			return nil
		}
		return fmt.Errorf("cron job %q not found", id)
	})
	return out, err
}

// Delete removes a job from the store.
// Returns an error if the job does not exist.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.write(func() error {
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
	})
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
