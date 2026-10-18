package cron

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/pkg/syncx"
	"github.com/robfig/cron/v3"
)

// TriggerHandler is the callback invoked when a cron job fires.
// It receives the Job and a context with a timeout. Implementations
// should execute the prompt against the target and return any error.
type TriggerHandler func(ctx context.Context, job *Job) error

// MaxJobs is the global cap on the number of cron jobs.
const MaxJobs = 50

// entryRef is one scheduled job: which engine's table it lives in, and its id
// there. robfig/cron takes ONE location per engine, so a job in another timezone
// gets its own engine; the pair is what lets Stop tell them apart.
type entryRef struct {
	engine *cron.Cron
	id     cron.EntryID
}

// Scheduler manages the lifecycle of cron jobs.
// It wraps a Store for persistence and robfig/cron for scheduling.
type Scheduler struct {
	store   *Store
	handler TriggerHandler
	logger  *logger.Logger

	// One engine per distinct timezone. The default (time.Local) engine is built
	// at construction; a job whose timezone is something else gets an engine on
	// demand. All of them share the same parser and logger.
	engines map[string]*cron.Cron

	// Semaphore to limit concurrent job executions.
	sem *syncx.Semaphore

	// Execution timeout for each job trigger.
	executionTimeout time.Duration

	// owns decides which jobs THIS process schedules. nil = all (the behaviour
	// before the desktop could schedule), so the channel manager keeps its
	// existing meaning until it opts in.
	owns func(*Job) bool
	// onFinished is the post-trigger notification, called after LastRun (and a
	// oneshot's deletion) is persisted. Nil for callers that need nothing.
	onFinished func(*Job)

	mu       sync.Mutex
	entryMap map[string]entryRef // jobID → engine + entry ID
	ctx      context.Context
	cancel   context.CancelFunc
	started  bool
}

// SchedulerConfig holds configuration for creating a Scheduler.
type SchedulerConfig struct {
	Store            *Store
	Handler          TriggerHandler
	Logger           *logger.Logger
	MaxConcurrent    int           // default: 3
	ExecutionTimeout time.Duration // default: 5m
	// Owns filters the jobs this process schedules. A store is shared by every
	// entry point (a channel process and the desktop), but a job belongs to
	// exactly one of them: without this filter both would fire it.
	Owns func(*Job) bool
	// OnFinished runs after a trigger's LastRun (and oneshot cleanup) has been
	// persisted. The desktop uses it to refresh an open task panel without
	// guessing when the scheduler's own write landed.
	OnFinished func(*Job)
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

	s := &Scheduler{
		store:            cfg.Store,
		handler:          cfg.Handler,
		logger:           l.With("source", "cron"),
		sem:              syncx.NewSemaphore(maxConc),
		executionTimeout: timeout,
		owns:             cfg.Owns,
		onFinished:       cfg.OnFinished,
		engines:          make(map[string]*cron.Cron),
		entryMap:         make(map[string]entryRef),
	}
	s.engines[time.Local.String()] = s.newEngine(time.Local)
	return s
}

// newEngine builds one robfig engine in loc. Caller must hold mu (or be the
// constructor, before anything else can see the Scheduler).
func (s *Scheduler) newEngine(loc *time.Location) *cron.Cron {
	return cron.New(
		cron.WithParser(cron.NewParser(
			cron.Minute|cron.Hour|cron.Dom|cron.Month|cron.Dow|cron.Descriptor,
		)),
		cron.WithLocation(loc),
		cron.WithLogger(cron.VerbosePrintfLogger(loggerWriter{s.logger})),
	)
}

// engineFor returns the engine for loc, creating (and, once started, starting)
// it on demand. Caller must hold mu.
func (s *Scheduler) engineFor(loc *time.Location) *cron.Cron {
	key := loc.String()
	if e, ok := s.engines[key]; ok {
		return e
	}
	e := s.newEngine(loc)
	if s.started {
		e.Start()
	}
	s.engines[key] = e
	return e
}

