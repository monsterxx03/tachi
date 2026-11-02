package cron

// Why this file exists: cron/format.go is the only code that decides what a job LOOKS like — the
// list a model reads inside a turn, the /cron print, the detail an operator judges a job from. All
// five functions sat at 0%, so a change to any of them would reach the reader with nothing to
// contradict it. The cases below are the boundaries that are easy to get wrong in exactly that way:
// an empty id compared as "equal", a paused job given a next fire time, a truncation applied to a
// field that must never be truncated.

import (
	"strings"
	"testing"
	"time"
)

// selfSuffix marks a destination as "here". The rule that matters: an EMPTY id never matches,
// not even another empty id — a job whose target is empty is nowhere yet, not this conversation.
func TestSelfSuffix(t *testing.T) {
	tests := []struct {
		target, self, want string
	}{
		{"conv-1", "conv-1", " (this conversation)"},
		{"conv-1", "conv-2", ""},
		{"", "", ""},
		{"", "conv-1", ""},
		{"conv-1", "", ""},
	}
	for _, tc := range tests {
		if got := selfSuffix(tc.target, tc.self); got != tc.want {
			t.Errorf("selfSuffix(%q, %q) = %q, want %q", tc.target, tc.self, got, tc.want)
		}
	}
}

// Where a job writes, said by id. A desktop job has three shapes and channel is the fallback for
// every job written before the desktop existed (empty TargetType).
func TestDescribeTarget(t *testing.T) {
	tests := []struct {
		name   string
		job    *Job
		selfID string
		want   string
	}{
		{"nil job", nil, "x", ""},
		{"channel thread", &Job{TargetThreadID: "slack:C1"}, "x", "channel thread slack:C1"},
		{"channel thread, here", &Job{TargetThreadID: "slack:C1"}, "slack:C1",
			"channel thread slack:C1 (this conversation)"},
		{"desktop without a destination", &Job{TargetType: TargetTypeDesktop}, "x", "desktop"},
		{"desktop, new conversation on first run",
			&Job{TargetType: TargetTypeDesktop, Desktop: &DesktopTarget{}}, "x",
			"a new desktop conversation (created on the first run)"},
		{"desktop conversation", &Job{TargetType: TargetTypeDesktop, Desktop: &DesktopTarget{SessionID: "c1"}}, "x",
			"desktop conversation c1"},
		{"desktop conversation, here",
			&Job{TargetType: TargetTypeDesktop, Desktop: &DesktopTarget{SessionID: "c1"}}, "c1",
			"desktop conversation c1 (this conversation)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DescribeTarget(tc.job, tc.selfID); got != tc.want {
				t.Errorf("DescribeTarget = %q, want %q", got, tc.want)
			}
		})
	}
}

// NextRunAt answers nil rather than inventing a time: a paused job does not fire, and a job whose
// schedule or timezone cannot be resolved must be reported as invalid by the UI, not given a guess.
func TestNextRunAt(t *testing.T) {
	if got := NextRunAt(nil); got != nil {
		t.Errorf("NextRunAt(nil) = %v, want nil", got)
	}
	if got := NextRunAt(&Job{Schedule: "@every 1h", Status: JobStatusPaused}); got != nil {
		t.Errorf("a paused job got a next fire time: %v", got)
	}
	if got := NextRunAt(&Job{Schedule: "not a cron expression"}); got != nil {
		t.Errorf("an unresolvable schedule got a next fire time: %v", got)
	}

	now := time.Now()
	next := NextRunAt(&Job{Schedule: "@every 1h"})
	if next == nil {
		t.Fatal("a valid @every job has no next fire time")
	}
	if next.Before(now) {
		t.Errorf("next fire time %v is in the past", next)
	}
}

