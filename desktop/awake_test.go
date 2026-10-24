package main

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/config"
)

// fakeSleep stands in for the operating system: it records what was acquired and lets a test fire
// the deadline by hand. Without it these tests would hold a REAL assertion — the machine running
// them would stay awake for three hours, which is exactly what the feature does to the user's.
type fakeSleep struct {
	acquired []*fakeGrant
	err      error

	deadline time.Duration
	fire     func()
	timers   []*time.Timer
}

func (f *fakeSleep) acquire(string) (sleepGrant, error) {
	if f.err != nil {
		return nil, f.err
	}
	g := &fakeGrant{}
	f.acquired = append(f.acquired, g)
	return g, nil
}

func (f *fakeSleep) after(d time.Duration, fn func()) *time.Timer {
	f.deadline = d
	f.fire = fn
	// A real timer that will never fire in a test: the deadline is driven by calling fire.
	t := time.AfterFunc(time.Hour, func() {})
	f.timers = append(f.timers, t)
	return t
}

type fakeGrant struct{ released bool }

func (g *fakeGrant) release() { g.released = true }

// newAwakeTestController builds a controller on the fake platform seam, on a temporary HOME (the
// preference is read from desktop_ui.json).
func newAwakeTestController(t *testing.T) (*awakeController, *fakeSleep, *[]awakeState) {
	t.Helper()
	config.SetBaseDir(t.TempDir())
	fake := &fakeSleep{}
	var changes []awakeState
	c := newAwakeController(func(st awakeState) { changes = append(changes, st) })
	c.acquire = fake.acquire
	c.after = fake.after
	t.Cleanup(func() {
		for _, tm := range fake.timers {
			tm.Stop()
		}
	})
	return c, fake, &changes
}

// TestAwakeDefaultsToThreeHours: a machine that never touched the switch starts OFF, and turning it
// on lasts the default 3 hours.
func TestAwakeDefaultsToThreeHours(t *testing.T) {
	c, fake, _ := newAwakeTestController(t)

	st := c.state()
	if st.On {
		t.Fatal("a fresh controller must start off: nothing may hold the machine awake at launch")
	}
	if st.DurationMin != awakeDefaultMinutes {
		t.Fatalf("durationMin = %d, want the 3-hour default (%d)", st.DurationMin, awakeDefaultMinutes)
	}

	st, err := c.set(true, nil)
	if err != nil {
		t.Fatalf("turning on: %v", err)
	}
	if !st.On || len(fake.acquired) != 1 {
		t.Fatalf("state = %+v, acquisitions = %d; want on with exactly one assertion", st, len(fake.acquired))
	}
	if fake.deadline != 3*time.Hour {
		t.Fatalf("deadline = %v, want 3h", fake.deadline)
	}
	until, err := time.Parse(time.RFC3339, st.Until)
	if err != nil {
		t.Fatalf("until %q is not RFC3339: %v", st.Until, err)
	}
	if d := time.Until(until); d < 179*time.Minute || d > 181*time.Minute {
		t.Fatalf("until is %v away, want ~3 hours", d)
	}
}

// TestAwakeOffReleases: turning it off gives the assertion back — that is the whole feature.
func TestAwakeOffReleases(t *testing.T) {
	c, fake, _ := newAwakeTestController(t)
	if _, err := c.set(true, nil); err != nil {
		t.Fatalf("on: %v", err)
	}
	st, err := c.set(false, nil)
	if err != nil {
		t.Fatalf("off: %v", err)
	}
	if st.On || st.Until != "" {
		t.Fatalf("state after off = %+v, want off with no deadline", st)
	}
	if !fake.acquired[0].released {
		t.Fatal("the assertion was not released: the machine would stay awake with the switch off")
	}
}

