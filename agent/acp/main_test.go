package acp

import (
	"os"
	"testing"

	"github.com/monsterxx03/tachi/config"
)

// TestMain points the process-global tachi home at a throwaway directory for the whole test binary.
//
// The individual tests that build a session store set a base dir of their own, but the ones that do
// not used to fall back to the developer's REAL ~/.tachi — running `go test ./agent/acp/` with a
// throwaway $HOME left a `.tachi/session` behind, which is what .tachi.md's "tests never write into
// ~/.tachi" forbids. Same shape as agent/tools's TestMain, and the same reason.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "tachi-acp-test-home-")
	if err != nil {
		panic("cannot create the test home: " + err.Error())
	}
	previous := config.BaseDir()
	config.SetBaseDir(dir)

	code := m.Run()

	config.SetBaseDir(previous)
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
