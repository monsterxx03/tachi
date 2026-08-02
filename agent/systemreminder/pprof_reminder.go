package systemreminder

import (
	"context"
	"fmt"
	"sync"
)

// PprofReminder surfaces the pprof debug server to the LLM exactly once per
// agent instance. It deliberately lives in the dynamic reminder layer rather
// than the static system prompt: the actual bound port is only known after
// bootstrap (startPprof auto-increments when the configured port is taken),
// so the value is stale by the time a system prompt is built. A fresh agent
// after a restart picks up the new port via the reminder.
type PprofReminder struct {
	Enabled bool
	Port    int
	PID     int

	mu   sync.Mutex
	sent bool
}

// Generate returns the pprof hint on the first call per instance, and nothing
// afterwards — one shot, so the port + PID aren't repeated on every message.
func (r *PprofReminder) Generate(_ context.Context, _ Context) []string {
	if !r.Enabled || r.Port == 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sent {
		return nil
	}
	r.sent = true
	addr := fmt.Sprintf("127.0.0.1:%d", r.Port)
	return []string{fmt.Sprintf(`- Pprof debug server: http://%s/debug/pprof/ (Tachi PID %d) — if the user asks you to debug Tachi's own performance issues (CPU, memory, goroutines), you can use Bash to run: go tool pprof http://%s/debug/pprof/profile?seconds=30 (CPU), go tool pprof http://%s/debug/pprof/heap (memory), or curl http://%s/debug/pprof/goroutine?debug=2 (goroutine dump)`, addr, r.PID, addr, addr, addr)}
}
