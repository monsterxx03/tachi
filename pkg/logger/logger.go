package logger

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ── Global state ──────────────────────────────────────────────────────────

var (
	logDir      string
	cfgMaxSize  int64    = 10 * 1024 * 1024 // 10MB default
	cfgMaxFiles int      = 10
	cfgLevel             = slog.LevelInfo
	cfgPerEntry          = true
	writers     sync.Map // map[string]*rotatingWriter — keyed by file path
	onceDef     sync.Once
	defaultL    *Logger
)

// Init sets the global log directory and configuration. Must be called once
// before any New() calls. The log directory is created if it doesn't exist.
//
// cfg.MaxSize is parsed as a human-readable size string (e.g. "10mb", "1gb").
// cfg.Level is one of "debug", "info", "warn", "error".
func Init(dir string, cfg Config) error {
	logDir = dir
	cfgPerEntry = cfg.PerEntry

	if cfg.MaxFiles > 0 {
		cfgMaxFiles = cfg.MaxFiles
	}
	if sz := parseSize(cfg.MaxSize); sz > 0 {
		cfgMaxSize = sz
	}
	if cfg.Level != "" {
		cfgLevel = parseLevel(cfg.Level).slogLevel()
	}

	return os.MkdirAll(dir, 0755)
}

// ── Logger ────────────────────────────────────────────────────────────────

// Logger wraps slog.Logger with a namespace (source) and a simplified interface.
// The zero value is NOT usable; create instances via New().
type Logger struct {
	slog   *slog.Logger
	name   string
	writer *rotatingWriter // may be nil if shared with parent
}

// New creates a named Logger. The name serves as the "source" attribute and
// determines the log file path:
//
//	New("tui")              → tui.log
//	New("channel.discord")  → channel/discord.log
//	New("channel")          → channel/all.log
//	New("run")              → run.log
//
// Init() must be called first to set the log directory.
func New(name string) *Logger {
	return newLogger(name, nil)
}

// NewSub creates a child Logger that appends subName to the parent's name.
//
//	l := New("channel")
//	dl := l.NewSub("discord")  // name = "channel.discord"
//
// The child shares the parent's writer.
func (l *Logger) NewSub(subName string) *Logger {
	if l == nil {
		return New(subName)
	}
	newName := l.name + "." + subName
	return newLogger(newName, l.writer)
}

// With returns a Logger copy with additional attributes.
func (l *Logger) With(attrs ...any) *Logger {
	if l == nil || l.slog == nil {
		return l
	}
	return &Logger{
		slog:   l.slog.With(attrs...),
		name:   l.name,
		writer: l.writer,
	}
}

// Debug logs at DEBUG level.
func (l *Logger) Debug(ctx context.Context, msg string, attrs ...any) {
	l.log(ctx, slog.LevelDebug, msg, attrs...)
}

// Info logs at INFO level.
func (l *Logger) Info(ctx context.Context, msg string, attrs ...any) {
	l.log(ctx, slog.LevelInfo, msg, attrs...)
}

// Warn logs at WARN level.
func (l *Logger) Warn(ctx context.Context, msg string, attrs ...any) {
	l.log(ctx, slog.LevelWarn, msg, attrs...)
}

// Error logs at ERROR level. The err parameter is automatically added as
// the "err" attribute.
func (l *Logger) Error(ctx context.Context, msg string, err error, attrs ...any) {
	if l == nil || l.slog == nil {
		if defaultL != nil {
			defaultL.Error(ctx, msg, err, attrs...)
		}
		return
	}
	all := make([]any, 0, len(attrs)+2)
	if err != nil {
		all = append(all, "err", err.Error())
	}
	all = append(all, attrs...)
	l.log(ctx, slog.LevelError, msg, all...)
}

// Logf is a migration bridge that logs a printf-formatted message at INFO level.
// It exists to ease the transition from debuglog's Log(format, args...) pattern.
// Prefer structured logging (Info, Debug, etc.) in new code.
func (l *Logger) Logf(ctx context.Context, format string, args ...any) {
	if l == nil || l.slog == nil {
		if defaultL != nil {
			defaultL.Logf(ctx, format, args...)
		}
		return
	}
	l.slog.LogAttrs(ctx, slog.LevelInfo, fmt.Sprintf(format, args...))
}

func (l *Logger) log(ctx context.Context, level slog.Level, msg string, attrs ...any) {
	if l == nil || l.slog == nil {
		if defaultL != nil {
			defaultL.log(ctx, level, msg, attrs...)
		}
		return
	}
	l.slog.LogAttrs(ctx, level, msg, toAttrs(attrs)...)
}

// toAttrs converts key-value pairs (where keys must be strings) to slog.Attr slice.
func toAttrs(args []any) []slog.Attr {
	if len(args) == 0 {
		return nil
	}
	attrs := make([]slog.Attr, 0, len(args)/2)
	for i := 0; i+1 < len(args); i += 2 {
		key, ok := args[i].(string)
		if !ok {
			// Non-string keys indicate a programming error; warn so it's not silent.
			fmt.Fprintf(os.Stderr, "logger: key at position %d is %T, not string — pair skipped\n", i, args[i])
			continue
		}
		attrs = append(attrs, slog.Any(key, args[i+1]))
	}
	return attrs
}

