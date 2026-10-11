package node

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/fileutil"
)

// useTempConfigBase points the config paths at a temp dir. Every test in this file reads the pin
// record through config.PeerIdentitiesPath, and none of them may touch the real one.
func useTempConfigBase(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	config.SetBaseDir(base)
	t.Cleanup(func() { config.SetBaseDir("") })
	return base
}

// peers is the provider a PeerList reads through: the shape a real entry point passes, where the
// peer list comes from a config the reader can edit while the process runs.
func peers() func() []config.PeerConfig {
	return func() []config.PeerConfig {
		return []config.PeerConfig{
			{Name: "ugreen-nas", URL: "https://100.64.0.3:7080", Note: "家里的 NAS"},
			{Name: "hk-vps", URL: "https://100.64.0.9:7080"},
		}
	}
}

// The picker's opening list is every CONFIGURED node — a node missing from it would read as "not
// configured" — with the identity this machine pinned for it, which only outbound contact writes.
func TestPeerListSnapshotIsEveryConfiguredNode(t *testing.T) {
	useTempConfigBase(t)
	require.NoError(t, fileutil.WriteFilePrivate(config.PeerIdentitiesPath(), []byte(`{
  "ugreen-nas": {"node_id": "cd726b446c34386ece70d88fdd5fbf2a", "first_seen": "2026-10-01T00:00:00Z"}
}`)))

	list := NewPeerList(peers(), nil, nil)
	snapshot := list.Snapshot()

	require.Len(t, snapshot, 2, "the configured list is the list")
	assert.Equal(t, "ugreen-nas", snapshot[0].Alias)
	assert.Equal(t, "家里的 NAS", snapshot[0].Note)
	assert.Equal(t, "cd726b446c34386ece70d88fdd5fbf2a", snapshot[0].NodeID)
	assert.False(t, snapshot[0].Checked, "nothing has looked yet, which is not the same as 'down'")

	assert.Equal(t, "hk-vps", snapshot[1].Alias)
	assert.Empty(t, snapshot[1].NodeID, "never called outbound, so this machine holds no identity for it")
}

// An unreadable record costs the row its identity and nothing else: a picker that does not open is
// worse than one that says "unknown".
func TestPeerListSnapshotSurvivesAnUnreadablePinRecord(t *testing.T) {
	useTempConfigBase(t)
	require.NoError(t, fileutil.WriteFilePrivate(config.PeerIdentitiesPath(), []byte("{not json")))

	snapshot := NewPeerList(peers(), nil, nil).Snapshot()

	require.Len(t, snapshot, 2)
	assert.Empty(t, snapshot[0].NodeID)
}

// A probe runs in the background and its answer shows up in a LATER snapshot — the ordering the
// picker depends on, since the keystroke that triggers the probe has already been drawn.
func TestProbeAnswersInALaterSnapshot(t *testing.T) {
	useTempConfigBase(t)
	probed := make(chan struct{}, 1)

	list := NewPeerList(peers(), func(ctx context.Context, force bool) []PeerReport {
		defer func() { probed <- struct{}{} }()
		return []PeerReport{
			{Alias: "ugreen-nas", Reachable: true, Card: &PeerCard{Description: "A Tachi node that runs on its own machine"}},
			{Alias: "hk-vps"},
		}
	}, nil)

	before := list.Snapshot()
	require.Len(t, before, 2)
	assert.False(t, before[0].Checked, "a probe has not answered yet")

	list.Probe()
	<-probed
	require.Eventually(t, func() bool {
		return list.Snapshot()[0].Checked
	}, time.Second, 5*time.Millisecond, "the probe's answer has to reach the next snapshot")

	after := list.Snapshot()
	assert.True(t, after[0].Reachable)
	assert.Equal(t, "A Tachi node that runs on its own machine", after[0].Description)
	assert.True(t, after[1].Checked)
	assert.False(t, after[1].Reachable, "checked and unreachable is a different answer from not checked")
}

// A peer the probe never got to is "not looked at", not "down": only what the probe actually said
// about a node may be recorded about it.
func TestProbeRecordsOnlyWhatItWasTold(t *testing.T) {
	useTempConfigBase(t)
	done := make(chan struct{})

	list := NewPeerList(peers(), func(ctx context.Context, force bool) []PeerReport {
		defer close(done)
		// Only one of the two answered — a cancelled context looks exactly like this.
		return []PeerReport{{Alias: "ugreen-nas", Reachable: true}}
	}, nil)

	list.Probe()
	<-done
	require.Eventually(t, func() bool { return list.Snapshot()[0].Checked }, time.Second, 5*time.Millisecond)

	snapshot := list.Snapshot()
	assert.True(t, snapshot[0].Checked)
	assert.False(t, snapshot[1].Checked, "the second node was never asked, so nothing may be claimed about it")
}