// TestAwakeExpiresOnItsOwn: the deadline is the only way the switch turns itself off, and it has to
// release the assertion AND tell every screen (the menu's label and the phone's card both say "on").
func TestAwakeExpiresOnItsOwn(t *testing.T) {
	c, fake, changes := newAwakeTestController(t)
	if _, err := c.set(true, nil); err != nil {
		t.Fatalf("on: %v", err)
	}
	if fake.fire == nil {
		t.Fatal("no deadline was scheduled")
	}
	fake.fire()

	st := c.state()
	if st.On || st.Until != "" {
		t.Fatalf("state after the deadline = %+v, want off", st)
	}
	if !fake.acquired[0].released {
		t.Fatal("the deadline did not release the assertion")
	}
	if n := len(*changes); n != 2 {
		t.Fatalf("change callbacks = %d, want 2 (on, then the deadline)", n)
	}
}

// TestAwakeStaleDeadlineDoesNothing: changing the length while it is on schedules a second deadline,
// and the FIRST one must not release the live assertion. A stale timer that did would turn the
// switch off minutes after the user extended it — the worst kind of silence.
func TestAwakeStaleDeadlineDoesNothing(t *testing.T) {
	c, fake, _ := newAwakeTestController(t)
	if _, err := c.set(true, nil); err != nil {
		t.Fatalf("on: %v", err)
	}
	stale := fake.fire

	m := 60
	if _, err := c.set(true, &m); err != nil {
		t.Fatalf("on with 60: %v", err)
	}
	if fake.deadline != time.Hour {
		t.Fatalf("deadline = %v, want 1h", fake.deadline)
	}
	stale()

	if st := c.state(); !st.On {
		t.Fatal("a stale deadline turned the switch off")
	}
	if fake.acquired[0].released {
		t.Fatal("a stale deadline released the live assertion")
	}
}

// TestAwakeRejectsANonsenseDuration: the length comes from two screens and a hand-editable file, so
// a value no timer could honour is refused rather than stored.
func TestAwakeRejectsANonsenseDuration(t *testing.T) {
	c, _, _ := newAwakeTestController(t)
	for _, m := range []int{1, -5, awakeMaxMinutes + 1} {
		if _, err := c.set(true, &m); !errors.Is(err, errAwakeDuration) {
			t.Fatalf("set(on, %d) = %v, want errAwakeDuration", m, err)
		}
		if st := c.state(); st.On {
			t.Fatalf("set(on, %d) left the switch on", m)
		}
	}
	// 不自动关 is a real choice, not nonsense: it counts as a valid length.
	zero := 0
	if st, err := c.set(true, &zero); err != nil || !st.On || st.Until != "" {
		t.Fatalf("set(on, 0) = %+v, %v; want on with no deadline", st, err)
	}
}

// TestAwakePreferenceSurvives: the length is a preference, so it outlives the session — and it is
// NOT the same thing as "on", which is always off at launch.
func TestAwakePreferenceSurvives(t *testing.T) {
	c, _, _ := newAwakeTestController(t)
	m := 90
	if _, err := c.set(true, &m); err != nil {
		t.Fatalf("on: %v", err)
	}
	if _, err := c.set(false, nil); err != nil {
		t.Fatalf("off: %v", err)
	}

	next := newAwakeController(nil) // same HOME, new process
	st := next.state()
	if st.On {
		t.Fatal("a new controller must start off")
	}
	if st.DurationMin != 90 {
		t.Fatalf("durationMin = %d, want the stored 90", st.DurationMin)
	}
}

// TestAwakeRefusedStaysOff: when the operating system will not hold the assertion, the switch reads
// OFF. A switch that says "保持唤醒" while nothing holds the machine up is the one lie this feature
// must not tell.
func TestAwakeRefusedStaysOff(t *testing.T) {
	c, fake, _ := newAwakeTestController(t)
	fake.err = errors.New("logind says no")

	st, err := c.set(true, nil)
	if err == nil {
		t.Fatal("a refused assertion must be reported")
	}
	if st.On {
		t.Fatalf("state = %+v after a refusal, want off", st)
	}
}

