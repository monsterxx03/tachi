package cron

import (
	"fmt"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/pkg/strutil"
)

// promptPreviewRunes bounds the prompt snippet a summary shows. The full text is
// what FormatJobDetail is for.
const promptPreviewRunes = 80

// selfSuffix renders the " (this conversation)" tail for a destination that is
// the conversation the reader is in. Empty selfID (no such conversation, or the
// caller does not know it) never matches: a job whose target id is empty is not
// "here", it is nowhere yet.
func selfSuffix(targetID, selfID string) string {
	if targetID == "" || selfID == "" || targetID != selfID {
		return ""
	}
	return " (this conversation)"
}

// DescribeTarget says where a job writes, by id rather than by title: the
// reader is often a model in the middle of a turn, for whom the id is the only
// stable fact. A destination equal to selfID is marked as this very
// conversation — on the desktop the target is the LOGICAL conversation id,
// which survives compaction and need not equal the session the turn runs in.
func DescribeTarget(job *Job, selfID string) string {
	if job == nil {
		return ""
	}
	if job.TargetType == TargetTypeDesktop {
		if job.Desktop == nil {
			return "desktop"
		}
		if job.Desktop.SessionID != "" {
			return "desktop conversation " + job.Desktop.SessionID + selfSuffix(job.Desktop.SessionID, selfID)
		}
		return "a new desktop conversation (created on the first run)"
	}
	return "channel thread " + job.TargetThreadID + selfSuffix(job.TargetThreadID, selfID)
}

// NextRunAt returns when the job will next fire, or nil when it is paused or
// its schedule/timezone cannot be resolved — an unresolvable job is reported as
// invalid by the UI rather than given a made-up time.
func NextRunAt(job *Job) *time.Time {
	if job == nil || job.Status == JobStatusPaused {
		return nil
	}
	next, err := NextRun(job.Schedule, job.Timezone, time.Now())
	if err != nil {
		return nil
	}
	return &next
}

// FormatJobSummary renders one job as the compact entry a list shows: status,
// name, id, schedule, a prompt preview, the next fire time and the last run
// (with its error). Both the model-side tool and the channel's /cron print this,
// so the two can't drift on what a user is told about a job.
func FormatJobSummary(job *Job, selfID string) string {
	if job == nil {
		return ""
	}

	status := "🟢"
	if job.Status == JobStatusPaused {
		status = "⏸️"
	}

	typeTag := ""
	if job.Type == JobTypeOneshot {
		typeTag = " [oneshot]"
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s **%s** [%s] `%s`%s\n", status, job.Name, job.ID, job.Schedule, typeTag)
	fmt.Fprintf(&sb, "  Prompt: %s\n", strutil.Truncate(job.Prompt, promptPreviewRunes))

	if next := NextRunAt(job); next != nil {
		fmt.Fprintf(&sb, "  Next: %s\n", next.Format(strutil.TimeFormatDateTime))
	}

	if !job.LastRunAt.IsZero() {
		icon := "✅"
		if job.LastRunStatus == "error" {
			icon = "❌"
		}
		fmt.Fprintf(&sb, "  Last: %s %s", icon, job.LastRunAt.Format(strutil.TimeFormatDateTime))
		if job.LastRunError != "" {
			fmt.Fprintf(&sb, " (%s)", job.LastRunError)
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

// FormatJobDetail renders everything an operator needs to judge a job without
// opening crons.json: full prompt (never truncated), destination, notify policy,
// timezone, retries, and the outcome of the last run.
func FormatJobDetail(job *Job, selfID string) string {
	if job == nil {
		return ""
	}

	status := "Active"
	if job.Status == JobStatusPaused {
		status = "Paused"
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "**%s** [%s]\n", job.Name, job.ID)
	fmt.Fprintf(&sb, "- Status: %s\n", status)
	if job.Type == JobTypeOneshot {
		sb.WriteString("- Type: oneshot (auto-delete after execution)\n")
	}
	fmt.Fprintf(&sb, "- Schedule: `%s`\n", job.Schedule)

	if next := NextRunAt(job); next != nil {
		fmt.Fprintf(&sb, "- Next run: %s\n", next.Format(strutil.TimeFormatDateTime))
	}

	fmt.Fprintf(&sb, "- Prompt: %s\n", job.Prompt)
	fmt.Fprintf(&sb, "- Target: %s\n", DescribeTarget(job, selfID))
	if job.Notify == NotifyWhenRelevant {
		sb.WriteString("- Notify: when_relevant (suppresses if nothing new)\n")
	}
	if job.MaxRetries > 0 {
		fmt.Fprintf(&sb, "- Max retries: %d\n", job.MaxRetries)
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
