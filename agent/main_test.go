package agent

import (
	"os"
	"testing"

	"github.com/monsterxx03/tachi/config"
)

// TestMain points the config base dir at a throwaway directory for the whole package.
//
// A few tests reach product paths that resolve their own location from config.BaseDir() — a session
// being written out, a one-off transcript — without setting a base dir of their own. Those used to
// land in the developer's REAL ~/.tachi: running `go test ./...` on a machine in use mixed test
// artefacts into the sessions and records living there (that is what .tachi.md's "tests never write
// into ~/.tachi" is about).
//
// One point of truth per package beats annotating every such test, and the tests that DO set a base
// dir keep winning: they capture the current value first and restore it, which is now this directory
// rather than "unset" — so they no longer fall back to the real home when they finish either.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "tachi-agent-test-")
	if err != nil {
		panic("agent tests: no temp dir for the config base dir: " + err.Error())
	}
	previous := config.BaseDir()
	config.SetBaseDir(dir)

	code := m.Run()

	config.SetBaseDir(previous)
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
