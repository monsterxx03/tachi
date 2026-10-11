package channel

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// fakeNotifier is a NotificationChannel that records what it was asked to send
// and can be made to fail, so a test can tell "which route was tried" from
// "which route answered".
type fakeNotifier struct {
	name string
	mu   sync.Mutex
	got  []string
	err  error
}

func (f *fakeNotifier) Name() string                                    { return f.name }
func (f *fakeNotifier) OnStart(context.Context) error                   { return nil }
func (f *fakeNotifier) Run(ctx context.Context, _ MessageHandler) error { <-ctx.Done(); return nil }

func (f *fakeNotifier) NotifyUser(_ context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, text)
	return f.err
}

func (f *fakeNotifier) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

// registerNotifier registers a factory producing a *fakeNotifier, returning the
// instance once built. The name must be unique: Register panics on duplicates.
func registerNotifier(t *testing.T, name string, err error) *fakeNotifier {
	t.Helper()
	f := &fakeNotifier{name: name, err: err}
	Register(name, func(map[string]any) (Channel, error) { return f, nil })
	return f
}

func TestNotifyRequested(t *testing.T) {
	cases := []struct {
		name string
		cfg  map[string]any
		want bool
	}{
		{"missing key", map[string]any{"enabled": true}, false},
		{"true", map[string]any{NotifyConfigKey: true}, true},
		{"false", map[string]any{NotifyConfigKey: false}, false},
		{"int one", map[string]any{NotifyConfigKey: 1}, true},
		{"float one", map[string]any{NotifyConfigKey: 1.0}, true},
		{"string is not a switch", map[string]any{NotifyConfigKey: "true"}, false},
	}
	for _, tc := range cases {
		if got := NotifyRequested(tc.cfg); got != tc.want {
			t.Errorf("%s: NotifyRequested = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestNotifiersFromConfigSwitchOff(t *testing.T) {
	cleanupRegistry()
	registerNotifier(t, "notify-off", nil)

	set := NotifiersFromConfig(map[string]map[string]any{
		"notify-off": {"enabled": true},
	}, nil)

	if !set.Empty() {
		t.Fatalf("a channel without the switch must not be a notification route, got %v", set.Channels())
	}
}

func TestNotifiersFromConfigWithoutInterface(t *testing.T) {
	cleanupRegistry()
	// A plain channel: enabled, switched on, but no NotificationChannel.
	Register("notify-plain", func(map[string]any) (Channel, error) {
		return &mockChannel{name: "notify-plain"}, nil
	})

	set := NotifiersFromConfig(map[string]map[string]any{
		"notify-plain": {"enabled": true, NotifyConfigKey: true},
	}, nil)

	if !set.Empty() {
		t.Fatalf("a channel without the interface must not be a route, got %v", set.Channels())
	}
}

func TestNotifiersFromConfigFactoryErrorIsSkipped(t *testing.T) {
	cleanupRegistry()
	Register("notify-broken", func(map[string]any) (Channel, error) {
		return nil, errors.New("token is required")
	})

	set := NotifiersFromConfig(map[string]map[string]any{
		"notify-broken": {"enabled": true, NotifyConfigKey: true},
	}, nil)

	if !set.Empty() {
		t.Fatalf("a factory that refuses its config must be skipped, got %v", set.Channels())
	}
}

func TestNotifiersFromConfigUnregisteredNameIsSkipped(t *testing.T) {
	cleanupRegistry()

	set := NotifiersFromConfig(map[string]map[string]any{
		"notify-ghost": {"enabled": true, NotifyConfigKey: true},
	}, nil)

	if !set.Empty() {
		t.Fatalf("an unregistered channel name must be skipped, got %v", set.Channels())
	}
}

func TestNotifiersFromConfigOrdersByName(t *testing.T) {
	cleanupRegistry()
	registerNotifier(t, "zeta", nil)
	registerNotifier(t, "alpha", nil)
	registerNotifier(t, "mid", nil)

	raw := map[string]map[string]any{
		"zeta":  {"enabled": true, NotifyConfigKey: true},
		"alpha": {"enabled": true, NotifyConfigKey: true},
		"mid":   {"enabled": true, NotifyConfigKey: true},
	}
	set := NotifiersFromConfig(raw, nil)

	got := set.Channels()
	want := []string{"alpha", "mid", "zeta"}
	if len(got) != len(want) {
		t.Fatalf("Channels() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Channels() = %v, want %v (map order must not leak)", got, want)
		}
	}
}

func TestNotifyFallsThroughToTheNextChannel(t *testing.T) {
	cleanupRegistry()
	registerNotifier(t, "alpha", errors.New("no stored credentials"))
	second := registerNotifier(t, "beta", nil)

	set := NotifiersFromConfig(map[string]map[string]any{
		"alpha": {"enabled": true, NotifyConfigKey: true},
		"beta":  {"enabled": true, NotifyConfigKey: true},
	}, nil)

	used, err := set.Notify(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if used != "beta" {
		t.Fatalf("Notify used %q, want beta", used)
	}
	if got := second.sent(); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("second channel received %v, want one hello", got)
	}
}

func TestNotifyReportsEveryFailureWithItsReason(t *testing.T) {
	cleanupRegistry()
	registerNotifier(t, "alpha", errors.New("no stored credentials"))
	registerNotifier(t, "beta", errors.New("user is not in a shared server"))

	set := NotifiersFromConfig(map[string]map[string]any{
		"alpha": {"enabled": true, NotifyConfigKey: true},
		"beta":  {"enabled": true, NotifyConfigKey: true},
	}, nil)

	_, err := set.Notify(context.Background(), "hello")
	if err == nil {
		t.Fatal("Notify must fail when no channel accepts the message")
	}
	// Both reasons have to survive: they name two different things to fix.
	for _, want := range []string{"alpha", "no stored credentials", "beta", "not in a shared server"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q is missing %q", err.Error(), want)
		}
	}
}

func TestNotifyRejectsEmptyInput(t *testing.T) {
	cleanupRegistry()
	f := registerNotifier(t, "alpha", nil)
	set := NotifiersFromConfig(map[string]map[string]any{
		"alpha": {"enabled": true, NotifyConfigKey: true},
	}, nil)

	if _, err := set.Notify(context.Background(), "   "); err == nil {
		t.Fatal("blank text must be refused")
	}
	if got := f.sent(); len(got) != 0 {
		t.Fatalf("blank text reached the channel: %v", got)
	}
}

func TestEmptySetIsEmptyAndNilSafe(t *testing.T) {
	var nilSet *NotifierSet
	if !nilSet.Empty() {
		t.Fatal("a nil set must read as empty")
	}
	if _, err := nilSet.Notify(context.Background(), "hello"); err == nil {
		t.Fatal("a nil set must refuse to notify")
	}

	empty := NotifiersFromConfig(nil, nil)
	if !empty.Empty() {
		t.Fatal("no channels must read as empty")
	}
}
