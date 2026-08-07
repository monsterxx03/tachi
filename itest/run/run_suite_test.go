//go:build integration

package run_test

import (
	"testing"

	"github.com/monsterxx03/tachi/itest/harness"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

// bin is the real tachi binary, built once per suite.
var bin string

func TestRun(t *testing.T) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "itest/run — -p pipe mode (real binary)")
}

var _ = ginkgo.BeforeSuite(func() {
	bin = harness.BuildBinary(ginkgo.GinkgoT())
})

// JustAfterEach dumps diagnostics when a scenario fails: the mock's request
// summary and recorded error, so agent-loop regressions are debuggable at
// unit-test level (docs §七).
var _ = ginkgo.JustAfterEach(func() {
	if !ginkgo.CurrentSpecReport().Failed() {
		return
	}
	ginkgo.GinkgoWriter.Printf("--- scenario failed: %s\n", ginkgo.CurrentSpecReport().FullText())
})