// ownsJob reports whether this process is responsible for job.
func (s *Scheduler) ownsJob(job *Job) bool {
	if s.owns == nil {
		return true
	}
	return s.owns(job)
}

// Start loads all active jobs from the store and starts the cron engines.
// Must be called once after construction.
func (s *Scheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.ctx, s.cancel = context.WithCancel(ctx)

	jobs, err := s.store.List()
	if err != nil {
		return fmt.Errorf("cron: load jobs: %w", err)
	}

	activeCount := 0
	for _, job := range jobs {
		if job.Status != JobStatusActive || !s.ownsJob(job) {
			continue
		}
		if err := s.addCronEntryLocked(job); err != nil {
			s.logger.Error(s.ctx, "cron: failed to add entry", err, "id", job.ID, "name", job.Name)
			continue
		}
		activeCount++
	}

	for _, e := range s.engines {
		e.Start()
	}
	s.started = true
	s.logger.Info(s.ctx, "cron: scheduler started", "count", activeCount, "engines", len(s.engines))
	return nil
}

// Stop cancels all active timers and waits for in-flight triggers.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return
	}
	s.started = false
	if s.cancel != nil {
		s.cancel()
	}
	engines := make([]*cron.Cron, 0, len(s.engines))
	for _, e := range s.engines {
		engines = append(engines, e)
	}
	s.mu.Unlock()

	for _, e := range engines {
		<-e.Stop().Done()
	}
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

// validateJob checks the target shape and the schedule/timezone pair.
// Create always calls it; Update calls it when either input changed.
func validateJob(job *Job) error {
	if job.Name == "" {
		return fmt.Errorf("cron: name is required")
	}
	if job.Schedule == "" {
		return fmt.Errorf("cron: schedule is required")
	}
	if job.Prompt == "" {
		return fmt.Errorf("cron: prompt is required")
	}
	switch job.TargetType {
	case "", TargetTypeChannel:
		// A channel job's thread id may be empty (the old behaviour: it is
		// filled by the caller that knows the thread).
	case TargetTypeDesktop:
		if job.Desktop == nil {
			return fmt.Errorf("cron: a desktop job needs a desktop target")
		}
		switch job.Desktop.Mode {
		case DesktopModeSession:
			if job.Desktop.SessionID == "" {
				return fmt.Errorf("cron: a desktop session target needs a session id")
			}
		case DesktopModeNew:
			// The dedicated conversation is created on the first run; an empty
			// template is legal (the desktop supplies defaults).
		case "":
			return fmt.Errorf("cron: a desktop target needs a mode (session or new)")
		default:
			return fmt.Errorf("cron: unknown desktop mode %q", job.Desktop.Mode)
		}
	default:
		return fmt.Errorf("cron: unknown target type %q", job.TargetType)
	}
	if _, err := Location(job.Timezone); err != nil {
		return fmt.Errorf("cron: invalid timezone %q: %w", job.Timezone, err)
	}
	if _, err := NextRun(job.Schedule, job.Timezone, time.Now()); err != nil {
		return fmt.Errorf("cron: invalid schedule %q: %w", job.Schedule, err)
	}
	return nil
}