// ── Default / Context ─────────────────────────────────────────────────────

// Default returns a fallback Logger. When Init has not been called,
// writes are silently discarded.
func Default() *Logger {
	onceDef.Do(func() {
		defaultL = newLogger("debug", nil)
	})
	return defaultL
}

type loggerKey struct{}

// WithLogger attaches a Logger to the context.
func WithLogger(ctx context.Context, l *Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, l)
}

// FromContext retrieves the Logger from ctx. Falls back to Default().
func FromContext(ctx context.Context) *Logger {
	if l, ok := ctx.Value(loggerKey{}).(*Logger); ok && l != nil {
		return l
	}
	return Default()
}

// ── Internal: logger construction ─────────────────────────────────────────

// newLogger creates a Logger writing to the file determined by name.
// If parentWriter is non-nil, it shares that writer (used by NewSub).
// If logDir is empty (Init not called), writes are silently discarded.
func newLogger(name string, parentWriter *rotatingWriter) *Logger {
	var rw *rotatingWriter
	var w io.Writer
	if parentWriter != nil {
		rw = parentWriter
		w = rw
	} else if logDir != "" {
		rw = getOrCreateWriter(logFilePath(logDir, name))
		w = rw
	} else {
		// Init not called (e.g. in tests) — discard silently.
		w = io.Discard
	}

	h := &textHandler{
		w:        w,
		name:     name,
		minLevel: cfgLevel,
	}

	// Build slog.Logger with source attribute.
	sl := slog.New(h).With(slog.String(FieldSource, name))

	return &Logger{
		slog:   sl,
		name:   name,
		writer: rw,
	}
}

// logFilePath maps a logger name to a log file path.
// When per_entry is false, all names route to debug.log.
func logFilePath(dir, name string) string {
	if !cfgPerEntry {
		return filepath.Join(dir, "debug.log")
	}
	parts := strings.SplitN(name, ".", 3)
	switch parts[0] {
	case "channel":
		if len(parts) >= 2 {
			return filepath.Join(dir, "channel", parts[1]+".log")
		}
		return filepath.Join(dir, "channel", "all.log")
	case "debug":
		return filepath.Join(dir, "debug.log")
	default:
		return filepath.Join(dir, parts[0]+".log")
	}
}

// getOrCreateWriter returns a shared rotatingWriter for the given path.
func getOrCreateWriter(path string) *rotatingWriter {
	if v, ok := writers.Load(path); ok {
		return v.(*rotatingWriter)
	}
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	_ = os.MkdirAll(dir, 0755)
	rw, err := newRotatingWriter(dir, base, cfgMaxSize, cfgMaxFiles)
	if err != nil {
		// Fallback: write to stderr
		return &rotatingWriter{fallback: os.Stderr}
	}
	actual, loaded := writers.LoadOrStore(path, rw)
	if loaded {
		// Another goroutine created a writer for the same path first;
		// close ours to avoid fd leak.
		rw.Close()
		return actual.(*rotatingWriter)
	}
	return rw
}

// ── Custom text handler ───────────────────────────────────────────────────

// textHandler implements slog.Handler with a custom human-readable text format:
//
//	2026-07-14T23:15:00.123+08:00 [INFO ] channel.discord connect=ready guilds=12 trace_id=turn_a1b2
type textHandler struct {
	w        io.Writer
	mu       sync.Mutex
	name     string
	minLevel slog.Level
	attrs    []slog.Attr
}

func (h *textHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.minLevel
}

func (h *textHandler) Handle(ctx context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Inject trace_id from context if present.
	if traceID := TraceIDFromContext(ctx); traceID != "" {
		r.AddAttrs(slog.String(FieldTraceID, traceID))
	}

	// Build combined attribute list: handler attrs + record attrs.
	allAttrs := make([]slog.Attr, 0, len(h.attrs)+r.NumAttrs())
	allAttrs = append(allAttrs, h.attrs...)
	r.Attrs(func(a slog.Attr) bool {
		allAttrs = append(allAttrs, a)
		return true
	})

	// Extract source; remaining become key=value pairs.
	var source string
	var rest []slog.Attr
	for _, a := range allAttrs {
		if a.Key == FieldSource {
			source = a.Value.String()
		} else {
			rest = append(rest, a)
		}
	}
	if source == "" {
		source = h.name
	}

	// Format: time [LEVEL] source message key=value...
	var buf strings.Builder
	buf.WriteString(r.Time.Format(time.RFC3339Nano))
	buf.WriteString(" [")
	buf.WriteString(padLevel(r.Level.String()))
	buf.WriteString("] ")
	buf.WriteString(source)
	buf.WriteByte(' ')
	buf.WriteString(r.Message)

	for _, a := range rest {
		buf.WriteByte(' ')
		buf.WriteString(formatAttr(a))
	}
	buf.WriteByte('\n')

	_, err := io.WriteString(h.w, buf.String())
	return err
}

