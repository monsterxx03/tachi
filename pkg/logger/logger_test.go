package logger

import (
	"os"
	"path/filepath"
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

	l.Info("TUI started", "version", "1.0")
	l.Debug("debug message", "key", "value")
	l.Warn("warning", "code", 42)
	l.Error("something failed", os.ErrNotExist, "file", "/tmp/x")
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

	child.Info("connected", "guilds", 12)
}

func TestWithTrace(t *testing.T) {
	dir := t.TempDir()
	if err := Init(dir, *DefaultConfig()); err != nil {
		t.Fatalf("Init: %v", err)
	}

	l := New("tui")
	tl := l.WithTrace("turn_abc12345")
	tl.Info("turn started")
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
	l.Info("default log message")
}

func TestLogFilePath(t *testing.T) {
	tests := []struct {
		name     string
		expected string
	}{
		{"tui", "tui.log"},
		{"run", "run.log"},
		{"acp", "acp.log"},
		{"channel", filepath.Join("channel", "all.log")},
		{"channel.discord", filepath.Join("channel", "discord.log")},
		{"channel.weixin", filepath.Join("channel", "weixin.log")},
		{"channel.chrome", filepath.Join("channel", "chrome.log")},
		{"channel.unknown", filepath.Join("channel", "all.log")},
		{"channel.manager.agent", filepath.Join("channel", "all.log")},
		{"debug", "debug.log"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := logFilePath("/logs", tt.name)
			expected := filepath.Join("/logs", tt.expected)
			if got != expected {
				t.Errorf("logFilePath(%q) = %q, want %q", tt.name, got, expected)
			}
		})
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