// Create adds a new job to the store and schedules it if active.
func (s *Scheduler) Create(job *Job) (*Job, error) {
	if job == nil {
		return nil, fmt.Errorf("cron: job is nil")
	}

	// Assign defaults before validation: TargetType and Type both have one,
	// and an old caller omitting them must keep working.
	if job.TargetType == "" {
		job.TargetType = TargetTypeChannel
	}
	if job.Status == "" {
		job.Status = JobStatusActive
	}
	if job.Type == "" {
		job.Type = JobTypeOneshot
	}
	if err := validateJob(job); err != nil {
		return nil, err
	}

	// Check global job count limit.
	jobs, err := s.store.List()
	if err != nil {
		return nil, fmt.Errorf("cron: list jobs: %w", err)
	}
	if len(jobs) >= MaxJobs {
		return nil, fmt.Errorf("cron: maximum %d jobs reached", MaxJobs)
	}

	if job.ID == "" {
		job.ID = GenerateID()
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

	// Schedule if active AND this process owns it. A store is shared; the other
	// entry point sees the record but must never fire it.
	if job.Status == JobStatusActive && s.ownsJob(job) {
		s.mu.Lock()
		err := s.addCronEntryLocked(job)
		s.mu.Unlock()
		if err != nil {
			// Clean up: remove from store since the schedule is invalid.
			if delErr := s.store.Delete(job.ID); delErr != nil {
				s.logger.Error(s.ctx, "cron: failed to delete invalid job", delErr, "id", job.ID)
			}
			return nil, fmt.Errorf("cron: invalid schedule %q: %w", job.Schedule, err)
		}
	}

	s.logger.Info(s.ctx, "cron: created job", "id", job.ID, "name", job.Name, "schedule", job.Schedule)
	return job.Clone(), nil
}

// UpdateOpts holds optional fields to update on a job.
type UpdateOpts struct {
	Name     *string
	Schedule *string
	Prompt   *string
	Type     *JobType
	Notify   *NotifyPolicy
	Timezone *string
	Desktop  *DesktopTarget
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

	reschedule := false
	if opts.Name != nil {
		job.Name = *opts.Name
	}
	if opts.Schedule != nil {
		if *opts.Schedule != job.Schedule {
			reschedule = true
		}
		job.Schedule = *opts.Schedule
	}
	if opts.Prompt != nil {
		job.Prompt = *opts.Prompt
	}
	if opts.Timezone != nil {
		if *opts.Timezone != job.Timezone {
			reschedule = true
		}
		job.Timezone = *opts.Timezone
	}
	if opts.Type != nil {
		job.Type = *opts.Type
	}
	if opts.Notify != nil {
		job.Notify = *opts.Notify
	}
	if opts.Desktop != nil {
		job.Desktop = opts.Desktop.Clone()
	}

	if err := validateJob(job); err != nil {
		return nil, err
	}

	if err := s.store.Update(job); err != nil {
		return nil, fmt.Errorf("cron: update job: %w", err)
	}

	// Reschedule if the schedule (or its timezone) changed and the job is
	// active and ours.
	if reschedule && job.Status == JobStatusActive && s.ownsJob(job) {
		s.stopJobTimer(job.ID)
		if err := s.startJobTimer(job); err != nil {
			s.logger.Error(s.ctx, "cron: failed to reschedule job", err, "id", job.ID)
			job.Status = JobStatusPaused
			if upErr := s.store.Update(job); upErr != nil {
				s.logger.Error(s.ctx, "cron: failed to persist paused job", upErr, "id", job.ID)
			}
		}
	}

	s.logger.Info(s.ctx, "cron: updated job", "id", job.ID, "name", job.Name)
	return job.Clone(), nil
}

// BindDesktopSession records the dedicated conversation a "new" desktop job
// created on its first run. It is a targeted write (Store.UpdateFunc), so a
// plan/prompt edit landing at the same moment cannot be clobbered by the bind.
func (s *Scheduler) BindDesktopSession(id, sessionID string) (*Job, error) {
	if id == "" {
		return nil, fmt.Errorf("cron: id is required")
	}
	if sessionID == "" {
		return nil, fmt.Errorf("cron: session id is required")
	}
	updated, err := s.store.UpdateFunc(id, func(job *Job) error {
		if job.TargetType != TargetTypeDesktop {
			return fmt.Errorf("cron: job %q is not a desktop job", id)
		}
		if job.Desktop == nil {
			job.Desktop = &DesktopTarget{}
		}
		job.Desktop.Mode = DesktopModeSession
		job.Desktop.SessionID = sessionID
		// The template is only for creating the conversation; it has one now.
		job.Desktop.ProjectID = ""
		job.Desktop.WorkingDir = ""
		job.Desktop.AdditionalDirs = nil
		job.Desktop.ProviderName = ""
		job.Desktop.SkipApproval = false
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("cron: bind desktop session: %w", err)
	}
	s.logger.Info(s.ctx, "cron: bound desktop job to a conversation", "id", id, "session", sessionID)
	return updated, nil
}

// Delete removes a job from the store and stops its timer.
func (s *Scheduler) Delete(id string) error {
	if id == "" {
		return fmt.Errorf("cron: id is required")
	}

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
	return job.Clone(), nil
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

	if s.ownsJob(job) {
		if err := s.startJobTimer(job); err != nil {
			s.logger.Error(s.ctx, "cron: failed to resume job", err, "id", job.ID)
			job.Status = JobStatusPaused
			if upErr := s.store.Update(job); upErr != nil {
				s.logger.Error(s.ctx, "cron: failed to persist paused job", upErr, "id", job.ID)
			}
			return nil, fmt.Errorf("cron: failed to schedule job: %w", err)
		}
	}

	s.logger.Info(s.ctx, "cron: resumed job", "id", job.ID, "name", job.Name)
	return job.Clone(), nil
}

// RunNow triggers a job immediately, without touching its schedule. It is the
// desktop panel's 「运行一次」 and the smoke suite's clock-independent trigger.
//
// The run is asynchronous and goes through the same path as a scheduled fire,
// so LastRun and the oneshot cleanup behave identically.
func (s *Scheduler) RunNow(id string) error {
	if id == "" {
		return fmt.Errorf("cron: id is required")
	}
	job, err := s.store.Get(id)
	if err != nil {
		return fmt.Errorf("cron: get job: %w", err)
	}
	if job == nil {
		return fmt.Errorf("cron: job %q not found", id)
	}
	if !s.ownsJob(job) {
		return fmt.Errorf("cron: job %q belongs to another entry point", id)
	}
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()
	if !started {
		return fmt.Errorf("cron: scheduler is not running")
	}
	go s.fire(job)
	return nil
}

// startJobTimer adds a cron entry for the job. Caller must NOT hold s.mu.
func (s *Scheduler) startJobTimer(job *Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Already scheduled?
	if _, ok := s.entryMap[job.ID]; ok {
		return nil
	}
	return s.addCronEntryLocked(job)
}

// stopJobTimer removes the cron entry for the job. Idempotent.
func (s *Scheduler) stopJobTimer(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if ref, ok := s.entryMap[id]; ok {
		ref.engine.Remove(ref.id)
		delete(s.entryMap, id)
	}
}

// addCronEntryLocked parses the job's schedule and registers a trigger
// callback. Caller must hold s.mu.
func (s *Scheduler) addCronEntryLocked(job *Job) error {
	loc, err := Location(job.Timezone)
	if err != nil {
		return err
	}
	engine := s.engineFor(loc)

	// Capture the job pointer for the closure.
	j := job
	entryID, err := engine.AddFunc(j.Schedule, func() {
		s.fire(j)
	})
	if err != nil {
		return fmt.Errorf("cron: parse schedule %q: %w", job.Schedule, err)
	}

	s.entryMap[job.ID] = entryRef{engine: engine, id: entryID}
	return nil
}

// fire is one trigger's entry point (the engine's callback and RunNow's
// target). It is the name the existing tests drive directly.
func (s *Scheduler) fire(job *Job) { s.run(job) }

// run handles one trigger: acquires semaphore, runs the handler, updates
// LastRun status, and cleans up oneshot jobs. It is used by the engine's
// callback and by RunNow.
func (s *Scheduler) run(job *Job) {
	// Try to acquire semaphore (non-blocking).
	if !s.sem.TryAcquire() {
		s.logger.Info(s.ctx, "cron: skipping job: max concurrent reached", "id", job.ID, "name", job.Name)
		return
	}
	defer s.sem.Release()

	s.logger.Info(s.ctx, "cron: triggering job", "id", job.ID, "name", job.Name)

	s.mu.Lock()
	base := s.ctx
	s.mu.Unlock()
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, s.executionTimeout)
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

	// After LastRun (and the oneshot cleanup) is on disk: a panel that re-reads
	// on this hook sees the result it is supposed to show.
	if s.onFinished != nil {
		s.onFinished(latest)
	}
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
