package node

import (
	"context"
	"sync"
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

// A machine that appears while a round is already out is asked about as soon as that round lands.
//
// The round in flight was composed from the directory as it stood when it went out
// (Sender.PeerReports walks the entries then), so it cannot answer about a machine that arrived since —
// and a machine that has just arrived is exactly what a request at that moment is about. Nothing else
// asks on its own (the picker asks on a keystroke, the panel on opening), so dropping the request is
// what left such a machine drawn as 检查中… with the count on the trigger unmoved — until the reader
// opened the very panel they were reading that count from.
func TestAMachineThatArrivesDuringARoundIsAskedWhenItLands(t *testing.T) {
	// Both ways a round can be out, because both compose their round the same way: the caller's own
	// probe (the picker's keystroke, a sighting, the panel opening) and a person pressing 刷新.
	for _, tc := range []struct {
		name  string
		start func(*PeerList)
	}{
		{name: "a probe already out", start: func(l *PeerList) { l.Probe() }},
		{name: "刷新 already out", start: func(l *PeerList) { go l.Refresh(context.Background()) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The directory as discovery sees it: one machine, and then two — with the round held open
			// between the two, so the assertions are about a real in-flight probe.
			var mu sync.Mutex
			directory := rows()[:1]
			asked := make(chan []string, 4)
			release := make(chan struct{})

			list := NewPeerList(
				func() []agentref.Peer {
					mu.Lock()
					defer mu.Unlock()
					return append([]agentref.Peer(nil), directory...)
				},
				func(ctx context.Context, force bool) []PeerReport {
					mu.Lock()
					keys := make([]string, 0, len(directory))
					for _, p := range directory {
						keys = append(keys, p.Key)
					}
					mu.Unlock()
					asked <- keys
					<-release
					reports := make([]PeerReport, 0, len(keys))
					for _, key := range keys {
						reports = append(reports, PeerReport{Key: key, Reachable: true})
					}
					return reports
				}, nil)

			tc.start(list)
			assert.Equal(t, []string{ugreenID}, <-asked, "the first round asked about the one machine there was")

			// Discovery finds a second machine and says so — this is that call (desktop/peerboard.go
			// peerDiscovered → Probe).
			mu.Lock()
			directory = rows()
			mu.Unlock()
			list.Probe()

			close(release)

			select {
			case second := <-asked:
				assert.ElementsMatch(t, []string{ugreenID, rows()[1].Key}, second,
					"the request that arrived mid-round is a round of its own once the first one lands")
			case <-time.After(2 * time.Second):
				t.Fatal("the request made while a round was in flight was dropped: the new machine is " +
					"never asked, and nothing else will ask on its own")
			}

			require.Eventually(t, func() bool {
				details := list.Details()
				return len(details) == 2 && details[1].Checked
			}, time.Second, 5*time.Millisecond, "and its answer has to reach the board")
		})
	}
}

// The line above must not turn the rate limit into no limit: a request that asks about the same rows
// the round in flight is already asking about is still refused (TestProbeCoalescesKeystrokes). What is
// remembered is a row NOBODY has asked about.
func TestARepeatRequestWhileARoundIsOutIsStillRefused(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})

	list := NewPeerList(rows, func(ctx context.Context, force bool) []PeerReport {
		calls.Add(1)
		<-release
		return nil
	}, nil)

	list.Probe()
	require.Eventually(t, func() bool { return calls.Load() == 1 }, time.Second, 5*time.Millisecond)

	// The same directory, over and over: nothing new, so nothing to remember.
	for range 10 {
		list.Probe()
	}
	close(release)
	require.Never(t, func() bool { return calls.Load() > 1 }, 200*time.Millisecond, 10*time.Millisecond,
		"a request about rows already being asked about is not a second round")
}

