package debuglog

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

const (
	maxSize  = 10 * 1024 * 1024 // 10MB
	maxFiles = 10
)

var (
	logger       *slog.Logger
	rotateWriter *rotatingWriter
)

// Init initializes the debug logger, writing to ~/.tachi/logs/debug.log
// with rotation: each chunk is 10MB, keeping up to 10 chunks.
func Init() error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	logDir := filepath.Join(homeDir, ".tachi", "logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return err
	}

	rw, err := newRotatingWriter(logDir, "debug.log", maxSize, maxFiles)
	if err != nil {
		return err
	}
	rotateWriter = rw

	logger = slog.New(slog.NewTextHandler(rw, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	return nil
}

// Close closes the debug log file.
func Close() {
	if rotateWriter != nil {
		rotateWriter.Close()
	}
}

// Log writes a formatted message to the debug log at INFO level.
// Format and args follow the same convention as fmt.Sprintf.
func Log(format string, args ...interface{}) {
	if logger != nil {
		logger.Info(fmt.Sprintf(format, args...))
	}
}

// Logger returns the underlying slog.Logger for callers that want
// structured logging with custom levels, attributes, or groups.
func Logger() *slog.Logger {
	return logger
}

// rotatingWriter implements io.Writer with size-based log rotation.
type rotatingWriter struct {
	dir      string
	baseName string
	maxSize  int64
	maxFiles int

	mu   sync.Mutex
	file *os.File
	size int64
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

// rotate closes the current file, shifts old chunks (debug.log -> .1, .1 -> .2, ...),
// drops the oldest chunk, and opens a fresh debug.log.
func (rw *rotatingWriter) rotate() error {
	if rw.file != nil {
		rw.file.Close()
		rw.file = nil
	}

	// Shift: debug.log -> debug.log.1, debug.log.1 -> debug.log.2, ...
	// The oldest (.maxFiles-1) is dropped.
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
			// Drop the oldest chunk.
			os.Remove(oldPath)
		} else {
			// Rename if exists — early on there may be no old chunks.
			if _, err := os.Stat(oldPath); err == nil {
				os.Rename(oldPath, newPath)
			}
		}
	}

	return rw.openCurrent()
}

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
