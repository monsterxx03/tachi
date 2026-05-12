// Package cron provides a global cron scheduler for Tachi.
//
// Jobs are persisted to ~/.tachi/crons.json and scheduled via robfig/cron.
// The CronTool (agent/tools/cron.go) exposes CRUD operations to the LLM.
package cron

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// JobStatus represents the lifecycle state of a cron job.
type JobStatus string

const (
	JobStatusActive JobStatus = "active"
	JobStatusPaused JobStatus = "paused"
)

// JobType categorises the schedule pattern of a job.
type JobType string

const (
	// JobTypeRecurring fires repeatedly on the schedule (default).
	JobTypeRecurring JobType = "recurring"
	// JobTypeOneshot fires exactly once, then auto-deletes.
	JobTypeOneshot JobType = "oneshot"
)

// Job represents a scheduled cron task.
type Job struct {
	// ID is a unique identifier (short prefix, e.g. "cr_a1b2c3").
	ID string `json:"id"`

	// Name is a human-readable label set by the LLM.
	Name string `json:"name"`

	// Schedule is a cron expression (5-field or @every/@daily etc).
	Schedule string `json:"schedule"`

	// Type controls the schedule pattern: recurring (default) or oneshot.
	// Oneshot jobs auto-delete after the first execution.
	Type JobType `json:"type,omitempty"`

	// Prompt is the message sent to the LLM when the cron fires.
	Prompt string `json:"prompt"`

	// TargetType identifies the consumer type. Currently only "channel".
	TargetType string `json:"target_type"`

	// TargetThreadID is the channel thread to send the response to.
	TargetThreadID string `json:"target_thread_id"`

	// Status controls whether the job is actively scheduled.
	Status JobStatus `json:"status"`

	// Timezone for schedule evaluation (default: system local).
	Timezone string `json:"timezone,omitempty"`

	// MaxRetries is how many times to retry on execution failure (default: 0).
	MaxRetries int `json:"max_retries,omitempty"`

	// CreatedAt is when the job was created.
	CreatedAt time.Time `json:"created_at"`

	// UpdatedAt is when the job was last modified.
	UpdatedAt time.Time `json:"updated_at"`

	// LastRunAt is when the job last fired (zero if never).
	LastRunAt time.Time `json:"last_run_at,omitzero"`

	// LastRunStatus records the outcome of the last execution.
	LastRunStatus string `json:"last_run_status,omitempty"`

	// LastRunError records the error message if LastRunStatus == "error".
	LastRunError string `json:"last_run_error,omitempty"`

	// CreatedBy records which thread/session created this job (for auditing).
	CreatedBy string `json:"created_by,omitempty"`
}

// GenerateID returns a short unique identifier for a cron job.
func GenerateID() string {
	uid := uuid.New().String()
	return fmt.Sprintf("cr_%s", uid[:6])
}