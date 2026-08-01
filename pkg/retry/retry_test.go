package retry

import (
	"context"
	"testing"
	"time"
)

func TestBackoffDelay(t *testing.T) {
	b := Backoff{BaseDelay: 500 * time.Millisecond, MaxDelay: 2 * time.Second}
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 500 * time.Millisecond},
		{2, 1 * time.Second},
		{3, 2 * time.Second}, // 4x base would exceed max — capped
		{4, 2 * time.Second},
		{10, 2 * time.Second},
	}
	for _, tc := range cases {
		if got := b.Delay(tc.attempt); got != tc.want {
			t.Errorf("Delay(%d) = %v, want %v", tc.attempt, got, tc.want)
		}
	}
}

func TestBackoffDelayNoCap(t *testing.T) {
	b := Backoff{BaseDelay: time.Millisecond, MaxDelay: time.Hour}
	if got := b.Delay(5); got != 16*time.Millisecond {
		t.Errorf("Delay(5) = %v, want 16ms", got)
	}
}

func TestSleep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Sleep(ctx, time.Second); err == nil {
		t.Error("Sleep with canceled ctx: want error, got nil")
	}

	if err := Sleep(context.Background(), 0); err != nil {
		t.Errorf("Sleep(0) = %v, want nil", err)
	}

	start := time.Now()
	if err := Sleep(context.Background(), 5*time.Millisecond); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 5*time.Millisecond {
		t.Errorf("Sleep returned too early: %v", elapsed)
	}
}
