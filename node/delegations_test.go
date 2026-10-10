package node

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"testing"

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
	require.Equal(t, a2a.TaskStateSubmitted, onDisk.LastKnown.State)
	require.False(t, onDisk.LastKnown.At.IsZero(), "a cached state carries when it was seen")

	// A reopened store finds it: the record outlives the process that wrote it.
	reopened, err := openDelegationStore(dir)
	require.NoError(t, err)
	got, ok := reopened.get(d.ID)
	require.True(t, ok)
	require.Equal(t, d.ID, got.ID)
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

	require.Len(t, store.list(""), 3, "no filter means the machine's own records, all of them")
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

func TestPeerNamesMustIdentifyOneNode(t *testing.T) {
	_, err := newPeerDirectory([]config.PeerConfig{
		{Name: "home-nas", URL: "http://127.0.0.1:1234"},
		{Name: "home-nas", URL: "http://127.0.0.1:5678"},
	})
	require.Error(t, err, "a name that resolves to two nodes makes every delegation ambiguous")
	require.Contains(t, err.Error(), "configured twice")

	_, err = newPeerDirectory([]config.PeerConfig{{Name: "", URL: "http://127.0.0.1:1234"}})
	require.Error(t, err)

	_, err = newPeerDirectory([]config.PeerConfig{{Name: "home-nas", URL: ""}})
	require.Error(t, err)
}

func TestAnUnknownPeerRefusalListsTheConfiguredOnes(t *testing.T) {
	dir, err := newPeerDirectory([]config.PeerConfig{
		{Name: "hk-vps", URL: "http://127.0.0.1:1"},
		{Name: "home-nas", URL: "http://127.0.0.1:2", Note: "家里的 NAS"},
	})
	require.NoError(t, err)

	_, err = dir.Lookup(t.Context(), "nas")
	var unknown *UnknownPeerError
	require.ErrorAs(t, err, &unknown)
	require.Equal(t, []string{"hk-vps", "home-nas"}, unknown.Known, "sorted, so the answer reads the same twice")
	require.Contains(t, err.Error(), "home-nas")
}

func TestAnUnreachablePeerIsReportedNotInvented(t *testing.T) {
	dir, err := newPeerDirectory([]config.PeerConfig{
		{Name: "home-nas", URL: "http://127.0.0.1:9", Note: "家里的 NAS"},
	})
	require.NoError(t, err)
	dir.resolve = func(context.Context, string) (*a2a.AgentCard, error) {
		return nil, errors.New("connection refused")
	}

	got, err := dir.Lookup(t.Context(), "home-nas")
	require.NoError(t, err, "an unreachable peer is a fact about the network, not a bad alias")
	require.Nil(t, got.Card)
	require.Error(t, got.Err, "…and the failure travels with the entry")
	require.Equal(t, "家里的 NAS", got.Note, "the configured line is what is left to say about it")

	// The card is cached, failure included: a peer that is down must not cost a timeout per turn.
	calls := 0
	dir.resolve = func(context.Context, string) (*a2a.AgentCard, error) {
		calls++
		return nil, errors.New("still down")
	}
	_, _ = dir.Lookup(t.Context(), "home-nas")
	require.Zero(t, calls, "the failure is remembered for the card TTL")
}

func TestAReachablePeerIsDescribedByItsOwnCard(t *testing.T) {
	dir, err := newPeerDirectory([]config.PeerConfig{{Name: "home-nas", URL: "http://127.0.0.1:9"}})
	require.NoError(t, err)
	dir.resolve = func(context.Context, string) (*a2a.AgentCard, error) {
		return &a2a.AgentCard{Name: "Tachi", Description: "一台节点的自述"}, nil
	}

	got, err := dir.Lookup(t.Context(), "home-nas")
	require.NoError(t, err)
	require.NoError(t, got.Err)
	require.Equal(t, "一台节点的自述", got.Card.Description)
	require.Len(t, dir.Snapshot(t.Context()), 1)
}
