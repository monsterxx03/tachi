//go:build integration

package tui_test

// TUI integration suite (docs/2026-07-31-tui-integration-test.md §五/§七):
// in-process bubbletea driving a REAL tui.Model + REAL AIAgent wired to a
// mockllm server — no tmux, no TTY, no binary build. Every spec owns an
// isolated --home + mock port + tea.Program, so ginkgo -p parallelism is
// safe (each node is a separate process; specs run serially within a node,
// which is what makes the process-global config.SetBaseDir / working-dir
// setup sound).

import (
	"testing"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

func TestTUI(t *testing.T) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "itest/tui — in-process TUI (real Model + real AIAgent)")
}

// JustAfterEach dumps diagnostics when a scenario fails: the last screen
// text and the mock's request summary, so cross-layer regressions
// (rendering / agent loop / config) are debuggable at unit-test level.
var _ = ginkgo.JustAfterEach(func() {
	if !ginkgo.CurrentSpecReport().Failed() {
		return
	}
	ginkgo.GinkgoWriter.Printf("--- scenario failed: %s\n", ginkgo.CurrentSpecReport().FullText())

	// mock is created inside each scenario (DeferCleanup'd there), so the
	// request count is best-effort: zero when the failure happened before
	// the mock was registered. The scenario's own failure dump (session
	// Screen/Expect) carries the rich diagnostics.
})
