package tools

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/cron"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

// ToolNameCron is the name for the CronTool registered in the tool registry.
const ToolNameCron = "Cron"

// CronTarget binds the Cron tool to the conversation that is calling it.
//
// The tool used to know only about channel threads. Both entry points that
// schedule work now need the same CRUD surface with a different destination,
// so the destination is supplied here instead of being hard-coded:
//
//   - channel: Type=channel, ID = the current thread;
//   - desktop: Type=desktop, ID = the current LOGICAL conversation id (it
//     survives compaction), and Prepare also records the template a "new"
//     dedicated conversation will be created from.
type CronTarget struct {
	// Type is written to Job.TargetType on create.
	Type string

	// ID identifies the current conversation: a channel thread id or a desktop
	// logical conversation id. It is called at create time and as CreatedBy.
	ID func() string

	// SelfID, when set, is the id this conversation has as a job TARGET (the
	// value ID is written as). describeTarget labels a job whose destination
	// equals it "this conversation", so the model does not have to reconcile
	// the target id with the session id in its own prompt: on the desktop the
	// target is the LOGICAL conversation id, which survives compaction and
	// therefore need not equal the session the turn is running in.
	SelfID func() string

	// Matches decides which jobs `list` shows. A desktop caller sees jobs that
	// write into this conversation and jobs it created (a dedicated one).
	Matches func(*cron.Job) bool

	// Prepare fills the target-specific fields on a job about to be created.
	// mode is "session" (the default) or "new". It may reject "new"; the
	// caller checks SupportsNewSession first, so a returned error is a real
	// failure rather than a "not here".
	Prepare func(job *cron.Job, mode string) error

	// SupportsNewSession says whether target="new" is meaningful here. Only a
	// frontend that can create a conversation (the desktop) sets it.
	SupportsNewSession bool
}

// CronTool allows the LLM to manage scheduled cron jobs.
type CronTool struct {
	scheduler *cron.Scheduler
	target    CronTarget
	// onChange is called after a mutation lands. The desktop uses it to tell
	// the frontend to re-read the session list, because a job created or
	// deleted from a conversation changes that conversation's badge count
	// while the turn is still running. Optional: the channel tool has no UI.
	onChange func()
}

// NewCronTool creates a CronTool backed by the given scheduler, scoped to one
// conversation through target.
func NewCronTool(scheduler *cron.Scheduler, target CronTarget) *CronTool {
	return &CronTool{scheduler: scheduler, target: target}
}

// SetOnChange installs the post-mutation callback. nil (the default) is a
// no-op, which is what the channel wants.
func (t *CronTool) SetOnChange(fn func()) { t.onChange = fn }

func (t *CronTool) changed() {
	if t.onChange != nil {
		t.onChange()
	}
}

func (t *CronTool) Name() string   { return ToolNameCron }
func (t *CronTool) Parallel() bool { return false } // mutations should be sequential

func (t *CronTool) Description() string {
	return `Manage scheduled cron jobs. Jobs automatically trigger at the specified schedule and send the prompt to the target conversation.

By default, jobs are oneshot — they fire ONCE then auto-delete. Only set type to "recurring" when the user explicitly asks for a repeating task (daily, weekly, every N hours, etc.).

Actions:
- list: List the cron jobs belonging to this conversation
- create: Create a new cron job (requires: name, schedule, prompt)
- get: Get details of a specific job (requires: id)
- update: Update an existing job (requires: id, plus fields to change)
- delete: Delete a job (requires: id)
- pause: Pause a job (requires: id)
- resume: Resume a paused job (requires: id)`
}

