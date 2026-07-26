package cron

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/robfig/cron/v3"
)

// TriggerHandler is the callback invoked when a cron job fires.
// It receives the Job and a context with a timeout. Implementations
// should execute the prompt against the target and return any error.
type TriggerHandler func(ctx context.Context, job *Job) error

// MaxJobs is the global cap on the number of cron jobs.
const MaxJobs = 50

// Scheduler manages the lifecycle of cron jobs.
// It wraps a Store for persistence and robfig/cron for scheduling.
type Scheduler struct {
	store   *Store
	handler TriggerHandler
	logger  *logger.Logger

	// Underlying cron engine.
	cron *cron.Cron

	// Semaphore to limit concurrent job executions.
	sem chan struct{}

	// Execution timeout for each job trigger.
	executionTimeout time.Duration

	mu       sync.Mutex
	entryMap map[string]cron.EntryID // jobID → cron entry ID
	ctx      context.Context
	cancel   context.CancelFunc
}

// SchedulerConfig holds configuration for creating a Scheduler.
type SchedulerConfig struct {
	Store            *Store
	Handler          TriggerHandler
	Logger           *logger.Logger
	MaxConcurrent    int           // default: 3
	ExecutionTimeout time.Duration // default: 5m
}

// NewScheduler creates a Scheduler. Call Start() to begin scheduling.
func NewScheduler(cfg SchedulerConfig) *Scheduler {
	maxConc := cfg.MaxConcurrent
	if maxConc <= 0 {
		maxConc = 3
	}
	timeout := cfg.ExecutionTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	l := cfg.Logger

	return &Scheduler{
		store:            cfg.Store,
		handler:          cfg.Handler,
		logger:           l.With("source", "cron"),
		sem:              make(chan struct{}, maxConc),
		executionTimeout: timeout,
		cron: cron.New(
			cron.WithParser(cron.NewParser(
				cron.Minute|cron.Hour|cron.Dom|cron.Month|cron.Dow|cron.Descriptor,
			)),
			cron.WithLocation(time.Local),
			cron.WithLogger(cron.VerbosePrintfLogger(loggerWriter{l})),
		),
		entryMap: make(map[string]cron.EntryID),
	}
}

// Start loads all active jobs from the store and starts the cron engine.
// Must be called once after construction.
func (s *Scheduler) Start(ctx context.Context) error {
	s.ctx, s.cancel = context.WithCancel(ctx)

	jobs, err := s.store.List()
	if err != nil {
		return fmt.Errorf("cron: load jobs: %w", err)
	}

	activeCount := 0
	for _, job := range jobs {
		if job.Status != JobStatusActive {
			continue
		}
		// Capture job pointer for the closure.
		j := job
		entryID, addErr := s.addCronEntry(j)
		if addErr != nil {
			s.logger.Error(s.ctx, "cron: failed to add entry", addErr, "id", j.ID, "name", j.Name)
			continue
		}
		s.entryMap[j.ID] = entryID
		activeCount++
	}

	s.cron.Start()
	s.logger.Info(s.ctx, "cron: scheduler started", "count", activeCount)
	return nil
}

// Stop cancels all active timers and waits for in-flight triggers.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	<-s.cron.Stop().Done()
	s.logger.Info(s.ctx, "cron: scheduler stopped")
}

// List returns a copy of all cron jobs.
func (s *Scheduler) List() ([]*Job, error) {
	return s.store.List()
}

// Get returns the job with the given ID, or nil if not found.
func (s *Scheduler) Get(id string) (*Job, error) {
	return s.store.Get(id)
}

