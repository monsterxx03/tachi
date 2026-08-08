//go:build integration

package acp_test

// ACP integration suite: the REAL tachi binary in `acp` mode driven by the
// REAL ACP client SDK (acp-go-sdk ClientSideConnection) over true stdio
// JSON-RPC. Scenarios start an isolated --home wired to a mockllm server,
// open ACP sessions, and assert on the streamed SessionUpdate sequence —
// the same "same scenario, all wire formats behave identically" table shape
// as itest/run, now for the editor-facing protocol.

import (
	"testing"

	"github.com/monsterxx03/tachi/itest/harness"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

// bin is the real tachi binary, built once per suite.
var bin string

func TestACP(t *testing.T) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "itest/acp — ACP agent (real binary + real client SDK)")
}

var _ = ginkgo.BeforeSuite(func() {
	bin = harness.BuildBinary(ginkgo.GinkgoT())
})

// JustAfterEach dumps diagnostics when a scenario fails: the agent's stderr
// (logs) so protocol regressions are debuggable at the wire level.
var _ = ginkgo.JustAfterEach(func() {
	if !ginkgo.CurrentSpecReport().Failed() {
		return
	}
	ginkgo.GinkgoWriter.Printf("--- scenario failed: %s\n", ginkgo.CurrentSpecReport().FullText())
})
