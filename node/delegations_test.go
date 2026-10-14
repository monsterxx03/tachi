package node

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/fileutil"
)

// The two halves of the sending side that exist before any tool does: the directory of nodes
// this machine may delegate to, and the record of what it already delegated.

func TestDelegationIDsAreLocalAndStable(t *testing.T) {
	dir := t.TempDir()
	store, err := openDelegationStore(dir)
	require.NoError(t, err)

	d := newDelegation("home-nas", "logical-1", "检查昨天的备份", "检查昨天的备份，只看不改", "msg-1")
	require.Regexp(t, regexp.MustCompile(`^d_[0-9a-f]{8}$`), d.ID,
		"the model copies this id around; it has to look like this machine's record")
	require.NoError(t, store.put(d))

	// Through to disk before it is visible: the record describes something that really left.
	var onDisk Delegation
	require.NoError(t, fileutil.ReadJSON(filepath.Join(dir, d.ID+".json"), &onDisk))
	require.Equal(t, d.ID, onDisk.ID)
	require.Equal(t, "msg-1", onDisk.MessageID, "the message id is part of the record, not regenerated")
	require.Equal(t, "检查昨天的备份，只看不改", onDisk.Prompt, "the request text is kept so a retry can be identical")
	// No state yet: the request may not have left, so the record reports "unknown" until a peer
	// says otherwise — recording SUBMITTED here would claim an acceptance that may never happen.
	require.Equal(t, a2a.TaskState(""), onDisk.LastKnown.State)
	require.False(t, onDisk.LastKnown.At.IsZero(), "when the record was last touched is not optional")

	// A reopened store finds it: the record outlives the process that wrote it.
	reopened, err := openDelegationStore(dir)
	require.NoError(t, err)
	got, ok := reopened.get(d.ID)
	require.True(t, ok)
	require.Equal(t, d.ID, got.ID)
}

// The stored request is cut by CHARACTERS too, and for the same reason as a peer's answer: it is
// text kept for a person to read (and for a retry to send the same thing), and a byte offset both
// costs a Chinese prompt two thirds of its budget and can cut one in half — invalid UTF-8 written
// into a json record, which every later read renders as a replacement character.
func TestALongPromptIsStoredCutBetweenCharacters(t *testing.T) {
	// One ASCII byte in front, so the byte limit falls off every character boundary.
	prompt := "x" + strings.Repeat("查", promptExcerptLen)

	d := newDelegation("home-nas", "conv-a", "长请求", prompt, "m1")

	require.NotContains(t, d.Prompt, "\uFFFD", "a half-cut character is what would land here")
	require.True(t, strings.HasPrefix(prompt, d.Prompt),
		"what is kept is the beginning of what was sent, not a sequence that broke on the way")
	require.Equal(t, promptExcerptLen, len([]rune(d.Prompt)), "the budget is characters")
}

func TestAListShowsOneConversationItsOwnWork(t *testing.T) {
	store, err := openDelegationStore(t.TempDir())
	require.NoError(t, err)

	mine := newDelegation("home-nas", "conv-a", "第一件", "p1", "m1")
	other := newDelegation("hk-vps", "conv-b", "别的对话的活", "p2", "m2")
	newer := newDelegation("home-nas", "conv-a", "第二件", "p3", "m3")
	for _, d := range []*Delegation{mine, other, newer} {
		require.NoError(t, store.put(d))
	}

	list := store.list("conv-a")
	require.Len(t, list, 2, "another conversation's work must not be listed (§8.2)")
	require.Equal(t, newer.ID, list[0].ID, "newest first")

	// The empty scope is not a wildcard: it is what a run without a conversation files under.
	require.Empty(t, store.list(""), "a sessionless scope must not see a conversation's work")
	require.Len(t, store.list("conv-b"), 1)

	sessionless := newDelegation("home-nas", "", "管道跑的一次", "p4", "m4")
	require.NoError(t, store.put(sessionless))
	require.Len(t, store.list(""), 1)
}

