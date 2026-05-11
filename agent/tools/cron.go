package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/cron"
	robfigcron "github.com/robfig/cron/v3"
)

// ToolNameCron is the name for the CronTool registered in the tool registry.
const ToolNameCron = "Cron"

// nextRunParser parses cron expressions to compute next run time.
// Use the same parser as the Scheduler.
var nextRunParser = robfigcron.NewParser(robfigcron.Minute | robfigcron.Hour | robfigcron.Dom | robfigcron.Month | robfigcron.Dow | robfigcron.Descriptor)

// CronTool allows the LLM to manage scheduled cron jobs.
type CronTool struct {
	scheduler    *cron.Scheduler
	threadIDFunc func() string // provides current thread ID for auto-fill
}

// NewCronTool creates a CronTool backed by the given scheduler.
// threadIDFunc is called during create to auto-fill the target thread.
func NewCronTool(scheduler *cron.Scheduler, threadIDFunc func() string) *CronTool {
	return &CronTool{
		scheduler:    scheduler,
		threadIDFunc: threadIDFunc,
	}
}

func (t *CronTool) Name() string   { return ToolNameCron }
func (t *CronTool) Parallel() bool { return false } // mutations should be sequential

func (t *CronTool) Description() string {
	return `Manage scheduled cron jobs. Jobs automatically trigger at the specified schedule and send the prompt to the target thread.

Actions:
- list: List all cron jobs
- create: Create a new cron job (requires: name, schedule, prompt)
- get: Get details of a specific job (requires: id)
- update: Update an existing job (requires: id, plus fields to change)
- delete: Delete a job (requires: id)
- pause: Pause a job (requires: id)
- resume: Resume a paused job (requires: id)`
}

func (t *CronTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"action": {
			Type:        "string",
			Description: "The action to perform: list, create, get, update, delete, pause, resume",
		},
		"id": {
			Type:        "string",
			Description: "Job ID (required for get/update/delete/pause/resume)",
		},
		"name": {
			Type:        "string",
			Description: "Human-readable job name (required for create)",
		},
		"schedule": {
			Type:        "string",
			Description: `Cron expression. Standard 5-field (minute hour day month weekday) or predefined: @yearly, @monthly, @weekly, @daily, @hourly, @every <duration>. Examples: "0 9 * * 1-5" (weekdays 9am), "*/30 * * * *" (every 30min), "@every 2h"`,
		},
		"prompt": {
			Type:        "string",
			Description: "The prompt to send to the LLM when the job triggers (required for create)",
		},
		"timezone": {
			Type:        "string",
			Description: "IANA timezone for schedule evaluation (default: system local). Example: Asia/Shanghai, UTC",
		},
	}
}

func (t *CronTool) Required() []string {
	return []string{"action"}
}

// cronArgs mirrors the JSON arguments the LLM passes.
type cronArgs struct {
	Action   string `json:"action"`
	ID       string `json:"id"`
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
	Prompt   string `json:"prompt"`
	Timezone string `json:"timezone"`
}

func (t *CronTool) ExecuteContext(ctx context.Context, args string) (string, error) {
	var params cronArgs
	if err := parseArgs(args, &params); err != nil {
		return "", err
	}

	switch params.Action {
	case "list":
		return t.handleList()
	case "create":
		return t.handleCreate(params)
	case "get":
		return t.handleGet(params)
	case "update":
		return t.handleUpdate(params)
	case "delete":
		return t.handleDelete(params)
	case "pause":
		return t.handlePause(params)
	case "resume":
		return t.handleResume(params)
	default:
		return "", fmt.Errorf("cron: unknown action %q. Valid actions: list, create, get, update, delete, pause, resume", params.Action)
	}
}

func (t *CronTool) handleList() (string, error) {
	jobs, err := t.scheduler.List()
	if err != nil {
		return "", fmt.Errorf("cron: list: %w", err)
	}

	if len(jobs) == 0 {
		return "No cron jobs configured.", nil
	}

	sort.Slice(jobs, func(i, j int) bool {
		return jobs[i].CreatedAt.Before(jobs[j].CreatedAt)
	})

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📋 Cron Jobs (%d total)\n\n", len(jobs)))

	for _, job := range jobs {
		sb.WriteString(formatJobSummary(job))
		sb.WriteString("\n")
	}

	return sb.String(), nil
}

