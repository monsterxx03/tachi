//go:build integration

package tui_test

// Shared scenario helpers (doc §六/§七): a protocol-specific mock + isolated
// --home, and a launched TUI session on top of them. Each spec owns its
// mock port + home + tea.Program, so ginkgo -p parallelism is safe.

import (
	"os"
	"path/filepath"
	"time"

	"github.com/monsterxx03/tachi/itest/harness"
	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/monsterxx03/tachi/itest/tui"
	"github.com/onsi/ginkgo/v2"
)

// startMock starts a protocol-specific mock server wired into an isolated
// --home whose config fixture follows the same protocol (provider type +
// base_url). Registered for cleanup; returned mock/home belong to one spec.
// opts carry scenario-level config variants (WithBashAllow, WithBashAsk,
// WithSecondProvider, WithSpecTimeout).
func startMock(p mockllm.Protocol, steps []mockllm.Step, opts ...harness.Option) (*mockllm.Server, string) {
	mock := mockllm.NewServer(mockllm.WithProtocol(p))
	ginkgo.DeferCleanup(mock.Close)
	mock.Script(steps...)
	home := harness.NewHome(ginkgo.GinkgoT(), mock,
		append([]harness.Option{harness.WithProtocol(p)}, opts...)...)
	return mock, home
}

// launch starts an in-process TUI session on the given home+mock. The spec
// owns the returned session; teardown (program quit + agent close) is wired
// by the driver's Launch cleanup.
func launch(home string, mock *mockllm.Server, opts ...tui.Option) *tui.Session {
	return tui.Launch(ginkgo.GinkgoT(), home, mock, opts...)
}

// specTimeout bounds every scenario — a wedged program (deadlock, event
// starvation) must fail the spec, never hang the suite.
const specTimeout = 60 * time.Second

// providerInfoFor returns the statusbar's provider display for a wire
// protocol: Provider().Name() is the provider TYPE (openai / anthropic /
// openai-res), Model() is the fixture's model.
func providerInfoFor(p mockllm.Protocol) string {
	switch p {
	case mockllm.ProtocolAnthropic:
		return "anthropic (mock-model)"
	case mockllm.ProtocolOpenAIResponses:
		return "openai-res (mock-model)"
	default:
		return "openai (mock-model)"
	}
}

// sessionMessageFiles returns the home's persisted session message files
// (home/session/<id>/messages.jsonl), newest first — the doc's "session
// 文件生成" assertion surface.
func sessionMessageFiles(home string) []string {
	dir := filepath.Join(home, "session")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name(), "messages.jsonl")
		if _, err := os.Stat(path); err == nil {
			out = append(out, path)
		}
	}
	return out
}
