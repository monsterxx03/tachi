package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/monsterxx03/tachi/config"
)

// readDesktopStatus decodes what writeDesktopStatus left on disk, as a consumer would — the
// raw map, because the point of some of these cases is which keys are ABSENT.
func readDesktopStatus(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile(desktopStatusPath())
	if err != nil {
		t.Fatalf("read %s: %v", desktopStatusPath(), err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode %s: %v", desktopStatusPath(), err)
	}
	return got
}

// The status file is a bar widget's only source, so what it says (and what it refuses to say)
// is a contract. The trap it guards against is the zero: the tray omits an unknown cost and an
// unmeasured context instead of drawing "¥0" or "0%", and a file that always carried both
// would put those two false claims back into every other surface.
func TestDesktopStatusOmitsUnknownNumbers(t *testing.T) {
	restoreBase(t, t.TempDir())
	st := AgentState{Status: StatusThinking, Label: "思考", Detail: "正在读文件"}

	writeDesktopStatus(st, 0, -1)
	got := readDesktopStatus(t)
	for _, key := range []string{"cost", "contextPercent"} {
		if _, present := got[key]; present {
			t.Errorf("%s present in %v, want it omitted while unknown", key, got)
		}
	}
	if got["status"] != string(StatusThinking) || got["label"] != "思考" || got["detail"] != "正在读文件" {
		t.Errorf("state fields = %v, want the AgentState's own", got)
	}
	if got["updatedAt"] == nil || got["updatedAt"] == "" {
		t.Errorf("updatedAt missing in %v", got)
	}
}

// 0% is a READING, not an absence: a session that just started has an empty context, and the
// tray draws "0%" for it. Only "not measured" is allowed to disappear — which is why the field
// has to be a pointer, and why this test would fail on a plain int with omitempty.
func TestDesktopStatusKeepsZeroPercent(t *testing.T) {
	restoreBase(t, t.TempDir())

	writeDesktopStatus(AgentState{Status: StatusIdle, Label: "空闲"}, 5.694, 0)
	got := readDesktopStatus(t)
	if percent, ok := got["contextPercent"]; !ok || percent != float64(0) {
		t.Errorf("contextPercent = %v (present %v), want 0", percent, ok)
	}
	if cost := got["cost"]; cost != 5.694 {
		t.Errorf("cost = %v, want 5.694", cost)
	}
}

// Shutting down withdraws the readout; a consumer that ignores it keeps drawing a process that
// is gone. Removing a file that is not there (a shutdown before the first repaint) is not an
// error.
func TestRemoveDesktopStatus(t *testing.T) {
	restoreBase(t, t.TempDir())
	removeDesktopStatus() // must not complain about a missing file

	writeDesktopStatus(AgentState{Status: StatusIdle, Label: "空闲"}, 1, 10)
	removeDesktopStatus()
	if _, err := os.Stat(desktopStatusPath()); !os.IsNotExist(err) {
		t.Errorf("%s still there after remove (err = %v)", desktopStatusPath(), err)
	}
}

// Every repaint replaces the file rather than writing through it, so a consumer can never read
// half a payload — and the file lands in the tachi home next to the UI state, where a widget
// can find it without being told where Tachi keeps things.
func TestDesktopStatusPathIsInTheTachiHome(t *testing.T) {
	dir := t.TempDir()
	restoreBase(t, dir)

	if got, want := desktopStatusPath(), filepath.Join(dir, desktopStatusFile); got != want {
		t.Errorf("desktopStatusPath() = %q, want %q", got, want)
	}
	if config.BaseDir() != dir {
		t.Fatalf("test harness pointed BaseDir elsewhere: %q", config.BaseDir())
	}
}
