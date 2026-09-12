package main

import (
	"strings"
	"testing"
	"time"
)

// The notification bodies are deliberately terse and never quote the model —
// they say WHAT happened, not what was said. These tests pin that down, so a
// future "let's preview the reply" change has to be deliberate.
func TestTurnDoneBody(t *testing.T) {
	tests := []struct {
		name       string
		iterations int
		took       time.Duration
		want       string
	}{
		{"counts iterations and duration", 3, 1500 * time.Millisecond, "回合完成 · 3 次迭代 · 1.5s"},
		{"rounds to milliseconds", 1, 250 * time.Millisecond, "回合完成 · 1 次迭代 · 250ms"},
		{"no iteration count to show", 0, 2 * time.Second, "回合完成"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := turnDoneBody(tt.iterations, tt.took); got != tt.want {
				t.Errorf("turnDoneBody(%d, %s) = %q, want %q", tt.iterations, tt.took, got, tt.want)
			}
		})
	}
}

func TestAskBody(t *testing.T) {
	if got, want := askBody(3), "等待你的回答（3 个问题）"; got != want {
		t.Errorf("askBody(3) = %q, want %q", got, want)
	}
	// A single question needs no count — "（1 个问题）" is noise — and neither
	// does a question set that arrived empty.
	for _, n := range []int{0, 1} {
		if got, want := askBody(n), notifyAskWaiting; got != want {
			t.Errorf("askBody(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestOneOffDoneBody(t *testing.T) {
	// The copy NAMES the run: there was no turn on screen, so 回合完成 would leave the reader
	// wondering what finished. What came of it is the reason to come back.
	tests := []struct {
		name     string
		kind     string
		findings int
		outcome  string
		want     string
	}{
		{"review with findings", commandReview, 2, "complete", "评审完成 · 2 条意见"},
		{"review with nothing to report", commandReview, 0, "complete", "评审完成 · 未报问题"},
		{"a later round is still a review", "review-round-2", 1, "complete", "评审完成 · 1 条意见"},
		{"a commit has no findings", commandCommit, 0, "complete", "提交完成"},
		{"stopped by the reader", commandReview, 0, "interrupted", "评审已停止"},
		{"a failed run says so", commandReview, 0, "error", "评审未完成（见日志）"},
		{"an unknown kind still says something", "dream", 0, "complete", "旁路运行完成 · 未报问题"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := oneOffDoneBody(tt.kind, tt.findings, tt.outcome); got != tt.want {
				t.Errorf("oneOffDoneBody(%q, %d, %q) = %q, want %q", tt.kind, tt.findings, tt.outcome, got, tt.want)
			}
		})
	}
}

func TestTruncateRunes(t *testing.T) {
	// The subtitle is cut on character boundaries, never mid-CJK-character, and
	// marked when it was cut.
	if got, want := truncateRunes(strings.Repeat("中", notifySubtitleMaxRune+5), notifySubtitleMaxRune), strings.Repeat("中", notifySubtitleMaxRune)+"…"; got != want {
		t.Errorf("truncateRunes(long) = %q, want %q", got, want)
	}
	if got, want := truncateRunes("短标题", notifySubtitleMaxRune), "短标题"; got != want {
		t.Errorf("truncateRunes(short) = %q, want %q", got, want)
	}
	if got := truncateRunes("任意", 0); got != "" {
		t.Errorf("truncateRunes(max=0) = %q, want empty", got)
	}
}
