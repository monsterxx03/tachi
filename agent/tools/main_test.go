package tools

import (
	"os"
	"testing"

	"github.com/monsterxx03/tachi/config"
)

// TestMain points the process-global tachi home at a throwaway directory for the whole test
// binary, and removes it afterwards.
//
// It is not housekeeping: the bash tool installs its askpass helper under
// config.BaseDir()/bin on the first command it runs, so ANY test that executes a command —
// including the ones that never mention the helper — would otherwise write a file into the
// real ~/.tachi. Tests write to t.TempDir() only.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "tachi-tools-test-home-")
	if err != nil {
		panic("cannot create the test home: " + err.Error())
	}
	config.SetBaseDir(dir)

	code := m.Run()

	config.SetBaseDir("")
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