func (t *CronTool) Properties() map[string]PropertySchema {
	props := map[string]PropertySchema{
		"action": {
			Type:        "string",
			Description: "The action to perform.",
			Enum:        []string{"list", "create", "get", "update", "delete", "pause", "resume"},
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
		"type": {
			Type:        "string",
			Description: "Job type. Default is oneshot (fire once, auto-delete). Only set to \"recurring\" when the user explicitly asks for a repeating task like daily/weekly/every N hours.",
			Enum:        []string{"oneshot", "recurring"},
			Default:     "oneshot",
		},
		"notify": {
			Type:        "string",
			Description: `Notification policy. "always" (default) sends every result to the user. "when_relevant" suppresses notification when nothing meaningful is found (e.g. no changes, no updates). Use "when_relevant" when the user is monitoring something that changes infrequently (blog updates, CI status, dependency changes).`,
			Enum:        []string{"always", "when_relevant"},
			Default:     "always",
		},
		"timezone": {
			Type:        "string",
			Description: "IANA timezone for schedule evaluation (default: system local). Example: Asia/Shanghai, UTC",
		},
	}
	if t.target.SupportsNewSession {
		props["target"] = PropertySchema{
			Type:        "string",
			Description: `Where the result is written. "session" (default) = this conversation, inheriting its context. "new" = a dedicated conversation created on the first run; later runs all continue in it. Use "new" for independent monitoring tasks that should not mix with this conversation.`,
			Enum:        []string{"session", "new"},
			Default:     "session",
		}
	}
	return props
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
	Type     string `json:"type"`
	Notify   string `json:"notify"`
	Timezone string `json:"timezone"`
	Target   string `json:"target"`
}

func (t *CronTool) ExecuteContext(ctx context.Context, args string) (string, error) {
	if t.scheduler == nil {
		return "", fmt.Errorf("cron: scheduler not configured — scheduled tasks are disabled in this entry point")
	}

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
	allJobs, err := t.scheduler.List()
	if err != nil {
		return "", fmt.Errorf("cron: list: %w", err)
	}

	var jobs []*cron.Job
	for _, job := range allJobs {
		if t.target.Matches == nil || t.target.Matches(job) {
			jobs = append(jobs, job)
		}
	}

	if len(jobs) == 0 {
		return "No cron jobs configured for this conversation.", nil
	}

	slices.SortFunc(jobs, func(a, b *cron.Job) int {
		return a.CreatedAt.Compare(b.CreatedAt)
	})

	var sb strings.Builder
	fmt.Fprintf(&sb, "📋 Cron Jobs (%d total)\n\n", len(jobs))

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

	mode := params.Target
	if mode == "" {
		mode = "session"
	}
	if mode != "session" && mode != "new" {
		return "", fmt.Errorf(`cron: target must be "session" or "new"`)
	}
	if mode == "new" && !t.target.SupportsNewSession {
		return "", fmt.Errorf("cron: target=new is only available where a new conversation can be created (the desktop app)")
	}

	job := &cron.Job{
		Name:       params.Name,
		Schedule:   params.Schedule,
		Prompt:     params.Prompt,
		Type:       cron.JobType(params.Type),
		Notify:     cron.NotifyPolicy(params.Notify),
		Timezone:   params.Timezone,
		TargetType: t.target.Type,
	}
	if t.target.Prepare != nil {
		if err := t.target.Prepare(job, mode); err != nil {
			return "", fmt.Errorf("cron: prepare target: %w", err)
		}
	}

	created, err := t.scheduler.Create(job)
	if err != nil {
		return "", fmt.Errorf("cron: create: %w", err)
	}
	t.changed()

	return t.formatJobCreated(created), nil
}

func (t *CronTool) handleGet(params cronArgs) (string, error) {
	if params.ID == "" {
		return "", fmt.Errorf("cron: id is required for get")
	}

	job, err := t.scheduler.Get(params.ID)
	if err != nil {
		return "", fmt.Errorf("cron: get: %w", err)
	}
	if job == nil || !t.allowed(job) {
		return fmt.Sprintf("❌ Job %q not found.", params.ID), nil
	}

	return t.formatJobDetail(job), nil
}

// allowed scopes every by-id action to the conversation this tool belongs to.
// `list` filtering alone is not enough: a model that saw an id in a previous
// conversation (or a channel run naming a desktop job) must not be able to
// update or delete work it cannot see. A job outside the scope reads as "not
// found", the same answer a genuinely missing id gets.
func (t *CronTool) allowed(job *cron.Job) bool {
	if job == nil {
		return false
	}
	return t.target.Matches == nil || t.target.Matches(job)
}

func (t *CronTool) handleUpdate(params cronArgs) (string, error) {
	if params.ID == "" {
		return "", fmt.Errorf("cron: id is required for update")
	}
	job, err := t.scheduler.Get(params.ID)
	if err != nil {
		return "", fmt.Errorf("cron: get: %w", err)
	}
	if !t.allowed(job) {
		return fmt.Sprintf("❌ Job %q not found.", params.ID), nil
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
	if params.Type != "" {
		t := cron.JobType(params.Type)
		opts.Type = &t
	}
	if params.Notify != "" {
		n := cron.NotifyPolicy(params.Notify)
		opts.Notify = &n
	}

	updated, err := t.scheduler.Update(params.ID, opts)
	if err != nil {
		return "", fmt.Errorf("cron: update: %w", err)
	}
	t.changed()

	return "✅ Job updated.\n\n" + t.formatJobDetail(updated), nil
}

func (t *CronTool) handleDelete(params cronArgs) (string, error) {
	if params.ID == "" {
		return "", fmt.Errorf("cron: id is required for delete")
	}
	job, err := t.scheduler.Get(params.ID)
	if err != nil {
		return "", fmt.Errorf("cron: get: %w", err)
	}
	if !t.allowed(job) {
		return fmt.Sprintf("❌ Job %q not found.", params.ID), nil
	}

	if err := t.scheduler.Delete(params.ID); err != nil {
		return "", fmt.Errorf("cron: delete: %w", err)
	}
	t.changed()

	return fmt.Sprintf("✅ Job %q deleted.", params.ID), nil
}

func (t *CronTool) handlePause(params cronArgs) (string, error) {
	if params.ID == "" {
		return "", fmt.Errorf("cron: id is required for pause")
	}
	existing, err := t.scheduler.Get(params.ID)
	if err != nil {
		return "", fmt.Errorf("cron: get: %w", err)
	}
	if !t.allowed(existing) {
		return fmt.Sprintf("❌ Job %q not found.", params.ID), nil
	}

	job, err := t.scheduler.Pause(params.ID)
	if err != nil {
		return "", fmt.Errorf("cron: pause: %w", err)
	}
	t.changed()

	return fmt.Sprintf("⏸️ Job %q (%s) paused.", job.ID, job.Name), nil
}

func (t *CronTool) handleResume(params cronArgs) (string, error) {
	if params.ID == "" {
		return "", fmt.Errorf("cron: id is required for resume")
	}
	existing, err := t.scheduler.Get(params.ID)
	if err != nil {
		return "", fmt.Errorf("cron: get: %w", err)
	}
	if !t.allowed(existing) {
		return fmt.Sprintf("❌ Job %q not found.", params.ID), nil
	}

	job, err := t.scheduler.Resume(params.ID)
	if err != nil {
		return "", fmt.Errorf("cron: resume: %w", err)
	}
	t.changed()

	var sb strings.Builder
	fmt.Fprintf(&sb, "▶️ Job %q (%s) resumed.\n", job.ID, job.Name)
	fmt.Fprintf(&sb, "Schedule: %s\n", job.Schedule)
	if nextRun := computeNextRun(job); nextRun != nil {
		fmt.Fprintf(&sb, "Next run: %s\n", nextRun.Format(strutil.TimeFormatDateTime))
	}
	return sb.String(), nil
}

// --- Formatting helpers ---

// describeTarget is the tool-facing description of a job's destination. It is
// deliberately not a session TITLE: the tool runs on the model side, where the
// id is the stable fact. A destination that equals SelfID is marked as this
// very conversation — see the field's comment for why the id can differ from
// the session id the model was told about.
func (t *CronTool) describeTarget(job *cron.Job) string {
	if job.TargetType == cron.TargetTypeDesktop {
		if job.Desktop == nil {
			return "desktop"
		}
		if job.Desktop.SessionID != "" {
			return "desktop conversation " + job.Desktop.SessionID + t.selfSuffix(job.Desktop.SessionID)
		}
		return "a new desktop conversation (created on the first run)"
	}
	return "channel thread " + job.TargetThreadID + t.selfSuffix(job.TargetThreadID)
}

func (t *CronTool) selfSuffix(targetID string) string {
	if targetID == "" || t.target.SelfID == nil || t.target.SelfID() != targetID {
		return ""
	}
	return " (this conversation)"
}

func formatJobSummary(job *cron.Job) string {
	status := "🟢"
	if job.Status == cron.JobStatusPaused {
		status = "⏸️"
	}

	typeTag := ""
	if job.Type == cron.JobTypeOneshot {
		typeTag = " [oneshot]"
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s **%s** [%s] `%s`%s\n", status, job.Name, job.ID, job.Schedule, typeTag)

	// Prompts can be long; show a snippet.
	promptPreview := strutil.Truncate(job.Prompt, 80)
	fmt.Fprintf(&sb, "  Prompt: %s\n", promptPreview)

	nextRun := computeNextRun(job)
	if nextRun != nil && job.Status == cron.JobStatusActive {
		fmt.Fprintf(&sb, "  Next: %s\n", nextRun.Format(strutil.TimeFormatDateTime))
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
		fmt.Fprintf(&sb, "  Last: %s %s", icon, job.LastRunAt.Format(strutil.TimeFormatDateTime))
		if job.LastRunError != "" {
			fmt.Fprintf(&sb, " (%s)", job.LastRunError)
		}
	}

	return sb.String()
}

func (t *CronTool) formatJobDetail(job *cron.Job) string {
	var sb strings.Builder

	status := "Active"
	if job.Status == cron.JobStatusPaused {
		status = "Paused"
	}

	fmt.Fprintf(&sb, "**%s** [%s]\n", job.Name, job.ID)
	fmt.Fprintf(&sb, "- Status: %s\n", status)
	if job.Type == cron.JobTypeOneshot {
		sb.WriteString("- Type: oneshot (auto-delete after execution)\n")
	}
	fmt.Fprintf(&sb, "- Schedule: `%s`\n", job.Schedule)

	nextRun := computeNextRun(job)
	if nextRun != nil && job.Status == cron.JobStatusActive {
		fmt.Fprintf(&sb, "- Next run: %s\n", nextRun.Format(strutil.TimeFormatDateTime))
	}

	fmt.Fprintf(&sb, "- Prompt: %s\n", job.Prompt)
	fmt.Fprintf(&sb, "- Target: %s\n", t.describeTarget(job))
	if job.Notify == cron.NotifyWhenRelevant {
		sb.WriteString("- Notify: when_relevant (suppresses if nothing new)\n")
	}
	if job.Timezone != "" {
		fmt.Fprintf(&sb, "- Timezone: %s\n", job.Timezone)
	}

	fmt.Fprintf(&sb, "- Created: %s\n", job.CreatedAt.Format(strutil.TimeFormatDateTime))

	if !job.LastRunAt.IsZero() {
		fmt.Fprintf(&sb, "- Last run: %s (status: %s)", job.LastRunAt.Format(strutil.TimeFormatDateTime), job.LastRunStatus)
		if job.LastRunError != "" {
			fmt.Fprintf(&sb, " — %s", job.LastRunError)
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

func (t *CronTool) formatJobCreated(job *cron.Job) string {
	var sb strings.Builder
	typeLabel := "recurring cron job"
	if job.Type == cron.JobTypeOneshot {
		typeLabel = "oneshot job (will auto-delete after execution)"
	}
	fmt.Fprintf(&sb, "✅ Created %s: **%s** (ID: `%s`)\n\n", typeLabel, job.Name, job.ID)
	fmt.Fprintf(&sb, "- Schedule: `%s`\n", job.Schedule)

	nextRun := computeNextRun(job)
	if nextRun != nil {
		fmt.Fprintf(&sb, "- Next run: %s\n", nextRun.Format(strutil.TimeFormatDateTime))
	}

	fmt.Fprintf(&sb, "- Prompt: %s\n", job.Prompt)
	fmt.Fprintf(&sb, "- Target: %s\n", t.describeTarget(job))

	if job.Timezone != "" {
		fmt.Fprintf(&sb, "- Timezone: %s\n", job.Timezone)
	}

	return sb.String()
}

// computeNextRun parses a cron expression in the job's own timezone and
// returns the next fire time. Returns nil when the expression or the timezone
// cannot be resolved (the job will be reported as invalid by the UI).
func computeNextRun(job *cron.Job) *time.Time {
	next, err := cron.NextRun(job.Schedule, job.Timezone, time.Now())
	if err != nil {
		return nil
	}
	return &next
}
