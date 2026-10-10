package node

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The helper-process pattern: this test binary re-executes ITSELF with an env var
// set, so the cases a single process cannot reach are covered by a real second one.
// There are exactly two such cases, and they are the two that matter for a
// single-instance lock:
//
//   - flock excludes ACROSS processes, not just across descriptors in one;
//   - the OS releases the lock when a process dies without running any cleanup.
//
// Everything else about the node is verified in-process, where a failing test can
// point at a stack instead of a killed child.
const (
	helperModeEnv = "TACHI_NODE_TEST_HELPER" // "host" | "attach"
	helperDirEnv  = "TACHI_NODE_TEST_DIR"
	helperTimeout = 20 * time.Second
)

func TestMain(m *testing.M) {
	switch os.Getenv(helperModeEnv) {
	case "host":
		helperHost()
	case "attach":
		helperAttach()
	default:
		os.Exit(m.Run())
	}
}

// helperHost takes the lock, announces what it published, then holds on until its
// stdin closes — which is how the parent ends it cleanly (or kills it outright).
func helperHost() {
	handle, err := StartOrAttach(context.Background(), helperOptions())
	if err != nil {
		fmt.Printf("ERROR %v\n", err)
		os.Exit(3)
	}
	fmt.Printf("HOSTING instance=%s addr=%s\n", handle.Client().InstanceID(), handle.Addr())
	_, _ = io.Copy(io.Discard, os.Stdin)
	_ = handle.Close()
	os.Exit(0)
}

// helperAttach joins the host that is already running, once, and reports what it got.
func helperAttach() {
	handle, err := StartOrAttach(context.Background(), helperOptions())
	if err != nil {
		fmt.Printf("ERROR %v\n", err)
		os.Exit(3)
	}
	fmt.Printf("ATTACHED hosting=%t instance=%s node=%s\n",
		handle.Hosting(), handle.Client().InstanceID(), handle.NodeID())
	_ = handle.Close()
	os.Exit(0)
}

func helperOptions() Options {
	return Options{
		DataDir:     os.Getenv(helperDirEnv),
		LocalListen: DefaultLocalListen,
	}
}

// helper is a running second process plus the one line it announced.
type helper struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner
}

func startHelper(t *testing.T, mode, dir string) *helper {
	t.Helper()

	cmd := exec.Command(os.Args[0]) // this test binary, re-executed
	cmd.Env = append(os.Environ(), helperModeEnv+"="+mode, helperDirEnv+"="+dir)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	cmd.Stderr = os.Stderr // a helper that fails should say why, in the test's own output
	require.NoError(t, cmd.Start())

	h := &helper{cmd: cmd, stdin: stdin, stdout: bufio.NewScanner(stdout)}
	t.Cleanup(func() {
		// Closing stdin is the polite ending; Kill covers the tests that already
		// shot the process, and Wait reaps whichever way it went.
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return h
}

// line waits for the helper's announcement. A helper that dies first is reported as
// what it printed (its stderr is the test's own), not silently as an empty line.
func (h *helper) line(t *testing.T) string {
	t.Helper()

	type scanResult struct {
		line string
		err  error
	}
	ch := make(chan scanResult, 1)
	go func() {
		if h.stdout.Scan() {
			ch <- scanResult{line: h.stdout.Text()}
			return
		}
		ch <- scanResult{err: h.stdout.Err()}
	}()

	select {
	case res := <-ch:
		require.NotEmpty(t, res.line, "helper exited without announcing itself (err=%v)", res.err)
		require.NotContains(t, res.line, "ERROR", "helper failed: %s", res.line)
		return res.line
	case <-time.After(helperTimeout):
		t.Fatalf("helper produced nothing within %s", helperTimeout)
		return ""
	}
}

// kill ends the helper the way a crash does: SIGKILL, no cleanup on its side, and a
// Wait that necessarily reports the signal that killed it.
func (h *helper) kill() {
	_ = h.cmd.Process.Kill()
	_ = h.cmd.Wait()
}

func field(t *testing.T, line, key string) string {
	t.Helper()
	for _, f := range strings.Fields(line) {
		if v, ok := strings.CutPrefix(f, key+"="); ok {
			return v
		}
	}
	t.Fatalf("helper line %q has no %s=", line, key)
	return ""
}

func TestSecondProcessAttachesToTheHostingProcess(t *testing.T) {
	dir := t.TempDir()

	child := startHelper(t, "host", dir)
	hostedInstance := field(t, child.line(t), "instance")

	// This process cannot take the lock, so it must join the one the child holds.
	attached, err := StartOrAttach(context.Background(), testOptions(dir))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, attached.Close()) })

	require.False(t, attached.Hosting())
	require.Equal(t, hostedInstance, attached.Client().InstanceID())
	require.NoError(t, attached.Client().Ping(context.Background()))
}

func TestKilledHostReleasesTheLockToTheNextProcess(t *testing.T) {
	dir := t.TempDir()

	child := startHelper(t, "host", dir)
	firstInstance := field(t, child.line(t), "instance")

	// Prove the host is really hosting before killing it.
	attached, err := StartOrAttach(context.Background(), testOptions(dir))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, attached.Close()) })
	require.False(t, attached.Hosting())

	// SIGKILL: no defers, no Close — only the OS can release the lock now. This is
	// the crash case the whole single-instance design has to survive.
	child.kill()

	next, err := StartOrAttach(context.Background(), testOptions(dir))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, next.Close()) })

	require.True(t, next.Hosting(), "a dead holder must not keep the node locked")
	require.NotEqual(t, firstInstance, next.Client().InstanceID(),
		"the new host is a new instance, and the instance id is what says so")
	require.NoError(t, next.Client().Ping(context.Background()))
}

func TestSecondProcessCanAttachToThisOne(t *testing.T) {
	dir := t.TempDir()

	host, err := StartOrAttach(context.Background(), testOptions(dir))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, host.Close()) })
	require.True(t, host.Hosting())

	child := startHelper(t, "attach", dir)
	line := child.line(t)

	require.Equal(t, "false", field(t, line, "hosting"))
	require.Equal(t, host.Client().InstanceID(), field(t, line, "instance"))
	require.Equal(t, host.NodeID(), field(t, line, "node"))
}
