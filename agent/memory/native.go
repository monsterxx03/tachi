package memory

import (
	"context"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// NativeBackend stores memory locally in a plain-text index file.
// It only writes at StoreScopeSession (one-line summary per session),
// and Recall is a no-op — the LLM uses GrepTool to search session transcripts.
type NativeBackend struct {
	logPath string
	mu      sync.Mutex
}

// NewNativeBackend creates a NativeBackend using the given config.
// Memory data is stored under BaseDir/memory/.
func NewNativeBackend(cfg Config) (*NativeBackend, error) {
	memDir := filepath.Join(cfg.BaseDir, "memory")
	if err := os.MkdirAll(memDir, 0700); err != nil {
		return nil, fmt.Errorf("memory: create directory %s: %w", memDir, err)
	}
	return &NativeBackend{
		logPath: filepath.Join(memDir, "log"),
	}, nil
}

// Store writes memory. NativeBackend only responds to StoreScopeSession —
// it writes a one-line summary to memory/log. Turn and compact scopes are
// ignored because native doesn't need frequent index updates.
func (b *NativeBackend) Store(ctx context.Context, opts StoreOptions) error {
	if opts.Scope != StoreScopeSession {
		return nil
	}
	if opts.SessionTitle == "" {
		return nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	f, err := os.OpenFile(b.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	ts := time.Now()
	id := genEntryID(ts, opts.SessionTitle)
	tags := ""
	if len(opts.Tags) > 0 {
		tags = " | tags: " + strings.Join(opts.Tags, ", ")
	}
	line := fmt.Sprintf("%s | %s | %s%s\n",
		id,
		ts.Format("2006-01-02 15:04"),
		opts.SessionTitle, tags)
	_, err = f.WriteString(line)
	return err
}

// Recall is a no-op. The LLM has GrepTool and can search session transcripts
// directly — it understands user semantics better than any programmatic
// keyword extraction.
//
// Recall chain:
//
//	MemoryRecallReminder injects recent 20 index entries ("what we've talked about")
//	→ LLM decides what to search based on user's question
//	→ LLM calls GrepTool("API timeout", "~/.tachi/session/")
//	→ hits relevant transcripts, reads them, answers
func (b *NativeBackend) Recall(ctx context.Context, query string, limit int) ([]Entry, error) {
	return nil, nil
}

// Forget removes a memory entry by its CRC32 ID from memory/log.
func (b *NativeBackend) Forget(ctx context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	lines, err := readLines(b.logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no memories found")
		}
		return err
	}

	filtered := make([]string, 0, len(lines))
	found := false
	prefix := id + " | "
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			found = true
			continue
		}
		filtered = append(filtered, line)
	}
	if !found {
		return fmt.Errorf("memory entry not found: %s", id)
	}

	tmpPath := b.logPath + ".tmp"
	if err := writeLines(tmpPath, filtered); err != nil {
		return err
	}
	return os.Rename(tmpPath, b.logPath)
}

// genEntryID generates an 8-char hex ID from timestamp + title CRC32.
func genEntryID(ts time.Time, title string) string {
	h := crc32.NewIEEE()
	h.Write([]byte(ts.Format(time.RFC3339)))
	h.Write([]byte(title))
	return fmt.Sprintf("%08x", h.Sum32())[:8]
}

// readLines reads all lines from a file.
func readLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	content := strings.TrimRight(string(data), "\n")
	if content == "" {
		return nil, nil
	}
	return strings.Split(content, "\n"), nil
}

// writeLines writes lines to a file, each followed by \n.
func writeLines(path string, lines []string) error {
	data := strings.Join(lines, "\n") + "\n"
	return os.WriteFile(path, []byte(data), 0644)
}