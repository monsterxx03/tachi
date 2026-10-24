//go:build integration

package headless_test

// 无头宿主的端到端套件：**真二进制**（`-tags headless`）、**真 HTTP/WS**、**真一轮**（假模型）。
//
// 它补的是另外两层都够不着的地方：
//   - `desktop` 的单元测试跑在默认标签下，走的永远是有窗口的那条路；
//   - CI 的 `desktop-headless` job 只证明它能编。
//
// 这一层不需要 GUI，也不需要 cgo（构建就是 CGO_ENABLED=0），所以在 Linux 的 runner 上能跑。
// 每个用例有自己的 HOME，因此有自己的 ~/.tachi、自己的锁、自己的端口 —— 用例之间互不相干。

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const testToken = "itest-headless-token"

var (
	buildOnce sync.Once
	builtBin  string
	buildErr  error
)

// headlessBinary builds the no-window host once per test binary.
//
// GOWORK=off: desktop/ is a module of its own and is deliberately not in the workspace. CGO_ENABLED=0
// is not an optimization here — it is the property under test (a host that needs no gtk/webkit).
func headlessBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		if err != nil {
			buildErr = err
			return
		}
		dir, err := os.MkdirTemp("", "tachi-headless-itest-")
		if err != nil {
			buildErr = err
			return
		}
		out := filepath.Join(dir, "tachi-headless")
		cmd := exec.Command("go", "build", "-tags", "headless", "-trimpath", "-o", out, ".")
		cmd.Dir = filepath.Join(root, "desktop")
		cmd.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0")
		if b, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("build the headless host: %v\n%s", err, b)
			return
		}
		builtBin = out
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return builtBin
}

// sandbox is one isolated home: config.yaml + provider.mock.json + mobile.json.
type sandbox struct {
	home string
}

// newSandbox writes the three files an install has, pointing the provider at the mock.
//
// listen is 127.0.0.1:0 — the OS picks the port, the host logs the address it actually bound, and the
// test reads it back. A fixed port would collide when two cases run at once.
func newSandbox(t *testing.T, providerURL string) *sandbox {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".tachi")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("sandbox: %v", err)
	}
	provider := fmt.Sprintf(`{"providers":[{"name":"mock","type":"openai","model":"mock-model",`+
		`"base_url":"%s/v1","api_key":"x","spec":{"context_window":128000}}]}`+"\n", providerURL)
	writeFile(t, filepath.Join(dir, "provider.mock.json"), provider)
	writeFile(t, filepath.Join(dir, "config.yaml"),
		"provider: mock\ntitle_generation: false\nlanguage: zh\n"+
			"mobile:\n  enabled: true\n  listen: \"127.0.0.1:0\"\n")
	writeFile(t, filepath.Join(dir, "mobile.json"), `{"token":"`+testToken+`"}`)
	return &sandbox{home: home}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// host is one running headless process.
type host struct {
	cmd  *exec.Cmd
	addr string // host:port the listener bound
	logs *lockedBuffer
}

// lockedBuffer collects the process's stdout+stderr; the readiness wait reads it while the process
// writes to it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

var boundAddrRe = regexp.MustCompile(`phone interface on ([0-9]{1,3}(?:\.[0-9]{1,3}){3}:[0-9]+)`)

// startHost launches the binary against a sandbox and waits until it answers.
//
// The wait is on the LOG LINE, not on a ping alone: the line carries the port the OS chose, and
// asking for the address in one place (the program's own report) keeps the test from guessing.
func startHost(t *testing.T, sb *sandbox) *host {
	t.Helper()
	cmd := exec.Command(headlessBinary(t))
	cmd.Env = append(os.Environ(), "HOME="+sb.home)
	logs := &lockedBuffer{}
	cmd.Stdout = logs
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start headless host: %v", err)
	}
	h := &host{cmd: cmd, logs: logs}
	t.Cleanup(func() { h.stop() })

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if m := boundAddrRe.FindStringSubmatch(logs.String()); m != nil {
			h.addr = m[1]
			if h.ping() {
				return h
			}
		}
		// A process that already exited will never print anything else; say why now instead of
		// waiting out the whole budget.
		if cmd.ProcessState != nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("headless host never became ready; logs:\n%s", logs.String())
	return nil
}

func (h *host) stop() {
	if h.cmd.Process == nil {
		return
	}
	_ = h.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_ = h.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = h.cmd.Process.Kill()
		<-done
	}
}

func (h *host) url(path string) string { return "http://" + h.addr + path }

func (h *host) ping() bool {
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get(h.url("/v1/ping"))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// do sends one authenticated request and returns the status and body.
func (h *host) do(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	var r *strings.Reader
	if body == "" {
		r = strings.NewReader("")
	} else {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.url(path), r)
	if err != nil {
		t.Fatalf("request %s %s: %v", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body) //nolint:errcheck // the status is the assertion; a short body is still printed
	return resp.StatusCode, string(b)
}

// dialWS connects the phone's progress stream. gorilla, not the coder client: it is what the rest
// of the suite (and mockapi's own tests) already use, and it is a direct dependency of this module.
func (h *host) dialWS(t *testing.T) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.DefaultDialer.Dial("ws://"+h.addr+"/v1/ws?token="+testToken, nil)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