// A request that finds nothing to ASK about is not dropped either: this machine may have no sending
// side yet (an entry point installs its config as it boots, and ForSender answers nil in between), and
// whoever asked knew something — a machine entered the directory — while nothing else will ask again.
// Losing it is a reading that stays wrong for as long as nobody touches the UI.
func TestAProbeIsNotLostWhenThereIsNothingToAskAboutYet(t *testing.T) {
	var mu sync.Mutex
	var directory []agentref.Peer // no sending side yet: the directory cannot be read at all
	var calls atomic.Int32

	list := NewPeerList(
		func() []agentref.Peer {
			mu.Lock()
			defer mu.Unlock()
			return append([]agentref.Peer(nil), directory...)
		},
		func(context.Context, bool) []PeerReport {
			calls.Add(1)
			return nil
		}, nil)

	list.Probe()
	require.Zero(t, calls.Load(), "there was nothing to ask about")

	// The sending side arrives a moment later, with the machine that was found in it.
	mu.Lock()
	directory = rows()
	mu.Unlock()

	require.Eventually(t, func() bool { return calls.Load() > 0 }, 15*time.Second, 20*time.Millisecond,
		"the request has to survive until there is something to ask about")
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

// The listener hears about a CHANGE, and only about a change: that is what lets the node board (and
// the count on its trigger) follow a reading nobody re-reads by itself without turning into a
// heartbeat — every re-read asks the entry point for the next probe, so a round that repeated the
// last answers and still reported itself would tick for ever.
func TestPeerListTellsItsListenerOnlyWhenTheAnswersChange(t *testing.T) {
	reachable := true
	answers := func() []PeerReport {
		return []PeerReport{
			{Key: ugreenID, Reachable: reachable, Card: &PeerCard{Name: "home-nas"}},
			{Key: "@https://100.64.0.9:7080"},
		}
	}

	told := make(chan string, 8)
	list := NewPeerList(rows, func(context.Context, bool) []PeerReport { return answers() }, nil)
	list.SetOnChange(func() { told <- "changed" })

	// The first round is news: before it, nothing had been asked.
	list.Probe()
	<-told

	// The second says exactly what the first one did, so there is nothing to tell.
	list.ProbeNow()
	select {
	case <-told:
		t.Fatal("a round that repeated the last answers reported itself as news")
	case <-time.After(100 * time.Millisecond):
	}

	// A peer that goes down IS news, and it arrives on the same listener.
	reachable = false
	list.ProbeNow()
	select {
	case <-told:
	case <-time.After(2 * time.Second):
		t.Fatal("a peer that stopped answering is a change the listener has to hear")
	}
}

// The button's round tells the listener too — and tells it once the round is over in every sense, so
// a listener that reacts by reading (or asking for another round) is not answered with "one is
// already running" by the round that just finished.
func TestRefreshTellsTheListenerAfterItsAnswerLanded(t *testing.T) {
	list := NewPeerList(rows, func(context.Context, bool) []PeerReport {
		return []PeerReport{{Key: ugreenID, Reachable: true, Card: &PeerCard{Name: "home-nas"}}}
	}, nil)
	seen := make(chan struct{}, 1)
	list.SetOnChange(func() {
		// Read from inside the listener: what a frontend's re-read does. A Refresh still marked as
		// running would send this round nowhere (PeerList.probeAsync refuses to stack).
		require.True(t, list.Details()[0].Checked, "the listener must be told after the answers are stored")
		seen <- struct{}{}
	})

	list.Refresh(context.Background())
	select {
	case <-seen:
	case <-time.After(time.Second):
		t.Fatal("the refresh's answers were a change nobody heard about")
	}
}

// A machine that has just appeared is asked even when a round just happened — that is the whole
// reason the interval yields — while a round of keystrokes still coalesces (the node above pins that
// half, and this one pins the other).
func TestProbeAsksAMachineThatJustAppearedEvenInsideTheInterval(t *testing.T) {
	asked := make(chan int, 8)
	directory := rows()
	list := NewPeerList(func() []agentref.Peer { return directory }, func(context.Context, bool) []PeerReport {
		asked <- 1
		return nil
	}, nil)

	list.Probe()
	select {
	case <-asked:
	case <-time.After(time.Second):
		t.Fatal("the first probe never went out")
	}
	// Inside probeInterval, and now the directory holds a node the round did not ask about.
	directory = append(rows(), agentref.Peer{Key: discoveredID, Name: "nas-b"})
	list.Probe()
	select {
	case <-asked:
	case <-time.After(time.Second):
		t.Fatal("a machine that just appeared waited for the interval instead of being asked")
	}
}