// The list entry: status icon, name, id, schedule, a bounded prompt preview, and only the fields a
// job actually has.
func TestFormatJobSummary(t *testing.T) {
	base := func() *Job {
		return &Job{
			ID: "cr_1", Name: "daily", Schedule: "@every 1h", Prompt: strings.Repeat("p", 200),
		}
	}

	t.Run("a running job", func(t *testing.T) {
		got := FormatJobSummary(base(), "")
		if !strings.HasPrefix(got, "🟢 **daily** [cr_1] `@every 1h`") {
			t.Errorf("header = %q", strings.SplitN(got, "\n", 2)[0])
		}
		if !strings.Contains(got, "  Prompt: ") {
			t.Error("no prompt line")
		}
		if strings.Contains(got, strings.Repeat("p", promptPreviewRunes+1)) {
			t.Errorf("the prompt preview was not bounded to %d runes", promptPreviewRunes)
		}
		if !strings.Contains(got, "  Next: ") {
			t.Error("a running job with a valid schedule has no Next line")
		}
		if strings.Contains(got, "  Last: ") {
			t.Error("a job that never ran printed a Last line")
		}
	})

	t.Run("paused and oneshot", func(t *testing.T) {
		job := base()
		job.Status = JobStatusPaused
		job.Type = JobTypeOneshot
		got := FormatJobSummary(job, "")
		if !strings.HasPrefix(got, "⏸️ **daily**") {
			t.Errorf("a paused job does not start with the paused icon: %q", strings.SplitN(got, "\n", 2)[0])
		}
		if !strings.Contains(got, " [oneshot]") {
			t.Error("a oneshot job is not marked as one")
		}
		if strings.Contains(got, "  Next: ") {
			t.Error("a paused job printed a Next line")
		}
	})

	t.Run("last run, failed", func(t *testing.T) {
		job := base()
		job.LastRunAt = time.Date(2026, 11, 3, 9, 0, 0, 0, time.Local)
		job.LastRunStatus = "error"
		job.LastRunError = "provider down"
		got := FormatJobSummary(job, "")
		if !strings.Contains(got, "  Last: ❌ ") {
			t.Errorf("a failed last run did not get the ❌ marker: %q", got)
		}
		if !strings.Contains(got, "(provider down)") {
			t.Errorf("the last run's error is missing: %q", got)
		}
	})

	t.Run("nil job", func(t *testing.T) {
		if got := FormatJobSummary(nil, ""); got != "" {
			t.Errorf("FormatJobSummary(nil) = %q, want empty", got)
		}
	})
}

// The detail view is for judging a job without opening crons.json, so the prompt is NEVER truncated
// and the optional lines appear only when the job has them.
func TestFormatJobDetail(t *testing.T) {
	longPrompt := strings.Repeat("q", promptPreviewRunes*3)
	job := &Job{
		ID: "cr_2", Name: "review", Schedule: "0 9 * * *", Prompt: longPrompt,
		TargetType: TargetTypeDesktop, Desktop: &DesktopTarget{SessionID: "conv-9"},
		CreatedAt: time.Date(2026, 10, 1, 8, 0, 0, 0, time.Local),
	}

	got := FormatJobDetail(job, "")
	if !strings.Contains(got, "- Prompt: "+longPrompt) {
		t.Error("the full prompt is not in the detail view (it must never be truncated here)")
	}
	if !strings.Contains(got, "- Target: desktop conversation conv-9") {
		t.Errorf("the destination line is missing: %q", got)
	}
	if !strings.Contains(got, "- Status: Active") {
		t.Errorf("status line is missing: %q", got)
	}
	for _, absent := range []string{"- Notify:", "- Max retries:", "- Timezone:", "- Last run:", "- Type:"} {
		if strings.Contains(got, absent) {
			t.Errorf("%q appears for a job that does not have it", absent)
		}
	}

	full := &Job{
		ID: "cr_3", Name: "n", Schedule: "@daily", Prompt: "p", Type: JobTypeOneshot,
		Status: JobStatusPaused, Notify: NotifyWhenRelevant, MaxRetries: 2, Timezone: "Asia/Shanghai",
		CreatedAt: time.Date(2026, 10, 1, 8, 0, 0, 0, time.Local),
		LastRunAt: time.Date(2026, 11, 2, 9, 0, 0, 0, time.Local), LastRunStatus: "ok",
	}
	got = FormatJobDetail(full, "")
	for _, want := range []string{
		"- Status: Paused",
		"- Type: oneshot (auto-delete after execution)",
		"- Notify: when_relevant (suppresses if nothing new)",
		"- Max retries: 2",
		"- Timezone: Asia/Shanghai",
		"- Last run: ",
		"(status: ok)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("detail is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "- Next run: ") {
		t.Error("a paused job printed a next run")
	}

	if got := FormatJobDetail(nil, ""); got != "" {
		t.Errorf("FormatJobDetail(nil) = %q, want empty", got)
	}
}
