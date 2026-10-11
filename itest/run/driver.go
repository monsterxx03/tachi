//go:build integration

// Package run drives the real tachi binary through its -p (pipe) mode:
// exec.Command + pipes, asserting on the NDJSON AgentEvent stream, exit
// codes, and the mock server's request log. This is the only layer that
// exercises the true process boundary (main.go flag parsing, config loading,
// --allowed-tools).
package run

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Result captures one binary invocation.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Err      error // non-ExitError failures (binary not found, ...)
}

// Binary runs the tachi binary with the given args. workdir becomes the
// process working directory (tool side effects land there). Pass
// "--home", home as part of args for state isolation.
func Binary(bin, workdir string, args ...string) Result {
	cmd := exec.Command(bin, args...)
	if workdir != "" {
		cmd.Dir = workdir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if err == nil {
		res.ExitCode = 0
		return res
	}
	if ee, ok := err.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
		return res
	}
	res.Err = err
	return res
}

// Daemon is a tachi process left RUNNING — what a HOSTING entry needs, since its node service
// lives exactly as long as the process does. Binary cannot serve this: it waits for the process
// to finish, and a host that finishes is a host that is gone.
//
// The output is collected as the process writes it rather than at the end, because the reason to
// read it is a failure — which is exactly when the process is still going.
type Daemon struct {
	cmd    *exec.Cmd
	stdout syncBuffer
	stderr syncBuffer
	done   chan struct{}
	err    error
}

// syncBuffer is a bytes.Buffer that a reader may touch while the process writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Spawn starts the binary and returns at once. The caller OWNS the process: without a Stop (or a
// ginkgo DeferCleanup that calls one) a spec leaves a running tachi behind, and a leftover host
// holding the node lock makes the next spec of this worker fail for a reason that has nothing to
// do with it.
func Spawn(bin, workdir string, args ...string) (*Daemon, error) {
	cmd := exec.Command(bin, args...)
	if workdir != "" {
		cmd.Dir = workdir
	}
	d := &Daemon{cmd: cmd, done: make(chan struct{})}
	cmd.Stdout = &d.stdout
	cmd.Stderr = &d.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		d.err = cmd.Wait()
		close(d.done)
	}()
	return d, nil
}

// Stdout/Stderr are what the process has said so far.
func (d *Daemon) Stdout() string { return d.stdout.String() }
func (d *Daemon) Stderr() string { return d.stderr.String() }

// PID is this process's id. It matters when two processes share a --home: the log file does too,
// and every line the node writes carries `pid=`, so this is what tells their lines apart.
func (d *Daemon) PID() int {
	if d.cmd.Process == nil {
		return 0
	}
	return d.cmd.Process.Pid
}

// Kill ends the process NOW, without asking it to shut down: the point of a spec that kills a host
// mid-task is that the host did not get to tidy up (design §9.3 is about what the NEXT start makes
// of what the dead one left behind).
func (d *Daemon) Kill() error {
	if exited, _ := d.Exited(); exited {
		return nil
	}
	if err := d.cmd.Process.Kill(); err != nil {
		return err
	}
	<-d.done
	return nil
}

// Exited reports whether the process is already gone, and why. A host that exited on its own is
// a fact the spec has to be able to CHECK: the alternative is a readiness wait that times out and
// reports "never became ready" while the real answer sits in this process's output.
func (d *Daemon) Exited() (bool, error) {
	select {
	case <-d.done:
		return true, d.err
	default:
		return false, nil
	}
}

// Stop asks the process to exit (SIGTERM, which is what a shutdown looks like on the platforms
// this suite runs on) and waits for it. A process that will not go is killed: a spec that ends
// with a live host is worse than a spec that reports a hard stop.
func (d *Daemon) Stop() error {
	if exited, err := d.Exited(); exited {
		return err
	}
	_ = d.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-d.done:
		return nil
	case <-time.After(daemonStopTimeout):
		_ = d.cmd.Process.Kill()
		<-d.done
		return fmt.Errorf("tachi did not stop within %s; killed it", daemonStopTimeout)
	}
}

const daemonStopTimeout = 5 * time.Second

// Event mirrors main.go's streamEvent NDJSON line.
type Event struct {
	Type       string `json:"type"`
	Content    string `json:"content,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	ToolArgs   string `json:"tool_args,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	ToolResult string `json:"tool_result,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	IsError    bool   `json:"is_error,omitempty"`
	ExitReason string `json:"exit_reason,omitempty"`
	Iterations int    `json:"iterations_used,omitempty"`
	Usage      *Usage `json:"usage,omitempty"`
	Error      string `json:"error,omitempty"`
}

// Usage mirrors main.go's usageJSON.
type Usage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// ParseNDJSON decodes the -o json-stream output into events, ignoring blank
// lines. Malformed lines fail the parse loudly (a format drift in the
// binary's output should never pass silently).
func ParseNDJSON(s string) []Event {
	var out []Event
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			panic("run: failed to parse NDJSON event line: " + err.Error() + "\nline: " + line)
		}
		out = append(out, ev)
	}
	return out
}

// TextDelta accumulates the streamed text from text_delta events — the
// -o json-stream equivalent of the human-readable final answer.
func TextDelta(events []Event) string {
	var sb strings.Builder
	for _, ev := range events {
		if ev.Type == "text_delta" {
			sb.WriteString(ev.Content)
		}
	}
	return sb.String()
}
