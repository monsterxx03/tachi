package cron

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestScheduler(t *testing.T, handler TriggerHandler) (*Scheduler, *Store) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "crons.json")
	store := NewStore(path)

	_ = logger.Init(os.TempDir(), *logger.DefaultConfig())
	scheduler := NewScheduler(SchedulerConfig{
		Store:            store,
		Handler:          handler,
		Logger:           logger.Default(),
		MaxConcurrent:    3,
		ExecutionTimeout: 5 * time.Minute,
	})

	return scheduler, store
}

func TestScheduler_Create(t *testing.T) {
	var mu sync.Mutex
	var triggered []string

	handler := func(ctx context.Context, job *Job) error {
		mu.Lock()
		triggered = append(triggered, job.ID)
		mu.Unlock()
		return nil
	}

	scheduler, _ := newTestScheduler(t, handler)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// Create a job with no trigger (just test CRUD).
	job, err := scheduler.Create(&Job{
		Name:     "Test Job",
		Schedule: "@every 1h", // won't fire during test
		Prompt:   "Test prompt",
	})
	require.NoError(t, err)
	assert.NotEmpty(t, job.ID)
	assert.Equal(t, JobStatusActive, job.Status)

	// Start the scheduler (will schedule the job).
	require.NoError(t, scheduler.Start(ctx))
	defer scheduler.Stop()

	// Verify the job is in the list.
	jobs, err := scheduler.List()
	require.NoError(t, err)
	assert.Len(t, jobs, 1)
	assert.Equal(t, "Test Job", jobs[0].Name)
}

func TestScheduler_PauseResume(t *testing.T) {
	var mu sync.Mutex
	var triggered []string

	handler := func(ctx context.Context, job *Job) error {
		mu.Lock()
		triggered = append(triggered, job.ID)
		mu.Unlock()
		return nil
	}

	scheduler, _ := newTestScheduler(t, handler)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	job, err := scheduler.Create(&Job{
		Name:     "Test Job",
		Schedule: "@every 1h",
		Prompt:   "Test",
	})
	require.NoError(t, err)

	require.NoError(t, scheduler.Start(ctx))
	defer scheduler.Stop()

	// Pause the job.
	paused, err := scheduler.Pause(job.ID)
	require.NoError(t, err)
	assert.Equal(t, JobStatusPaused, paused.Status)

	// Resume the job.
	resumed, err := scheduler.Resume(job.ID)
	require.NoError(t, err)
	assert.Equal(t, JobStatusActive, resumed.Status)
}

func TestScheduler_Update(t *testing.T) {
	var mu sync.Mutex
	var triggered []string

	handler := func(ctx context.Context, job *Job) error {
		mu.Lock()
		triggered = append(triggered, job.ID)
		mu.Unlock()
		return nil
	}

	scheduler, _ := newTestScheduler(t, handler)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	job, err := scheduler.Create(&Job{
		Name:     "Original",
		Schedule: "@every 1h",
		Prompt:   "Original prompt",
	})
	require.NoError(t, err)

	require.NoError(t, scheduler.Start(ctx))
	defer scheduler.Stop()

	newName := "Updated"
	updated, err := scheduler.Update(job.ID, UpdateOpts{
		Name: &newName,
	})
	require.NoError(t, err)
	assert.Equal(t, "Updated", updated.Name)
}

func TestScheduler_Delete(t *testing.T) {
	var mu sync.Mutex
	var triggered []string

	handler := func(ctx context.Context, job *Job) error {
		mu.Lock()
		triggered = append(triggered, job.ID)
		mu.Unlock()
		return nil
	}

	scheduler, _ := newTestScheduler(t, handler)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	job, err := scheduler.Create(&Job{
		Name:     "To Delete",
		Schedule: "@every 1h",
		Prompt:   "Test",
	})
	require.NoError(t, err)

	require.NoError(t, scheduler.Start(ctx))
	defer scheduler.Stop()

	// Delete the job.
	err = scheduler.Delete(job.ID)
	require.NoError(t, err)

	// Verify it's gone.
	got, err := scheduler.Get(job.ID)
	require.NoError(t, err)
	assert.Nil(t, got)

	// Verity list is empty.
	jobs, err := scheduler.List()
	require.NoError(t, err)
	assert.Len(t, jobs, 0)
}

