package hooks

import (
	"os"
	"testing"

	"github.com/monsterxx03/tachi/config"
)

// TestMain points the process-global tachi home at a throwaway directory for the whole test binary,
// and restores whatever was there before on the way out.
//
// Same reason as agent/tools's TestMain: any test that reaches a path resolving itself from
// config.BaseDir() (a store, a cache, a record) would otherwise write into the developer's REAL
// ~/.tachi. Tests write to t.TempDir() only — this is the default that makes that true even for the
// ones that never mention a base dir.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "tachi-hooks-test-home-")
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
