//go:build integration

package headless_test

// A headless host with NO phone: the IM side alone keeps the process alive.
//
// This is the shape a server deployment wants — one process serving the bots and (when configured)
// the phone, instead of a `tachi channel` service and a headless host side by side over one
// ~/.tachi. The other half of the property (the phone still answers when a channel is ALSO running)
// is the ordinary mobile case, which host_test.go's shared sandbox already covers.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/itest/mockllm"
)

// newIMOnlySandbox is the shared sandbox minus the phone: a provider, a channel, and no
// `mobile:` block at all — so a host that starts here started for the channel's sake.
func newIMOnlySandbox(t *testing.T, providerURL string) *sandbox {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".tachi")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("sandbox: %v", err)
	}
	writeFile(t, filepath.Join(dir, "provider.mock.json"),
		`{"providers":[{"name":"mock","type":"openai","model":"mock-model",`+
			`"base_url":"`+providerURL+`/v1","api_key":"x","spec":{"context_window":128000}}]}`+"\n")
	writeFile(t, filepath.Join(dir, "config.yaml"),
		"provider: mock\ntitle_generation: false\nlanguage: zh\n"+
			"channel:\n  channels:\n    mock:\n      enabled: true\n")
	return &sandbox{home: home}
}

// channelsRunningRe is the line that says the IM side came up. It is ALSO the readiness signal: with
// no phone there is no listener to ping, so the log is the program's own answer to "are you up".
var channelsRunningRe = regexp.MustCompile(`IM channels running: \[mock\]`)

func startIMOnlyHost(t *testing.T, sb *sandbox) *host {
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
		if channelsRunningRe.MatchString(logs.String()) {
			return h
		}
		if cmd.ProcessState != nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("headless host never reported its channels; logs:\n%s", logs.String())
	return nil
}

func TestHostServesAnIMChannelWithoutThePhone(t *testing.T) {
	mock := mockllm.NewServer()
	defer mock.Close()

	h := startIMOnlyHost(t, newIMOnlySandbox(t, mock.URL()))

	// The host is up and running on the channel alone. Nothing bound a phone listener: the phone's
	// port must not exist in a config that never asked for one.
	if h.cmd.ProcessState != nil {
		t.Fatalf("host exited instead of serving the channel; logs:\n%s", h.logs.String())
	}
	if boundAddrRe.MatchString(h.logs.String()) {
		t.Errorf("a host with mobile disabled reported a phone listener:\n%s", h.logs.String())
	}
	if t.Failed() {
		return
	}

	// …and a stop really stops it, with the channel's goroutine and the process both ending.
	h.stop()
	if h.cmd.ProcessState == nil {
		t.Fatal("host is still running after SIGTERM")
	}
}