func (t *CronTool) handleCreate(params cronArgs) (string, error) {
	// Validate required fields.
	if params.Name == "" {
		return "", fmt.Errorf("cron: name is required for create")
	}
	if params.Schedule == "" {
		return "", fmt.Errorf("cron: schedule is required for create")
	}
	if params.Prompt == "" {
		return "", fmt.Errorf("cron: prompt is required for create")
	}

	// Get the current thread ID.
	threadID := ""
	if t.threadIDFunc != nil {
		threadID = t.threadIDFunc()
	}

	job := &cron.Job{
		Name:           params.Name,
		Schedule:       params.Schedule,
		Prompt:         params.Prompt,
		Timezone:       params.Timezone,
		TargetType:     "channel",
		TargetThreadID: threadID,
	}

	created, err := t.scheduler.Create(job)
	if err != nil {
		return "", fmt.Errorf("cron: create: %w", err)
	}

	return formatJobCreated(created), nil
}

func (t *CronTool) handleGet(params cronArgs) (string, error) {
	if params.ID == "" {
		return "", fmt.Errorf("cron: id is required for get")
	}

	job, err := t.scheduler.Get(params.ID)
	if err != nil {
		return "", fmt.Errorf("cron: get: %w", err)
	}
	if job == nil {
		return fmt.Sprintf("❌ Job %q not found.", params.ID), nil
	}

	return formatJobDetail(job), nil
}

func (t *CronTool) handleUpdate(params cronArgs) (string, error) {
	if params.ID == "" {
		return "", fmt.Errorf("cron: id is required for update")
	}

	opts := cron.UpdateOpts{}
	if params.Name != "" {
		opts.Name = &params.Name
	}
	if params.Schedule != "" {
		opts.Schedule = &params.Schedule
	}
	if params.Prompt != "" {
		opts.Prompt = &params.Prompt
	}
	if params.Timezone != "" {
		opts.Timezone = &params.Timezone
	}

	updated, err := t.scheduler.Update(params.ID, opts)
	if err != nil {
		return "", fmt.Errorf("cron: update: %w", err)
	}

	return "✅ Job updated.\n\n" + formatJobDetail(updated), nil
}

func (t *CronTool) handleDelete(params cronArgs) (string, error) {
	if params.ID == "" {
		return "", fmt.Errorf("cron: id is required for delete")
	}

	if err := t.scheduler.Delete(params.ID); err != nil {
		return "", fmt.Errorf("cron: delete: %w", err)
	}

	return fmt.Sprintf("✅ Job %q deleted.", params.ID), nil
}

func (t *CronTool) handlePause(params cronArgs) (string, error) {
	if params.ID == "" {
		return "", fmt.Errorf("cron: id is required for pause")
	}

	job, err := t.scheduler.Pause(params.ID)
	if err != nil {
		return "", fmt.Errorf("cron: pause: %w", err)
	}

	return fmt.Sprintf("⏸️ Job %q (%s) paused.", job.ID, job.Name), nil
}

func (t *CronTool) handleResume(params cronArgs) (string, error) {
	if params.ID == "" {
		return "", fmt.Errorf("cron: id is required for resume")
	}

	job, err := t.scheduler.Resume(params.ID)
	if err != nil {
		return "", fmt.Errorf("cron: resume: %w", err)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("▶️ Job %q (%s) resumed.\n", job.ID, job.Name))
	sb.WriteString(fmt.Sprintf("Schedule: %s\n", job.Schedule))
	if nextRun := computeNextRun(job.Schedule); nextRun != nil {
		sb.WriteString(fmt.Sprintf("Next run: %s\n", nextRun.Format("2006-01-02 15:04:05 MST")))
	}
	return sb.String(), nil
}

// --- Formatting helpers ---

