// Package cron provides a global cron scheduler for Tachi.
//
// Jobs are persisted to ~/.tachi/crons.json and scheduled via robfig/cron.
// The CronTool (agent/tools/cron.go) exposes CRUD operations to the LLM.
package cron

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// NotifyPolicy controls when a cron job delivers its result to the channel.
type NotifyPolicy string

const (
	// NotifyAlways delivers the result to the channel unconditionally (default).
	NotifyAlways NotifyPolicy = "always"
	// NotifyWhenRelevant instructs the agent to self-evaluate whether the result
	// is worth notifying the user about. If the agent deems it irrelevant
	// (e.g. no changes detected), it responds with [SILENT] and delivery is skipped.
	NotifyWhenRelevant NotifyPolicy = "when_relevant"
)

// SilentMarker is the sentinel string the agent outputs when a when_relevant
// job has nothing worth reporting.
const SilentMarker = "[SILENT]"

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

// TargetType identifies which entry point owns a job.
const (
	// TargetTypeChannel is a channel thread (the only kind that existed before
	// the desktop could schedule).
	TargetTypeChannel = "channel"
	// TargetTypeDesktop is a desktop conversation.
	TargetTypeDesktop = "desktop"
)

// Desktop modes. A desktop job always ends up bound to ONE conversation; Mode
// only says whether that conversation exists yet.
const (
	// DesktopModeSession is a job already bound to DesktopTarget.SessionID.
	DesktopModeSession = "session"
	// DesktopModeNew is a job whose dedicated conversation is created on its
	// first run. After that run, the scheduler binds it (see
	// Scheduler.BindDesktopSession) and the mode becomes DesktopModeSession.
	DesktopModeNew = "new"
)

// DesktopTarget is a desktop job's destination. Exactly one conversation: the
// difference between the two modes is only whether it has been created yet.
type DesktopTarget struct {
	// Mode is session or new.
	Mode string `json:"mode"`
	// SessionID is the LOGICAL conversation id (session.Session.LogicalConversationID)
	// once bound. It survives compaction, so a job keeps writing into the same
	// conversation across every successor session.
	SessionID string `json:"session_id,omitempty"`

	// The following are a TEMPLATE, used only while SessionID is empty and Mode
	// is new: the first run creates the conversation with them. Cleared when the
	// job is bound.
	ProjectID      string   `json:"project_id,omitempty"`
	WorkingDir     string   `json:"working_dir,omitempty"`
	AdditionalDirs []string `json:"additional_dirs,omitempty"`
	ProviderName   string   `json:"provider_name,omitempty"`
	// SkipApproval seeds the dedicated conversation's approval mode. It is a
	// template field for the same reason as the rest: after creation the
	// SESSION's own record is what every run reads.
	SkipApproval bool `json:"skip_approval,omitempty"`
}

// Clone returns a copy that shares no mutable slices with d.
func (d *DesktopTarget) Clone() *DesktopTarget {
	if d == nil {
		return nil
	}
	cp := *d
	cp.AdditionalDirs = append([]string(nil), d.AdditionalDirs...)
	return &cp
}

// Clone returns a copy that shares no mutable slices with j.
func (j *Job) Clone() *Job {
	if j == nil {
		return nil
	}
	cp := *j
	if j.Desktop != nil {
		cp.Desktop = j.Desktop.Clone()
	}
	return &cp
}

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

	// TargetType identifies the consumer type. "channel" (the default) or
	// "desktop". Empty reads as channel, so every job written before the
	// desktop could schedule keeps its meaning.
	TargetType string `json:"target_type"`

	// TargetThreadID is the channel thread to send the response to. Channel-only.
	TargetThreadID string `json:"target_thread_id"`

	// Desktop is the desktop destination (nil on channel jobs). See DesktopTarget.
	Desktop *DesktopTarget `json:"desktop,omitempty"`

	// Status controls whether the job is actively scheduled.
	Status JobStatus `json:"status"`

	// Timezone for schedule evaluation (default: system local).
	Timezone string `json:"timezone,omitempty"`

	// Notify controls delivery policy. "always" (default) sends every result;
	// "when_relevant" asks the agent to evaluate relevance and suppress
	// delivery when the result is not actionable (agent responds with [SILENT]).
	Notify NotifyPolicy `json:"notify,omitempty"`

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

// EffectiveNotify returns the job's notification policy, defaulting to NotifyAlways.
func (j *Job) EffectiveNotify() NotifyPolicy {
	if j.Notify == "" {
		return NotifyAlways
	}
	return j.Notify
}

// ShouldSuppressResult returns true if the job has notify=when_relevant
// and the agent result indicates nothing worth reporting.
func (j *Job) ShouldSuppressResult(result string) bool {
	if j.EffectiveNotify() != NotifyWhenRelevant {
		return false
	}
	trimmed := strings.TrimSpace(result)
	return trimmed == "" || strings.Contains(trimmed, SilentMarker)
}