func TestScheduler_Validation(t *testing.T) {
	scheduler, _ := newTestScheduler(t, nil)

	// Missing name.
	_, err := scheduler.Create(&Job{Schedule: "@daily", Prompt: "test"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "name is required")

	// Missing schedule.
	_, err = scheduler.Create(&Job{Name: "test", Prompt: "test"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "schedule is required")

	// Missing prompt.
	_, err = scheduler.Create(&Job{Name: "test", Schedule: "@daily"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "prompt is required")

	// Invalid schedule.
	_, err = scheduler.Create(&Job{Name: "test", Schedule: "invalid", Prompt: "test"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid schedule")
}

func TestScheduler_StartupRecovery(t *testing.T) {
	// Simulate crash recovery: create jobs in store,
	// then create a new scheduler that loads them.

	dir := t.TempDir()
	path := filepath.Join(dir, "crons.json")
	store := NewStore(path)

	// Create jobs directly in store (bypassing scheduler).
	require.NoError(t, store.Create(&Job{
		ID:         "cr_test1",
		Name:       "Test Job",
		Schedule:   "@every 1h",
		Prompt:     "Test",
		Status:     JobStatusActive,
		TargetType: "channel",
	}))

	var mu sync.Mutex
	var triggered []string
	handler := func(ctx context.Context, job *Job) error {
		mu.Lock()
		triggered = append(triggered, job.ID)
		mu.Unlock()
		return nil
	}

	_ = logger.Init(os.TempDir(), *logger.DefaultConfig())
	scheduler := NewScheduler(SchedulerConfig{
		Store:            store,
		Handler:          handler,
		Logger:           logger.Default(),
		MaxConcurrent:    3,
		ExecutionTimeout: 5 * time.Minute,
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	require.NoError(t, scheduler.Start(ctx))
	defer scheduler.Stop()

	jobs, err := scheduler.List()
	require.NoError(t, err)
	assert.Len(t, jobs, 1)
	assert.Equal(t, "cr_test1", jobs[0].ID)
}

func TestScheduler_SemaphoreSkip(t *testing.T) {
	// Test that when max concurrent is reached, new triggers are skipped.
	// We test this by directly driving the semaphore.

	_ = logger.Init(os.TempDir(), *logger.DefaultConfig())

	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "crons.json"))

	var mu sync.Mutex
	var triggered []string
	blockCh := make(chan struct{})

	handler := func(ctx context.Context, job *Job) error {
		mu.Lock()
		triggered = append(triggered, job.ID)
		mu.Unlock()
		<-blockCh // block until test unblocks
		return nil
	}

	scheduler := NewScheduler(SchedulerConfig{
		Store:            store,
		Handler:          handler,
		Logger:           logger.Default(),
		MaxConcurrent:    1,
		ExecutionTimeout: 5 * time.Second,
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// DON'T start the cron engine — we test semaphore directly.
	// But we need to set ctx so fire doesn't panic.
	scheduler.ctx = ctx

	job1 := &Job{ID: "cr_t1", Name: "J1", Schedule: "@every 1h", Prompt: "P1", Status: JobStatusActive}
	job2 := &Job{ID: "cr_t2", Name: "J2", Schedule: "@every 1h", Prompt: "P2", Status: JobStatusActive}

	// Fire job1 in background — handler blocks on blockCh.
	go scheduler.fire(job1)

	// Wait for job1 to acquire the semaphore.
	time.Sleep(50 * time.Millisecond)

	// Fire job2 synchronously — should be skipped because semaphore is full.
	scheduler.fire(job2)

	// Unblock and wait.
	close(blockCh)
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	count := len(triggered)
	mu.Unlock()

	// Only the first job should have been executed.
	assert.Equal(t, 1, count, "only 1 job should execute when max_concurrent=1")
	if count >= 1 {
		assert.Equal(t, "cr_t1", triggered[0], "job1 should be executed")
	}
}

func TestScheduler_OneshotAutoDelete(t *testing.T) {
	// Verify that oneshot jobs are auto-deleted after execution.

	dir := t.TempDir()
	path := filepath.Join(dir, "crons.json")
	store := NewStore(path)

	var mu sync.Mutex
	var triggered []string

	handler := func(ctx context.Context, job *Job) error {
		mu.Lock()
		triggered = append(triggered, job.ID)
		mu.Unlock()
		return nil
	}

	_ = logger.Init(os.TempDir(), *logger.DefaultConfig())
	scheduler := NewScheduler(SchedulerConfig{
		Store:            store,
		Handler:          handler,
		Logger:           logger.Default(),
		MaxConcurrent:    3,
		ExecutionTimeout: 5 * time.Second,
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// Create separate scheduler context so fire() won't panic.
	scheduler.ctx = ctx

	// Create a oneshot job directly in the store.
	oneshotJob := &Job{
		ID:         "cr_oneshot1",
		Name:       "One-shot Task",
		Schedule:   "@every 1h",
		Prompt:     "Run once",
		Type:       JobTypeOneshot,
		Status:     JobStatusActive,
		TargetType: "channel",
	}
	require.NoError(t, store.Create(oneshotJob))

	// Also create a recurring job for comparison.
	recurJob := &Job{
		ID:         "cr_recur1",
		Name:       "Recurring Task",
		Schedule:   "@every 1h",
		Prompt:     "Run forever",
		Type:       JobTypeRecurring,
		Status:     JobStatusActive,
		TargetType: "channel",
	}
	require.NoError(t, store.Create(recurJob))

	// Fire both jobs.
	go scheduler.fire(oneshotJob)
	go scheduler.fire(recurJob)

	// Wait for both to complete.
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	count := len(triggered)
	mu.Unlock()
	assert.Equal(t, 2, count, "both jobs should execute")

	// Verify oneshot job is gone from store.
	got, err := scheduler.Get("cr_oneshot1")
	require.NoError(t, err)
	assert.Nil(t, got, "oneshot job should be auto-deleted after execution")

	// Verify recurring job still exists.
	got, err = scheduler.Get("cr_recur1")
	require.NoError(t, err)
	assert.NotNil(t, got, "recurring job should still exist")
}

func TestScheduler_OneshotDefaultIsOneshot(t *testing.T) {
	// Verify that jobs without an explicit Type default to oneshot
	// and are auto-deleted after execution.

	dir := t.TempDir()
	path := filepath.Join(dir, "crons.json")
	store := NewStore(path)

	handler := func(ctx context.Context, job *Job) error {
		return nil
	}

	_ = logger.Init(os.TempDir(), *logger.DefaultConfig())
	scheduler := NewScheduler(SchedulerConfig{
		Store:            store,
		Handler:          handler,
		Logger:           logger.Default(),
		MaxConcurrent:    3,
		ExecutionTimeout: 5 * time.Second,
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	scheduler.ctx = ctx

	// Create through the scheduler API (tests the Create default path).
	job, err := scheduler.Create(&Job{
		Name:     "No Type Specified",
		Schedule: "@every 1h",
		Prompt:   "Test",
	})
	require.NoError(t, err)
	assert.Equal(t, JobTypeOneshot, job.Type, "default type should be oneshot")

	// Fire it.
	scheduler.fire(job)
	time.Sleep(100 * time.Millisecond)

	// Should be auto-deleted (oneshot jobs are cleaned up).
	got, err := scheduler.Get(job.ID)
	require.NoError(t, err)
	assert.Nil(t, got, "oneshot job should be auto-deleted after execution")
}
