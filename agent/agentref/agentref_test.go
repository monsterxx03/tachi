package agentref

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/monsterxx03/tachi/agent/atfile"
)

func staticPeers(peers ...Peer) PeerList {
	return func() []Peer { return peers }
}

// The picker's opening list is every configured node, in alias order. A node missing from it
// would read as "not configured", which is the one thing a list of nodes may not imply.
func TestSearchListsEveryNodeInAliasOrder(t *testing.T) {
	src := New(staticPeers(
		Peer{Alias: "ugreen-nas", Note: "家里的 NAS", NodeID: "aa11", Reachable: true, Checked: true},
		Peer{Alias: "hk-vps", Note: "香港的机器"},
	))

	targets := src.Search("", 0)

	assert.Len(t, targets, 2)
	assert.Equal(t, "hk-vps", targets[0].Alias)
	assert.Equal(t, "ugreen-nas", targets[1].Alias)
	assert.Equal(t, "aa11", targets[1].NodeID)
	assert.True(t, targets[1].Checked)
	assert.False(t, targets[0].Checked, "a node nobody has probed is not 'checked and unreachable'")
}

// A match on the alias outranks a match on prose, because the alias is what the reader is typing.
func TestSearchRanksAliasHitsAboveProse(t *testing.T) {
	src := New(staticPeers(
		Peer{Alias: "mac", Description: "runs on the nas if it is up"},
		Peer{Alias: "nas", Note: "the box in the closet"},
		Peer{Alias: "ugreen-nas", Note: "家里的 NAS"},
	))

	targets := src.Search("nas", 0)

	assert.Equal(t, []string{"nas", "ugreen-nas", "mac"}, aliases(targets),
		"exact alias, then alias substring, then a mention in the description")
}

// An unknown name resolves to nothing. A near miss would point the reference at a DIFFERENT
// machine, which is worse than leaving it visible.
func TestResolveIsExactOrNothing(t *testing.T) {
	src := New(staticPeers(
		Peer{Alias: "ugreen-nas", NodeID: "cd726b446c34386ece70d88fdd5fbf2a"},
		Peer{Alias: "hk-vps", NodeID: "bb22"},
	))

	// The alias, and case-insensitively: it is typed by hand in two places.
	target, ok := src.Resolve("ugreen-nas")
	assert.True(t, ok)
	assert.Equal(t, "cd726b446c34386ece70d88fdd5fbf2a", target.NodeID)

	_, ok = src.Resolve("UGREEN-NAS")
	assert.True(t, ok, "the same machine spelled with different case")

	// The node id, which is what a reader who saw one in a pointer would paste back.
	target, ok = src.Resolve("cd726b446c34386ece70d88fdd5fbf2a")
	assert.True(t, ok)
	assert.Equal(t, "ugreen-nas", target.Alias)

	_, ok = src.Resolve("ugreen")
	assert.False(t, ok, "a prefix of an alias is not the alias")
	_, ok = src.Resolve("")
	assert.False(t, ok)
	_, ok = src.Resolve("   ")
	assert.False(t, ok)
}

// A Source with no list — a frontend that cannot reach this machine's peers — offers
// nothing, and says so by returning nothing rather than by failing.
func TestSourceWithoutAListOffersNothing(t *testing.T) {
	var nilSource *Source
	assert.Nil(t, New(nil))
	assert.Empty(t, nilSource.Search("", 0))
	_, ok := nilSource.Resolve("nas")
	assert.False(t, ok)
}

// The candidates are the two namespaces, and their spelling is atfile's: the row a reader picks
// inserts a prefix the expansion scan looks for.
func TestPrefixCandidatesAreTheTwoNamespaces(t *testing.T) {
	candidates := PrefixCandidates()

	assert.Len(t, candidates, 2)
	assert.Equal(t, "#a:", candidates[0].Ref)
	assert.Equal(t, "#s:", candidates[1].Ref)
	for _, c := range candidates {
		assert.Equal(t, c.Ref, c.Title)
		assert.NotEmpty(t, c.Note)
	}

	// What the rows insert must open the namespace the scan expects.
	ns, _ := atfile.HashQueryNamespace(strings.TrimPrefix(candidates[0].Ref, "#"))
	assert.Equal(t, atfile.HashNamespaceAgents, ns)
	ns, _ = atfile.HashQueryNamespace(strings.TrimPrefix(candidates[1].Ref, "#"))
	assert.Equal(t, atfile.HashNamespaceSessions, ns)
}

// RowNote prefers what the node says about itself and falls back to the configured
// line: one is a description, the other is what an operator wrote before the node ever answered.
func TestRowNotePrefersTheNodesOwnWords(t *testing.T) {
	assert.Equal(t, "A Tachi node", RowNote(atfile.AgentTarget{Description: "A Tachi node", Note: "家里的 NAS"}, 0))
	assert.Equal(t, "家里的 NAS", RowNote(atfile.AgentTarget{Note: "家里的 NAS"}, 0))
	assert.Equal(t, "", RowNote(atfile.AgentTarget{}, 0))
}

func aliases(targets []atfile.AgentTarget) []string {
	out := make([]string, len(targets))
	for i, t := range targets {
		out[i] = t.Alias
	}
	return out
}