func formatJobSummary(job *cron.Job) string {
	status := "🟢"
	if job.Status == cron.JobStatusPaused {
		status = "⏸️"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%s **%s** [%s] `%s`\n", status, job.Name, job.ID, job.Schedule))

	// Prompts can be long; show a snippet.
	promptPreview := job.Prompt
	if len(promptPreview) > 80 {
		promptPreview = promptPreview[:80] + "..."
	}
	sb.WriteString(fmt.Sprintf("  Prompt: %s\n", promptPreview))

	nextRun := computeNextRun(job.Schedule)
	if nextRun != nil && job.Status == cron.JobStatusActive {
		sb.WriteString(fmt.Sprintf("  Next: %s\n", nextRun.Format("2006-01-02 15:04:05 MST")))
	}

	if !job.LastRunAt.IsZero() {
		lastStatus := job.LastRunStatus
		if lastStatus == "" {
			lastStatus = "unknown"
		}
		icon := "✅"
		if lastStatus == "error" {
			icon = "❌"
		}
		sb.WriteString(fmt.Sprintf("  Last: %s %s", icon, job.LastRunAt.Format("2006-01-02 15:04:05 MST")))
		if job.LastRunError != "" {
			sb.WriteString(fmt.Sprintf(" (%s)", job.LastRunError))
		}
	}

	return sb.String()
}

func formatJobDetail(job *cron.Job) string {
	var sb strings.Builder

	status := "Active"
	if job.Status == cron.JobStatusPaused {
		status = "Paused"
	}

	sb.WriteString(fmt.Sprintf("**%s** [%s]\n", job.Name, job.ID))
	sb.WriteString(fmt.Sprintf("- Status: %s\n", status))
	sb.WriteString(fmt.Sprintf("- Schedule: `%s`\n", job.Schedule))

	nextRun := computeNextRun(job.Schedule)
	if nextRun != nil && job.Status == cron.JobStatusActive {
		sb.WriteString(fmt.Sprintf("- Next run: %s\n", nextRun.Format("2006-01-02 15:04:05 MST")))
	}

	sb.WriteString(fmt.Sprintf("- Prompt: %s\n", job.Prompt))
	sb.WriteString(fmt.Sprintf("- Target: channel thread %s\n", job.TargetThreadID))
	if job.Timezone != "" {
		sb.WriteString(fmt.Sprintf("- Timezone: %s\n", job.Timezone))
	}

	sb.WriteString(fmt.Sprintf("- Created: %s\n", job.CreatedAt.Format("2006-01-02 15:04:05 MST")))

	if !job.LastRunAt.IsZero() {
		sb.WriteString(fmt.Sprintf("- Last run: %s (status: %s)", job.LastRunAt.Format("2006-01-02 15:04:05 MST"), job.LastRunStatus))
		if job.LastRunError != "" {
			sb.WriteString(fmt.Sprintf(" — %s", job.LastRunError))
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

func formatJobCreated(job *cron.Job) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("✅ Created cron job: **%s** (ID: `%s`)\n\n", job.Name, job.ID))
	sb.WriteString(fmt.Sprintf("- Schedule: `%s`\n", job.Schedule))

	nextRun := computeNextRun(job.Schedule)
	if nextRun != nil {
		sb.WriteString(fmt.Sprintf("- Next run: %s\n", nextRun.Format("2006-01-02 15:04:05 MST")))
	}

	sb.WriteString(fmt.Sprintf("- Prompt: %s\n", job.Prompt))
	sb.WriteString(fmt.Sprintf("- Target: current thread (%s)\n", job.TargetThreadID))

	if job.Timezone != "" {
		sb.WriteString(fmt.Sprintf("- Timezone: %s\n", job.Timezone))
	}

	return sb.String()
}

// computeNextRun parses a cron expression and returns the next fire time.
// Returns nil if the expression cannot be parsed.
func computeNextRun(schedule string) *time.Time {
	sched, err := nextRunParser.Parse(schedule)
	if err != nil {
		return nil
	}
	next := sched.Next(time.Now())
	return &next
}