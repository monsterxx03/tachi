package agent

import (
	"strings"
	"testing"
	"time"
)

func TestFormatTurnSummary(t *testing.T) {
	tests := []struct {
		name   string
		result *RunResult
		want   string
	}{
		{
			name:   "nil result",
			result: nil,
			want:   "",
		},
		{
			name:   "empty result",
			result: &RunResult{},
			want:   "",
		},
		{
			name:   "iterations + duration + trace (no billing)",
			result: &RunResult{IterationsUsed: 3, Duration: 12 * time.Second, TraceID: "abc"},
			want:   "\n\n*(回合: 3 次迭代, 12.0s, trace: abc)*",
		},
		{
			name:   "cost and credit appended when billed",
			result: &RunResult{IterationsUsed: 2, Duration: 1500 * time.Millisecond, TurnCost: 0.0123, TurnCredit: 4.2},
			want:   "\n\n*(回合: 2 次迭代, 1.5s, ¥0.0123, 4.2 credit)*",
		},
		{
			name:   "unpriced turn (zero cost/credit) hides billing",
			result: &RunResult{IterationsUsed: 1, TurnCost: 0, TurnCredit: 0},
			want:   "\n\n*(回合: 1 次迭代)*",
		},
		{
			name:   "credit without cost (credit_rate set, model unpriced)",
			result: &RunResult{IterationsUsed: 1, TurnCredit: 1},
			want:   "\n\n*(回合: 1 次迭代, 1 credit)*",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FormatTurnSummary(tt.result); got != tt.want {
				t.Errorf("FormatTurnSummary() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTrimTrailingZeros(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{1.5, "1.5"},
		{100.0, "100"},
		{0.0123, "0.0123"},
		{0.1, "0.1"},
		{4.20, "4.2"},
		{123.45678, "123.4568"}, // rounds at 4 decimals
		{0.00001, "0.00001"},    // below the 4-decimal floor: shortest exact form
		{0.0001, "0.0001"},
	}
	for _, tt := range tests {
		got := trimTrailingZeros(tt.in)
		if got != tt.want {
			t.Errorf("trimTrailingZeros(%v) = %q, want %q", tt.in, got, tt.want)
		}
		if strings.Contains(got, "e") {
			t.Errorf("trimTrailingZeros(%v) = %q: exponent notation leaked", tt.in, got)
		}
	}
}