// The picker calls Probe on every keystroke; the network may not be asked that often.
func TestProbeCoalescesKeystrokes(t *testing.T) {
	useTempConfigBase(t)
	var calls atomic.Int32

	list := NewPeerList(peers(), func(ctx context.Context, force bool) []PeerReport {
		calls.Add(1)
		return nil
	}, nil)

	// The first one goes out, the rest are absorbed: the interval is what keeps a burst of
	// keystrokes from becoming a burst of requests.
	list.Probe()
	require.Eventually(t, func() bool { return calls.Load() == 1 }, time.Second, 5*time.Millisecond)
	for range 20 {
		list.Probe()
	}
	require.Never(t, func() bool { return calls.Load() > 1 }, 100*time.Millisecond, 10*time.Millisecond)
}

// A process that cannot delegate still knows which nodes exist, so the picker still lists them.
// Nothing is checked, and nothing is claimed.
func TestPeerListWithoutAProbeStillListsPeers(t *testing.T) {
	useTempConfigBase(t)

	list := NewPeerList(peers(), nil, nil)
	list.Probe() // must not panic, and must not claim anything

	snapshot := list.Snapshot()
	require.Len(t, snapshot, 2)
	assert.False(t, snapshot[0].Checked)
	assert.False(t, snapshot[0].Reachable)
}

// No peers configured means nothing to offer and nothing to probe.
func TestPeerListEmptyConfiguration(t *testing.T) {
	useTempConfigBase(t)
	var calls atomic.Int32

	list := NewPeerList(func() []config.PeerConfig { return nil }, func(ctx context.Context, force bool) []PeerReport {
		calls.Add(1)
		return nil
	}, nil)

	assert.Empty(t, list.Snapshot())
	list.Probe()
	assert.Zero(t, calls.Load(), "there is nothing to ask about")
}

// A nil PeerList is what an entry point without a node side holds; it must answer, not panic.
func TestNilPeerList(t *testing.T) {
	var list *PeerList
	assert.Empty(t, list.Snapshot())
	list.Probe()
}

// The board's refresh is synchronous and honest about it: a person pressed a button, so the answer
// is there when the call returns — and two presses must not become two rounds of requests.
func TestRefreshWaitsForItsAnswerAndDoesNotStack(t *testing.T) {
	useTempConfigBase(t)
	release := make(chan struct{})
	var calls atomic.Int32
	var forced atomic.Bool

	list := NewPeerList(peers(), func(ctx context.Context, force bool) []PeerReport {
		calls.Add(1)
		if force {
			forced.Store(true)
		}
		<-release // hold the round open, so the assertions below are about a real in-flight probe
		return []PeerReport{{Alias: "ugreen-nas", Reachable: true}}
	}, nil)

	first := make(chan struct{})
	go func() { defer close(first); list.Refresh(context.Background()) }()

	second := make(chan struct{})
	go func() { defer close(second); list.Refresh(context.Background()) }()

	require.Eventually(t, func() bool { return calls.Load() == 1 }, time.Second, 5*time.Millisecond)
	require.Never(t, func() bool { return calls.Load() > 1 }, 150*time.Millisecond, 10*time.Millisecond)

	close(release)
	<-first
	<-second

	assert.True(t, forced.Load(), "刷新 asks the nodes NOW, not the cached cards")
	assert.True(t, list.Details()[0].Checked, "the answer is in place when the call returns")

	// The interval that paces the picker's probes does not apply to a person's click.
	list.Refresh(context.Background())
	assert.Equal(t, int32(2), calls.Load())
}

// Details is the board's projection of the same probe the picker reads: one round of answers, two
// views of it, so the panel and the '#' list cannot disagree about what is up.
func TestDetailsAndSnapshotShareOneProbe(t *testing.T) {
	useTempConfigBase(t)
	done := make(chan struct{})

	list := NewPeerList(peers(), func(ctx context.Context, force bool) []PeerReport {
		defer close(done)
		return []PeerReport{{
			Alias:     "ugreen-nas",
			URL:       "https://100.64.0.3:7080",
			Reachable: true,
			Card:      &PeerCard{Name: "tachi@home-nas", Description: "家里的节点", Version: "1.2.3"},
		}}
	}, nil)

	list.Probe()
	<-done
	require.Eventually(t, func() bool { return list.Details()[0].Checked }, time.Second, 5*time.Millisecond)

	detail := list.Details()[0]
	assert.Equal(t, "ugreen-nas", detail.Alias)
	assert.Equal(t, "https://100.64.0.3:7080", detail.URL, "the address THIS machine dials, from the config")
	require.NotNil(t, detail.Card)
	assert.Equal(t, "家里的节点", detail.Card.Description)

	// The picker's view of the same round: reachability yes, the card's description as the note...
	snapshot := list.Snapshot()
	assert.True(t, snapshot[0].Checked)
	assert.True(t, snapshot[0].Reachable)
	assert.Equal(t, "家里的节点", snapshot[0].Description)
	// ...and NOT the name the node reports about itself: that is not an identity, and this list
	// shows none of it (node/knownpeers.go).
	assert.NotContains(t, snapshot[0].Description, "tachi@home-nas")
}
