//go:build !darwin

package main

import (
	"os"
	"strings"
	"testing"
)

// TestEnvWithDropsParentCopies pins the one property the sandbox cannot get wrong: an overridden
// key must appear EXACTLY ONCE in the child's environment.
//
// A duplicate is worse than a tie — Go resolves last-wins, glib and the GTK/WebKit stack
// first-wins — so whichever order the two copies are in, one of the two readers gets the parent's
// HOME. That is the whole app reading the user's real ~/.tachi while the suite reports UI
// failures: it created sessions in a real store and dragged the real sidebar before anyone
// noticed. The names this function is given are exactly the ones the app itself reads.
func TestEnvWithDropsParentCopies(t *testing.T) {
	t.Setenv("TACHI_SMOKE_ENV_TEST", "parent")

	env := envWith(map[string]string{"TACHI_SMOKE_ENV_TEST": "sandbox"})

	var got []string
	for _, kv := range env {
		if strings.HasPrefix(kv, "TACHI_SMOKE_ENV_TEST=") {
			got = append(got, kv)
		}
	}
	if len(got) != 1 || got[0] != "TACHI_SMOKE_ENV_TEST=sandbox" {
		t.Fatalf("overridden key must appear exactly once with the sandbox value, got %v", got)
	}
}

// TestEnvWithKeepsTheRest: everything else the parent had must survive. The app needs PATH (it
// shells out), DISPLAY/WAYLAND_DISPLAY (there is a window to map), and so on.
func TestEnvWithKeepsTheRest(t *testing.T) {
	t.Setenv("TACHI_SMOKE_ENV_KEEP", "kept")

	env := envWith(map[string]string{"TACHI_SMOKE_ENV_TEST": "sandbox"})

	found := false
	for _, kv := range env {
		if kv == "TACHI_SMOKE_ENV_KEEP=kept" {
			found = true
		}
	}
	if !found {
		t.Fatal("a variable the sandbox does not override must be passed through")
	}
	if len(env) < len(os.Environ()) {
		t.Fatalf("child env has %d entries, parent has %d — entries were dropped", len(env), len(os.Environ()))
	}
}
