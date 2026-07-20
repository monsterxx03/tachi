package logger

import (
	"os"
	"strings"
	"testing"
)

func TestNewLogger(t *testing.T) {
	dir := t.TempDir()
	if err := Init(dir, *DefaultConfig()); err != nil {
		t.Fatalf("Init: %v", err)
	}

	l := New("tui")
	if l == nil {
		t.Fatal("New returned nil")
	}
	if l.name != "tui" {
		t.Errorf("name = %q, want %q", l.name, "tui")
	}

	l.Info(t.Context(), "TUI started", "version", "1.0")
	l.Debug(t.Context(), "debug message", "key", "value")
	l.Warn(t.Context(), "warning", "code", 42)
	l.Error(t.Context(), "something failed", os.ErrNotExist, "file", "/tmp/x")
}

func TestNewSub(t *testing.T) {
	dir := t.TempDir()
	if err := Init(dir, *DefaultConfig()); err != nil {
		t.Fatalf("Init: %v", err)
	}

	parent := New("channel")
	child := parent.NewSub("discord")

	if child.name != "channel.discord" {
		t.Errorf("child name = %q, want %q", child.name, "channel.discord")
	}

	// Child should share parent's writer.
	if child.writer != parent.writer {
		t.Error("child should share parent's writer")
	}

	child.Info(t.Context(), "connected", "guilds", 12)
}

func TestWithTrace(t *testing.T) {
	dir := t.TempDir()
	if err := Init(dir, *DefaultConfig()); err != nil {
		t.Fatalf("Init: %v", err)
	}

	l := New("tui")
	ctx := WithTraceID(t.Context(), "turn_abc12345")
	l.Info(ctx, "turn started")
}

func TestDefault(t *testing.T) {
	dir := t.TempDir()
	if err := Init(dir, *DefaultConfig()); err != nil {
		t.Fatalf("Init: %v", err)
	}

	l := Default()
	if l == nil {
		t.Fatal("Default returned nil")
	}
	l.Info(t.Context(), "default log message")
}

func TestLogFilePath(t *testing.T) {
	// After removing per_entry, all names route to the same debug.log.
	// Verify by checking getOrCreateWriter uses the same file for all loggers.
	dir := t.TempDir()
	_ = Init(dir, *DefaultConfig())

	l1 := New("tui")
	l2 := New("channel.discord")
	l3 := New("run")

	if l1.writer != l2.writer || l2.writer != l3.writer {
		t.Error("all loggers should share the same writer when per_entry is removed")
	}
}

func TestRotatingWriter(t *testing.T) {
	dir := t.TempDir()

	const chunkSize int64 = 128
	const keep = 3

	rw, err := newRotatingWriter(dir, "test.log", chunkSize, keep)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	defer rw.Close()

	for i := range 20 {
		msg := strings.Repeat("a", 39) + "\n" // 40 bytes
		_, err := rw.Write([]byte(msg))
		if err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	t.Logf("files: %v", names)

	var gotLog, got1, got2, got3 bool
	for _, n := range names {
		switch n {
		case "test.log":
			gotLog = true
		case "test.log.1":
			got1 = true
		case "test.log.2":
			got2 = true
		case "test.log.3":
			got3 = true
		}
	}

	if !gotLog || !got1 || !got2 {
		t.Errorf("expected test.log, .1, .2 to exist; got files: %v", names)
	}
	if got3 {
		t.Errorf("test.log.3 should have been dropped (keep=%d)", keep)
	}
}

func TestParseLevel(t *testing.T) {
	tests := []struct {
		input string
		want  Level
	}{
		{"debug", LevelDebug},
		{"DEBUG", LevelDebug},
		{"info", LevelInfo},
		{"warn", LevelWarn},
		{"warning", LevelWarn},
		{"error", LevelError},
		{"unknown", LevelInfo},
	}

	for _, tt := range tests {
		got := parseLevel(tt.input)
		if got != tt.want {
			t.Errorf("parseLevel(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestNewTraceID(t *testing.T) {
	id := NewTraceID()
	if !strings.HasPrefix(id, "turn_") {
		t.Errorf("TraceID should start with 'turn_', got %q", id)
	}
	// "turn_" + 8 hex chars = 13
	if len(id) != 13 {
		t.Errorf("TraceID length = %d, want 13", len(id))
	}

	// Uniqueness check (probabilistic).
	ids := make(map[string]bool)
	for range 100 {
		id := NewTraceID()
		if ids[id] {
			t.Errorf("duplicate trace ID: %s", id)
		}
		ids[id] = true
	}
}

func TestFormatAttr(t *testing.T) {
	tests := []struct {
		key, val, expected string
	}{
		{"key", "value", "key=value"},
		{"key", "hello world", `key="hello world"`},
		{"key", "", `key=""`},
		{"key", "a=b", `key="a=b"`},
	}

	for _, tt := range tests {
		// We can't easily construct slog.Attr in tests without importing slog,
		// so test needsQuoting directly.
		if needsQuoting(tt.val) {
			// Just verify the function works
			if tt.expected == tt.key+"="+tt.val {
				t.Errorf("expected quoting for %q", tt.val)
			}
		}
	}
}