// ── 手机那半边 ──

// TestMobileAwakeRoundTrip: the phone reads and writes the same switch the tray menu does, and the
// write reaches every other screen — 双端同步 is the whole point of putting it on the wire.
func TestMobileAwakeRoundTrip(t *testing.T) {
	d, _ := newMobileApp(t)
	m, ts := mobileTestServer(t, d)
	// The platform seam is faked: this test must not hold the machine it runs on awake.
	fake := &fakeSleep{}
	d.awake.acquire = fake.acquire
	d.awake.after = fake.after
	t.Cleanup(func() {
		for _, tm := range fake.timers {
			tm.Stop()
		}
	})

	c := newMobileConn(m, nil)
	c.out = make(chan []byte, 8)
	m.mu.Lock()
	m.conns[c] = struct{}{}
	m.mu.Unlock()

	code, body := mobileCall(t, ts, "GET", "/v1/awake", "secret", nil)
	if code != 200 {
		t.Fatalf("GET /v1/awake = %d, want 200", code)
	}
	awake, _ := body["awake"].(map[string]any)
	if awake == nil || awake["on"] != false {
		t.Fatalf("awake = %v, want an object that says off", body["awake"])
	}
	if awake["durationMin"] != float64(awakeDefaultMinutes) {
		t.Fatalf("durationMin = %v, want the 3-hour default", awake["durationMin"])
	}

	code, body = mobileCall(t, ts, "POST", "/v1/awake", "secret", map[string]any{"on": true, "durationMin": 45})
	if code != 200 {
		t.Fatalf("POST /v1/awake = %d, want 200", code)
	}
	awake, _ = body["awake"].(map[string]any)
	if awake == nil || awake["on"] != true || awake["durationMin"] != float64(45) {
		t.Fatalf("awake after POST = %v, want on with 45", body["awake"])
	}
	if until, _ := awake["until"].(string); until == "" {
		t.Fatal("a 45-minute 开启 must carry its deadline; the phone draws the countdown from it")
	}

	select {
	case b := <-c.out:
		var ev map[string]any
		if err := json.Unmarshal(b, &ev); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if ev["type"] != mobileEvAwakeChanged {
			t.Fatalf("event = %v, want awake_changed", ev)
		}
		if st, _ := ev["awake"].(map[string]any); st == nil || st["on"] != true {
			t.Fatalf("awake_changed = %v, want the new state", ev["awake"])
		}
	default:
		t.Fatal("a 开启 did not broadcast awake_changed: the tray menu and other phones stay stale")
	}

	// 关掉：另一块屏也要知道。
	if code, body = mobileCall(t, ts, "POST", "/v1/awake", "secret", map[string]any{"on": false}); code != 200 {
		t.Fatalf("POST off = %d, want 200", code)
	}
	if awake, _ = body["awake"].(map[string]any); awake["on"] != false {
		t.Fatalf("awake after off = %v, want off", body["awake"])
	}
	if !fake.acquired[0].released {
		t.Fatal("POST off did not release the assertion")
	}
}

// TestMobileAwakeRejectsBadDuration: an out-of-bounds length is the caller's mistake (400), not the
// machine refusing (500) — the phone shows a different sentence for each.
func TestMobileAwakeRejectsBadDuration(t *testing.T) {
	d, _ := newMobileApp(t)
	_, ts := mobileTestServer(t, d)
	fake := &fakeSleep{}
	d.awake.acquire = fake.acquire
	d.awake.after = fake.after

	code, body := mobileCall(t, ts, "POST", "/v1/awake", "secret", map[string]any{"on": true, "durationMin": 1})
	if code != 400 {
		t.Fatalf("POST with 1 minute = %d, want 400", code)
	}
	if body["error"] != "bad_request" {
		t.Fatalf("error = %v, want bad_request", body["error"])
	}
	if st := d.awakeState(); st.On {
		t.Fatal("a rejected length must not leave the switch on")
	}
}