// Create adds a new job to the store and schedules it if active.
func (s *Scheduler) Create(job *Job) (*Job, error) {
	if job == nil {
		return nil, fmt.Errorf("cron: job is nil")
	}

	// Validate input.
	if job.Name == "" {
		return nil, fmt.Errorf("cron: name is required")
	}
	if job.Schedule == "" {
		return nil, fmt.Errorf("cron: schedule is required")
	}
	if job.Prompt == "" {
		return nil, fmt.Errorf("cron: prompt is required")
	}

	// Pre-validate cron expression (fail fast before persistence).
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	if _, err := parser.Parse(job.Schedule); err != nil {
		return nil, fmt.Errorf("cron: invalid schedule %q: %w", job.Schedule, err)
	}

	// Check global job count limit.
	jobs, err := s.store.List()
	if err != nil {
		return nil, fmt.Errorf("cron: list jobs: %w", err)
	}
	if len(jobs) >= MaxJobs {
		return nil, fmt.Errorf("cron: maximum %d jobs reached", MaxJobs)
	}

	// Assign defaults.
	if job.ID == "" {
		job.ID = GenerateID()
	}
	if job.Status == "" {
		job.Status = JobStatusActive
	}
	if job.Type == "" {
		job.Type = JobTypeOneshot
	}
	if job.TargetType == "" {
		job.TargetType = "channel"
	}
	now := time.Now()
	if job.CreatedAt.IsZero() {
		job.CreatedAt = now
	}
	job.UpdatedAt = now

	// Persist first.
	if err := s.store.Create(job); err != nil {
		return nil, fmt.Errorf("cron: create job: %w", err)
	}

	// Schedule if active.
	if job.Status == JobStatusActive {
		if err := s.startJobTimer(job); err != nil {
			// Clean up: remove from store since the schedule is invalid.
			s.store.Delete(job.ID)
			return nil, fmt.Errorf("cron: invalid schedule %q: %w", job.Schedule, err)
		}
	}

	s.logger.Info(s.ctx, "cron: created job", "id", job.ID, "name", job.Name, "schedule", job.Schedule)
	return s.copyJob(job), nil
}

// UpdateOpts holds optional fields to update on a job.
type UpdateOpts struct {
	Name     *string
	Schedule *string
	Prompt   *string
	Type     *JobType
	Notify   *NotifyPolicy
	Timezone *string
}

// Update modifies an existing job. If the schedule changed, the job is
// rescheduled. Returns the updated job copy.
func (s *Scheduler) Update(id string, opts UpdateOpts) (*Job, error) {
	if id == "" {
		return nil, fmt.Errorf("cron: id is required")
	}

	job, err := s.store.Get(id)
	if err != nil {
		return nil, fmt.Errorf("cron: get job: %w", err)
	}
	if job == nil {
		return nil, fmt.Errorf("cron: job %q not found", id)
	}

	scheduleChanged := false
	if opts.Name != nil {
		job.Name = *opts.Name
	}
	if opts.Schedule != nil {
		if *opts.Schedule != job.Schedule {
			scheduleChanged = true
		}
		job.Schedule = *opts.Schedule
	}
	if opts.Prompt != nil {
		job.Prompt = *opts.Prompt
	}
	if opts.Timezone != nil {
		job.Timezone = *opts.Timezone
	}
	if opts.Type != nil {
		job.Type = *opts.Type
	}
	if opts.Notify != nil {
		job.Notify = *opts.Notify
	}

	if err := s.store.Update(job); err != nil {
		return nil, fmt.Errorf("cron: update job: %w", err)
	}

	// Reschedule if the schedule changed and the job is active.
	if scheduleChanged && job.Status == JobStatusActive {
		s.stopJobTimer(job.ID)
		if err := s.startJobTimer(job); err != nil {
			s.logger.Error(s.ctx, "cron: failed to reschedule job", err, "id", job.ID)
			job.Status = JobStatusPaused
			s.store.Update(job)
		}
	}

	s.logger.Info(s.ctx, "cron: updated job", "id", job.ID, "name", job.Name)
	return s.copyJob(job), nil
}

// Delete removes a job from the store and stops its timer.
func (s *Scheduler) Delete(id string) error {
	if id == "" {
		return fmt.Errorf("cron: id is required")
	}

	// Stop the timer first.
	s.stopJobTimer(id)

	if err := s.store.Delete(id); err != nil {
		return fmt.Errorf("cron: delete job: %w", err)
	}

	s.logger.Info(s.ctx, "cron: deleted job", "id", id)
	return nil
}

// Pause stops the job's timer and sets its status to paused.
func (s *Scheduler) Pause(id string) (*Job, error) {
	if id == "" {
		return nil, fmt.Errorf("cron: id is required")
	}

	job, err := s.store.Get(id)
	if err != nil {
		return nil, fmt.Errorf("cron: get job: %w", err)
	}
	if job == nil {
		return nil, fmt.Errorf("cron: job %q not found", id)
	}

	job.Status = JobStatusPaused
	if err := s.store.Update(job); err != nil {
		return nil, fmt.Errorf("cron: update job: %w", err)
	}

	s.stopJobTimer(id)

	s.logger.Info(s.ctx, "cron: paused job", "id", job.ID, "name", job.Name)
	return s.copyJob(job), nil
}

