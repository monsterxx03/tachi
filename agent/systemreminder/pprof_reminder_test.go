package systemreminder

import (
	"strings"
	"testing"
)

func TestPprofReminder_FiresOnce(t *testing.T) {
	r := &PprofReminder{Enabled: true, Port: 6060, PID: 4242}

	first := r.Generate(t.Context(), Context{})
	if len(first) != 1 {
		t.Fatalf("expected 1 line on first call, got %d", len(first))
	}
	if !strings.Contains(first[0], "127.0.0.1:6060") {
		t.Errorf("expected pprof address in output, got: %s", first[0])
	}
	if !strings.Contains(first[0], "PID 4242") {
		t.Errorf("expected PID in output, got: %s", first[0])
	}

	// One shot — subsequent calls must not repeat the hint.
	second := r.Generate(t.Context(), Context{})
	if len(second) != 0 {
		t.Fatalf("expected empty on second call, got %d lines", len(second))
	}
}

func TestPprofReminder_Disabled(t *testing.T) {
	r := &PprofReminder{Enabled: false, Port: 6060, PID: 1}
	if lines := r.Generate(t.Context(), Context{}); len(lines) != 0 {
		t.Fatalf("expected no output when disabled, got %d lines", len(lines))
	}
}

func TestPprofReminder_ZeroPort(t *testing.T) {
	r := &PprofReminder{Enabled: true, Port: 0, PID: 1}
	if lines := r.Generate(t.Context(), Context{}); len(lines) != 0 {
		t.Fatalf("expected no output when port is 0, got %d lines", len(lines))
	}
}