// The store's answer must not depend on MAP ITERATION. A burst of delegations made in one instant
// is the shape that exposes it: the records come out of a map and an unstable comparator leaves
// their relative order to the iteration. A reminder whose lines reorder for no reason is one the
// collector appends again as if it were news, and the prompt prefix moves with it.
func TestAListOrdersTiesTheSameWayEveryTime(t *testing.T) {
	store, err := openDelegationStore(t.TempDir())
	require.NoError(t, err)

	same := time.Now().UTC()
	for _, id := range []string{"d_cccccccc", "d_aaaaaaaa", "d_bbbbbbbb"} {
		d := newDelegation("home-nas", "conv-a", id, "p", id)
		d.ID = id
		d.CreatedAt, d.UpdatedAt = same, same
		require.NoError(t, store.put(d))
	}

	first := store.list("conv-a")
	require.Len(t, first, 3)
	require.Equal(t, []string{"d_aaaaaaaa", "d_bbbbbbbb", "d_cccccccc"},
		[]string{first[0].ID, first[1].ID, first[2].ID},
		"equal instants are broken by id, so the answer reads the same twice")

	for i := range 20 {
		again := store.list("conv-a")
		require.Len(t, again, 3)
		for k := range first {
			require.Equal(t, first[k].ID, again[k].ID,
				"the order must not depend on map iteration (call %d)", i+1)
		}
	}
}

func TestAnUnknownIDIsRefusedRatherThanGuessed(t *testing.T) {
	store, err := openDelegationStore(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, store.put(newDelegation("home-nas", "conv-a", "t", "p", "m")))

	_, err = store.find("d_doesnotexist")
	var unknown *UnknownDelegationError
	require.ErrorAs(t, err, &unknown)
	require.Contains(t, err.Error(), "list", "the refusal has to say how to find the right id")
}

func TestPeerLabelsMustNameOneMachine(t *testing.T) {
	// A duplicated LABEL is refused because it is a display rule: a tool argument names a machine by
	// identity, so two rows reading "home-nas" would be resolvable and unreadable — which is the
	// whole point of a label, defeated.
	_, err := newPeerDirectory([]config.PeerConfig{
		{Name: "home-nas", URL: "https://127.0.0.1:1234"},
		{Name: "home-nas", URL: "https://127.0.0.1:5678"},
	})
	require.Error(t, err, "a label that names two machines reads as the same machine twice")
	require.Contains(t, err.Error(), "configured twice")

	// The label is OPTIONAL now: a config line is an address, and the label is a convenience.
	_, err = newPeerDirectory([]config.PeerConfig{{URL: "https://127.0.0.1:1234"}})
	require.NoError(t, err, "a peer without a label is a peer")

	_, err = newPeerDirectory([]config.PeerConfig{{Name: "home-nas", URL: ""}})
	require.Error(t, err, "a delegation needs an address to reach")
}

func TestAnUnknownNodeRefusalListsTheOnesThisMachineCanName(t *testing.T) {
	dir, err := newPeerDirectory([]config.PeerConfig{
		{Name: "hk-vps", URL: "https://127.0.0.1:1"},
		{Name: "home-nas", URL: "https://127.0.0.1:2", Note: "家里的 NAS"},
	})
	require.NoError(t, err)

	_, err = dir.Lookup(t.Context(), "deadbeef")
	var unknown *UnknownPeerError
	require.ErrorAs(t, err, &unknown)
	// Neither machine has been reached, so neither has an identity, and the refusal says that in
	// words rather than leaving a hole where the identity would be.
	require.Equal(t, []string{"hk-vps (身份未记录)", "home-nas (身份未记录)"}, unknown.Known,
		"sorted, so the answer reads the same twice")
	require.Contains(t, err.Error(), "home-nas")
}

// A label is the mistake a reader WILL make — it is what the reminder and the rows show — so it gets
// an answer of its own rather than "no such node": one that says where the id would have come from.
func TestALabelRefusalSaysWhatToUseInstead(t *testing.T) {
	dir, err := newPeerDirectory([]config.PeerConfig{{Name: "home-nas", URL: "https://127.0.0.1:2"}})
	require.NoError(t, err)

	_, err = dir.Lookup(t.Context(), "home-nas")
	require.Error(t, err)
	require.Contains(t, err.Error(), "label")
	require.Contains(t, err.Error(), "identity")
	require.NotContains(t, err.Error(), "is not a node this machine knows",
		"a label this machine configured is not an unknown node")
}

