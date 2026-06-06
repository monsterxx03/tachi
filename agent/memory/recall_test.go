package memory

import (
	"fmt"
	"testing"
	"time"
)

func TestRelativeAge_JustNow(t *testing.T) {
	ts := time.Now().Unix()
	result := RelativeAge(ts)
	if result != "(just now) " {
		t.Errorf("RelativeAge(now) = %q, want %q", result, "(just now) ")
	}
}

func TestRelativeAge_Minutes(t *testing.T) {
	ts := time.Now().Add(-5 * time.Minute).Unix()
	result := RelativeAge(ts)
	if result != "(5 minutes ago) " {
		t.Errorf("RelativeAge(-5m) = %q, want %q", result, "(5 minutes ago) ")
	}
}

func TestRelativeAge_OneMinute(t *testing.T) {
	ts := time.Now().Add(-1 * time.Minute).Unix()
	result := RelativeAge(ts)
	if result != "(1 minutes ago) " {
		t.Errorf("RelativeAge(-1m) = %q, want %q", result, "(1 minutes ago) ")
	}
}

func TestRelativeAge_OneHour(t *testing.T) {
	ts := time.Now().Add(-1 * time.Hour).Unix()
	result := RelativeAge(ts)
	if result != "(1 hour ago) " {
		t.Errorf("RelativeAge(-1h) = %q, want %q", result, "(1 hour ago) ")
	}
}

func TestRelativeAge_Hours(t *testing.T) {
	ts := time.Now().Add(-3 * time.Hour).Unix()
	result := RelativeAge(ts)
	if result != "(3 hours ago) " {
		t.Errorf("RelativeAge(-3h) = %q, want %q", result, "(3 hours ago) ")
	}
}

func TestRelativeAge_OneDay(t *testing.T) {
	ts := time.Now().Add(-24 * time.Hour).Unix()
	result := RelativeAge(ts)
	if result != "(1 day ago) " {
		t.Errorf("RelativeAge(-24h) = %q, want %q", result, "(1 day ago) ")
	}
}

func TestRelativeAge_Days(t *testing.T) {
	ts := time.Now().Add(-72 * time.Hour).Unix()
	result := RelativeAge(ts)
	if result != "(3 days ago) " {
		t.Errorf("RelativeAge(-72h) = %q, want %q", result, "(3 days ago) ")
	}
}

func TestRelativeAge_OneWeek(t *testing.T) {
	ts := time.Now().Add(-7 * 24 * time.Hour).Unix()
	result := RelativeAge(ts)
	if result != "(1 week ago) " {
		t.Errorf("RelativeAge(-7d) = %q, want %q", result, "(1 week ago) ")
	}
}

func TestRelativeAge_Weeks(t *testing.T) {
	ts := time.Now().Add(-14 * 24 * time.Hour).Unix()
	result := RelativeAge(ts)
	if result != "(2 weeks ago) " {
		t.Errorf("RelativeAge(-14d) = %q, want %q", result, "(2 weeks ago) ")
	}
}

func TestRelativeAge_OneMonth(t *testing.T) {
	ts := time.Now().Add(-30 * 24 * time.Hour).Unix()
	result := RelativeAge(ts)
	if result != "(1 month ago) " {
		t.Errorf("RelativeAge(-30d) = %q, want %q", result, "(1 month ago) ")
	}
}

func TestRelativeAge_Months(t *testing.T) {
	ts := time.Now().Add(-60 * 24 * time.Hour).Unix()
	result := RelativeAge(ts)
	if result != "(2 months ago) " {
		t.Errorf("RelativeAge(-60d) = %q, want %q", result, "(2 months ago) ")
	}
}

func TestRelativeAge_BoundaryMinutesToHour(t *testing.T) {
	// 59 minutes: still in "minutes ago" range
	ts := time.Now().Add(-59 * time.Minute).Unix()
	result := RelativeAge(ts)
	if result != "(59 minutes ago) " {
		t.Errorf("RelativeAge(-59m) = %q, want %q", result, "(59 minutes ago) ")
	}
}

func TestRelativeAge_Boundary24hToOneDay(t *testing.T) {
	// 23.5 hours: still "hours ago"
	ts := time.Now().Add(-23*time.Hour - 30*time.Minute).Unix()
	result := RelativeAge(ts)
	if result != "(23 hours ago) " {
		t.Errorf("RelativeAge(-23.5h) = %q, want %q", result, "(23 hours ago) ")
	}
}

func TestRelativeAge_Future(t *testing.T) {
	ts := time.Now().Add(1 * time.Hour).Unix()
	result := RelativeAge(ts)
	if result != "(just now) " {
		t.Errorf("RelativeAge(+1h) = %q, want %q", result, "(just now) ")
	}
}

func TestRelativeAge_Zero(t *testing.T) {
	// Unix epoch (0) is Jan 1 1970 — many years ago, so falls into "months ago"
	// Calculate expected months dynamically to avoid hardcoding a time-dependent value.
	now := time.Now()
	epoch := time.Unix(0, 0)
	expectedMonths := int(now.Sub(epoch).Hours() / (24 * 30))
	expected := fmt.Sprintf("(%d months ago) ", expectedMonths)
	result := RelativeAge(int64(0))
	if result != expected {
		t.Errorf("RelativeAge(0) = %q, want %q (diff may be due to rounding)", result, expected)
	}
}
