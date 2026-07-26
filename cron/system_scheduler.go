package cron

import (
	"context"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/robfig/cron/v3"
)

// SystemScheduler is a system-level cron scheduler completely isolated from
// the user-facing Cron (which is backed by crons.json and managed via CronTool).
//
// Key differences from the user Scheduler:
//   - No persistent store: jobs are registered programmatically at startup.
//   - Invisible to LLM/user: cannot be listed, created, or deleted via CronTool.
//   - Independent lifecycle: registered from config.yaml, not user interaction.
//   - No thread binding: executes handlers directly (no channel delivery).
//
// Currently used for: AutoDream memory consolidation.
type SystemScheduler struct {
	engine *cron.Cron
	logger *logger.Logger

	mu      sync.Mutex
	entries map[string]cron.EntryID // name → entry ID
	ctx     context.Context
	cancel  context.CancelFunc
	started bool
}

// SystemSchedulerConfig holds configuration for creating a SystemScheduler.
type SystemSchedulerConfig struct {
	Logger *logger.Logger
}

// NewSystemScheduler creates a SystemScheduler. Call Start() to begin scheduling.
func NewSystemScheduler(cfg SystemSchedulerConfig) *SystemScheduler {
	l := cfg.Logger

	return &SystemScheduler{
		engine: cron.New(
			cron.WithParser(cron.NewParser(
				cron.Minute|cron.Hour|cron.Dom|cron.Month|cron.Dow|cron.Descriptor,
			)),
			cron.WithLocation(time.Local),
		),
		logger:  l.With("source", "system-cron"),
		entries: make(map[string]cron.EntryID),
	}
}

// Register adds a named job to the scheduler. Must be called before Start().
// The handler receives a context with the given timeout. If the handler returns
// an error, it is logged but does not affect other jobs.
func (s *SystemScheduler) Register(name, schedule string, timeout time.Duration, fn func(ctx context.Context) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return ErrAlreadyStarted
	}

	if timeout <= 0 {
		timeout = 15 * time.Minute
	}

	entryID, err := s.engine.AddFunc(schedule, func() {
		ctx, cancel := context.WithTimeout(s.ctx, timeout)
		defer cancel()

		s.logger.Info(s.ctx, "triggered", "name", name)
		start := time.Now()

		if err := fn(ctx); err != nil {
			s.logger.Error(s.ctx, "failed", err, "name", name, "duration", time.Since(start))
		} else {
			s.logger.Info(s.ctx, "completed", "name", name, "duration", time.Since(start))
		}
	})
	if err != nil {
		return err
	}

	s.entries[name] = entryID
	s.logger.Info(context.Background(), "registered", "name", name, "schedule", schedule, "timeout", timeout)
	return nil
}

// Start begins the scheduler. Must be called after all Register() calls.
func (s *SystemScheduler) Start(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.ctx, s.cancel = context.WithCancel(ctx)
	s.started = true
	s.engine.Start()
	s.logger.Info(s.ctx, "started", "count", len(s.entries))
}

// Stop halts the scheduler and waits for any in-flight job to finish.
func (s *SystemScheduler) Stop() {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()

	<-s.engine.Stop().Done()
	s.logger.Info(s.ctx, "stopped")
}

// ErrAlreadyStarted is returned when Register is called after Start.
var ErrAlreadyStarted = errAlreadyStarted{}

type errAlreadyStarted struct{}

func (errAlreadyStarted) Error() string { return "system scheduler: cannot register after Start()" }