// A node becomes nameable when a handshake proves its identity, and not before: the identity is what
// a tool argument takes, so there is nothing to pass until there is one (design §八 验收:
// 配置来的对端在这轮探测成功之后可被点名).
func TestAConfiguredNodeBecomesNameableAfterOneContact(t *testing.T) {
	const id = "3f2a1b4c3f2a1b4c3f2a1b4c3f2a1b4c"
	dir, err := newPeerDirectory([]config.PeerConfig{{Name: "home-nas", URL: "https://127.0.0.1:2"}})
	require.NoError(t, err)
	dir.known = emptyPins(t)

	_, err = dir.Lookup(t.Context(), id)
	require.Error(t, err, "before the first contact there is no identity to name")

	// The handshake is what pins it (node/tls.go confirm), and this is where that leaves the entry.
	require.NoError(t, dir.known.confirm("home-nas", "https://127.0.0.1:2", id))

	got, err := dir.Lookup(t.Context(), id[:8])
	require.NoError(t, err, "a unique prefix is enough")
	require.Equal(t, id, got.Identity)
	require.Equal(t, "home-nas", got.Name(), "the label is what the row calls it")
	require.Equal(t, "home-nas ("+id[:8]+")", got.Display())
}

// An identity pinned for a machine this machine can no longer place is refused with the reason, not
// silently sent somewhere: the pin outlives the config line and says so.
func TestAPinnedNodeWithNoEntryIsRefusedWithItsReason(t *testing.T) {
	dir, err := newPeerDirectory(nil)
	require.NoError(t, err)
	dir.known = emptyPins(t)
	const gone = "aaaa1111aaaa1111aaaa1111aaaa1111"
	require.NoError(t, dir.known.confirm("old-nas", "https://127.0.0.1:9", gone))

	_, err = dir.Lookup(t.Context(), gone[:8])
	require.Error(t, err, "nothing in the directory carries that identity any more")

	client := &remoteClient{peers: dir, known: dir.known}
	_, err = client.peerOf(&Delegation{ID: "d_1", Peer: "old-nas", PeerNodeID: gone})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Configure that node again")
}

func TestAnUnreachablePeerIsReportedNotInvented(t *testing.T) {
	dir, err := newPeerDirectory([]config.PeerConfig{
		{Name: "home-nas", URL: "https://127.0.0.1:9", Note: "家里的 NAS"},
	})
	require.NoError(t, err)
	calls := 0
	dir.resolveEntry = func(context.Context, peerEntry) (*a2a.AgentCard, string, error) {
		calls++
		return nil, "", errors.New("connection refused")
	}

	snapshot := dir.Snapshot(t.Context())
	require.Len(t, snapshot, 1, "an unreachable peer is listed: not listed would read as not configured")
	got := snapshot[0]
	require.Nil(t, got.Card)
	require.Error(t, got.Err, "…and the failure travels with the entry")
	require.Equal(t, "家里的 NAS", got.Note, "the configured line is what is left to say about it")
	require.Equal(t, "home-nas (身份未记录)", got.Display())
	require.Equal(t, 1, calls)

	// The card is cached, failure included: a peer that is down must not cost a timeout per turn.
	dir.Snapshot(t.Context())
	require.Equal(t, 1, calls, "the failure is remembered for the card TTL")
}

func TestAReachablePeerIsDescribedByItsOwnCard(t *testing.T) {
	dir, err := newPeerDirectory([]config.PeerConfig{{Name: "home-nas", URL: "https://127.0.0.1:9"}})
	require.NoError(t, err)
	dir.resolveEntry = func(context.Context, peerEntry) (*a2a.AgentCard, string, error) {
		return &a2a.AgentCard{Name: "Tachi", Description: "一台节点的自述"}, "http://127.0.0.1:9", nil
	}

	snapshot := dir.Snapshot(t.Context())
	require.Len(t, snapshot, 1)
	require.NoError(t, snapshot[0].Err)
	require.Equal(t, "一台节点的自述", snapshot[0].Card.Description)
	require.True(t, snapshot[0].Reachable())
}