// Resume restarts the job's timer and sets its status to active.
func (s *Scheduler) Resume(id string) (*Job, error) {
	if id == "" {
		return nil, fmt.Errorf("cron: id is required")
	}

	job, err := s.store.Get(id)
	if err != nil {
		return nil, fmt.Errorf("cron: get job: %w", err)
	}
	if job == nil {
		return nil, fmt.Errorf("cron: job %q not found", id)
	}

	job.Status = JobStatusActive
	if err := s.store.Update(job); err != nil {
		return nil, fmt.Errorf("cron: update job: %w", err)
	}

	if err := s.startJobTimer(job); err != nil {
		s.logger.Error(s.ctx, "cron: failed to resume job", err, "id", job.ID)
		job.Status = JobStatusPaused
		s.store.Update(job)
		return nil, fmt.Errorf("cron: failed to schedule job: %w", err)
	}

	s.logger.Info(s.ctx, "cron: resumed job", "id", job.ID, "name", job.Name)
	return s.copyJob(job), nil
}

// startJobTimer adds a cron entry for the job. Caller must handle locking.
func (s *Scheduler) startJobTimer(job *Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Already scheduled?
	if _, ok := s.entryMap[job.ID]; ok {
		return nil
	}

	entryID, err := s.addCronEntry(job)
	if err != nil {
		return err
	}

	s.entryMap[job.ID] = entryID
	return nil
}

// stopJobTimer removes the cron entry for the job. Idempotent.
func (s *Scheduler) stopJobTimer(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if entryID, ok := s.entryMap[id]; ok {
		s.cron.Remove(entryID)
		delete(s.entryMap, id)
	}
}

// addCronEntry parses the job's schedule and registers a trigger callback.
// Caller must hold s.mu.
func (s *Scheduler) addCronEntry(job *Job) (cron.EntryID, error) {
	// Capture job pointer for the closure.
	j := job

	entryID, err := s.cron.AddFunc(j.Schedule, func() {
		s.fire(j)
	})
	if err != nil {
		return 0, fmt.Errorf("cron: parse schedule %q: %w", job.Schedule, err)
	}

	return entryID, nil
}

// fire handles the actual job trigger: acquires semaphore, runs handler,
// updates LastRun status, and cleans up oneshot jobs.
func (s *Scheduler) fire(job *Job) {
	// Try to acquire semaphore (non-blocking).
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		s.logger.Info(s.ctx, "cron: skipping job: max concurrent reached", "id", job.ID, "name", job.Name)
		return
	}

	s.logger.Info(s.ctx, "cron: triggering job", "id", job.ID, "name", job.Name)

	ctx, cancel := context.WithTimeout(s.ctx, s.executionTimeout)
	defer cancel()

	startTime := time.Now()
	err := s.handler(ctx, job)
	elapsed := time.Since(startTime)

	// Update last run status in store.
	s.mu.Lock()
	// Reload job from store to get the latest version (may have been updated).
	latest, storeErr := s.store.Get(job.ID)
	if storeErr != nil || latest == nil {
		latest = job // fall back to our copy
	}

	latest.LastRunAt = time.Now()
	if err != nil {
		latest.LastRunStatus = "error"
		latest.LastRunError = err.Error()
		s.logger.Error(s.ctx, "cron: job failed", err, "id", job.ID, "name", job.Name, "duration", elapsed)
	} else {
		latest.LastRunStatus = "success"
		latest.LastRunError = ""
		s.logger.Info(s.ctx, "cron: job succeeded", "id", job.ID, "name", job.Name, "duration", elapsed)
	}

	if updateErr := s.store.Update(latest); updateErr != nil {
		s.logger.Error(s.ctx, "cron: failed to update last_run", updateErr, "id", job.ID)
	}

	isOneshot := latest.Type == JobTypeOneshot
	s.mu.Unlock()

	// Clean up oneshot jobs after releasing the lock to avoid deadlock
	// (Delete also acquires s.mu).
	if isOneshot {
		if delErr := s.Delete(job.ID); delErr != nil {
			s.logger.Error(s.ctx, "cron: failed to clean up oneshot job", delErr, "id", job.ID)
		} else {
			s.logger.Info(s.ctx, "cron: oneshot job completed and removed", "id", job.ID, "name", job.Name)
		}
	}
}

// copyJob returns a shallow copy of the job.
func (s *Scheduler) copyJob(job *Job) *Job {
	cp := *job
	return &cp
}

// loggerWriter adapts logger.Logger to io.Writer for robfig/cron.
type loggerWriter struct {
	logger *logger.Logger
}

func (w loggerWriter) Write(p []byte) (n int, err error) {
	w.logger.Logf(context.Background(), "cron: %s", string(p))
	return len(p), nil
}

func (w loggerWriter) Printf(format string, args ...any) {
	w.logger.Logf(context.Background(), "cron: "+format, args...)
}