func (h *textHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]slog.Attr, len(h.attrs)+len(attrs))
	copy(newAttrs, h.attrs)
	copy(newAttrs[len(h.attrs):], attrs)
	return &textHandler{
		w:        h.w,
		name:     h.name,
		minLevel: h.minLevel,
		attrs:    newAttrs,
	}
}

func (h *textHandler) WithGroup(name string) slog.Handler {
	// Groups are not specially formatted; wrap attrs in a group attr.
	return h.WithAttrs([]slog.Attr{slog.Group(name)})
}

// padLevel pads a level string to 5 characters for aligned output.
func padLevel(s string) string {
	switch len(s) {
	case 4:
		return s + " " // "INFO" → "INFO "
	case 3:
		return " " + s + " " // "ERR" → " ERR "
	case 5:
		return s
	default:
		// Unknown length: don't truncate; just return as-is.
		return s
	}
}

// formatAttr formats a slog.Attr as "key=value", with quoting for strings
// containing spaces or special characters.
func formatAttr(a slog.Attr) string {
	k := a.Key
	v := a.Value.String()

	// If the value contains spaces, quotes, or equals, quote it.
	if needsQuoting(v) {
		return fmt.Sprintf("%s=%q", k, v)
	}
	return k + "=" + v
}

func needsQuoting(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '"' || c == '=' || c == '\n' || c == '\t' {
			return true
		}
	}
	return s == ""
}

// ── Rotating file writer ──────────────────────────────────────────────────

// parseSize parses a human-readable size string like "10mb", "1gb", "512kb".
// Returns 0 on parse failure.
func parseSize(s string) int64 {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return 0
	}

	var multiplier int64 = 1
	switch {
	case strings.HasSuffix(s, "gb"):
		multiplier = 1024 * 1024 * 1024
		s = strings.TrimSuffix(s, "gb")
	case strings.HasSuffix(s, "mb"):
		multiplier = 1024 * 1024
		s = strings.TrimSuffix(s, "mb")
	case strings.HasSuffix(s, "kb"):
		multiplier = 1024
		s = strings.TrimSuffix(s, "kb")
	case strings.HasSuffix(s, "b"):
		s = strings.TrimSuffix(s, "b")
	}

	var n int64
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n); err != nil {
		return 0
	}
	return n * multiplier
}

const (
	defaultMaxSize  = 10 * 1024 * 1024 // 10MB
	defaultMaxFiles = 10
)

// rotatingWriter implements io.Writer with size-based log rotation.
type rotatingWriter struct {
	dir      string
	baseName string
	maxSize  int64
	maxFiles int

	mu       sync.Mutex
	file     *os.File
	size     int64
	fallback io.Writer // used when file creation fails (e.g. stderr)
}

func newRotatingWriter(dir, baseName string, maxSize int64, maxFiles int) (*rotatingWriter, error) {
	rw := &rotatingWriter{
		dir:      dir,
		baseName: baseName,
		maxSize:  maxSize,
		maxFiles: maxFiles,
	}
	if err := rw.openCurrent(); err != nil {
		return nil, err
	}
	return rw, nil
}

func (rw *rotatingWriter) openCurrent() error {
	path := filepath.Join(rw.dir, rw.baseName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	rw.file = f

	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	rw.size = fi.Size()
	return nil
}

func (rw *rotatingWriter) Write(p []byte) (n int, err error) {
	if rw.fallback != nil {
		return rw.fallback.Write(p)
	}

	rw.mu.Lock()
	defer rw.mu.Unlock()

	if rw.size+int64(len(p)) > rw.maxSize {
		if err := rw.rotate(); err != nil {
			return 0, err
		}
	}

	n, err = rw.file.Write(p)
	if n > 0 {
		rw.size += int64(n)
	}
	return n, err
}

// rotate closes the current file, shifts old chunks, and opens a fresh file.
func (rw *rotatingWriter) rotate() error {
	if rw.file != nil {
		rw.file.Close()
		rw.file = nil
	}

	for i := rw.maxFiles - 1; i >= 0; i-- {
		var oldName string
		if i == 0 {
			oldName = rw.baseName
		} else {
			oldName = fmt.Sprintf("%s.%d", rw.baseName, i)
		}
		oldPath := filepath.Join(rw.dir, oldName)
		newPath := filepath.Join(rw.dir, fmt.Sprintf("%s.%d", rw.baseName, i+1))

		if i == rw.maxFiles-1 {
			os.Remove(oldPath)
		} else {
			if _, err := os.Stat(oldPath); err == nil {
				os.Rename(oldPath, newPath)
			}
		}
	}

	return rw.openCurrent()
}

// Close closes the underlying file.
func (rw *rotatingWriter) Close() error {
	rw.mu.Lock()
	defer rw.mu.Unlock()

	if rw.file != nil {
		err := rw.file.Close()
		rw.file = nil
		return err
	}
	return nil
}
