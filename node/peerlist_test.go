package node

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/agent/agentref"
)

const ugreenID = "cd726b446c34386ece70d88fdd5fbf2a"

// rows is the provider a PeerList reads through: the shape a real entry point passes, which is the
// sending side's DIRECTORY — configured machines and discovered ones alike, with whatever identity
// this machine holds for each.
func rows() []agentref.Peer {
	return []agentref.Peer{
		{Key: ugreenID, Label: "ugreen-nas", Name: "ugreen-nas", NodeID: ugreenID,
			Addr: "https://100.64.0.3:7080", Note: "家里的 NAS"},
		// A machine nobody has reached in this process: listed, because "not listed" would read as
		// "not configured", and marked as having no identity yet.
		{Key: "@https://100.64.0.9:7080", Name: "hk-vps", Note: "香港的机器"},
	}
}

// The picker's opening list is every node in the directory — a node missing from it would read as
// "not configured" — with the identity this machine holds for each (if it holds one).
func TestPeerListSnapshotIsEveryKnownNode(t *testing.T) {
	list := NewPeerList(rows, nil, nil)
	snapshot := list.Snapshot()

	require.Len(t, snapshot, 2, "the directory is the list")
	assert.Equal(t, "ugreen-nas", snapshot[0].Name)
	assert.Equal(t, "家里的 NAS", snapshot[0].Note)
	assert.Equal(t, ugreenID, snapshot[0].NodeID)
	assert.False(t, snapshot[0].Checked, "nothing has looked yet, which is not the same as 'down'")

	assert.Equal(t, "hk-vps", snapshot[1].Name)
	assert.Empty(t, snapshot[1].NodeID, "never reached, so this machine holds no identity for it")
}

// A probe runs in the background and its answer shows up in a LATER snapshot — the ordering the
// picker depends on, since the keystroke that triggers the probe has already been drawn.
func TestProbeAnswersInALaterSnapshot(t *testing.T) {
	probed := make(chan struct{}, 1)

	list := NewPeerList(rows, func(ctx context.Context, force bool) []PeerReport {
		defer func() { probed <- struct{}{} }()
		return []PeerReport{
			{Key: ugreenID, Reachable: true, Card: &PeerCard{Description: "A Tachi node that runs on its own machine"}},
			{Key: "@https://100.64.0.9:7080"},
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
	done := make(chan struct{})

	list := NewPeerList(rows, func(ctx context.Context, force bool) []PeerReport {
		defer close(done)
		// Only one of the two answered — a cancelled context looks exactly like this.
		return []PeerReport{{Key: ugreenID, Reachable: true}}
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
	var calls atomic.Int32

	list := NewPeerList(rows, func(ctx context.Context, force bool) []PeerReport {
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
	list := NewPeerList(rows, nil, nil)
	list.Probe() // must not panic, and must not claim anything

	snapshot := list.Snapshot()
	require.Len(t, snapshot, 2)
	assert.False(t, snapshot[0].Checked)
	assert.False(t, snapshot[0].Reachable)
}

// No peers configured means nothing to offer and nothing to probe.
func TestPeerListEmptyConfiguration(t *testing.T) {
	var calls atomic.Int32

	list := NewPeerList(func() []agentref.Peer { return nil }, func(ctx context.Context, force bool) []PeerReport {
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
	release := make(chan struct{})
	var calls atomic.Int32
	var forced atomic.Bool

	list := NewPeerList(rows, func(ctx context.Context, force bool) []PeerReport {
		calls.Add(1)
		if force {
			forced.Store(true)
		}
		<-release // hold the round open, so the assertions below are about a real in-flight probe
		return []PeerReport{{Key: ugreenID, Reachable: true}}
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
	done := make(chan struct{})

	list := NewPeerList(rows, func(ctx context.Context, force bool) []PeerReport {
		defer close(done)
		return []PeerReport{{
			Key:       ugreenID,
			Label:     "ugreen-nas",
			Name:      "ugreen-nas",
			NodeID:    ugreenID,
			URL:       "https://100.64.0.3:7080",
			Reachable: true,
			Card:      &PeerCard{Name: "tachi@home-nas", Description: "家里的节点", Version: "1.2.3"},
		}}
	}, nil)

	list.Probe()
	<-done
	require.Eventually(t, func() bool { return list.Details()[0].Checked }, time.Second, 5*time.Millisecond)

	detail := list.Details()[0]
	assert.Equal(t, "ugreen-nas", detail.Name)
	assert.Equal(t, ugreenID, detail.NodeID)
	// The address and the note come from the DIRECTORY, not from the probe: the board has them before
	// anybody has asked anything, and only the answer below waits for a round trip.
	assert.Equal(t, "https://100.64.0.3:7080", detail.URL, "the address THIS machine dials first")
	assert.Equal(t, "家里的 NAS", detail.Note)
	require.NotNil(t, detail.Card)
	assert.Equal(t, "家里的节点", detail.Card.Description)

	// The picker's view of the same round: reachability yes, the card's description as the note...
	snapshot := list.Snapshot()
	assert.True(t, snapshot[0].Checked)
	assert.True(t, snapshot[0].Reachable)
	assert.Equal(t, "家里的节点", snapshot[0].Description)
	// ...and the row keeps showing what to CALL the node, not the name it reports about itself: that
	// is not an identity, so it does not become a row's label either (node/peerdir.go Name).
	assert.Equal(t, "ugreen-nas", snapshot[0].Name)
	assert.NotEqual(t, "tachi@home-nas", snapshot[0].Name)
}
