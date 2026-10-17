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

// The picker's opening list is every node this machine knows, in name order. A node missing from it
// would read as "not configured", which is the one thing a list of nodes may not imply.
func TestSearchListsEveryNameableNodeInNameOrder(t *testing.T) {
	src := New(staticPeers(
		Peer{Label: "ugreen-nas", Name: "ugreen-nas", Note: "家里的 NAS", NodeID: "aa11aa11aa11aa11", Reachable: true, Checked: true},
		Peer{Label: "hk-vps", Name: "hk-vps", Note: "香港的机器", NodeID: "bb22bb22bb22bb22"},
	))

	targets := src.Search("", 0)

	assert.Len(t, targets, 2)
	assert.Equal(t, "hk-vps", targets[0].Name)
	assert.Equal(t, "ugreen-nas", targets[1].Name)
	assert.Equal(t, "aa11aa11aa11aa11", targets[1].NodeID)
	assert.True(t, targets[1].Checked)
	assert.False(t, targets[0].Checked, "a node nobody has probed is not 'checked and unreachable'")
}

// A node this machine holds no identity for is not offered as a REFERENCE, and it is not hidden
// either: this picker inserts identity-carrying references, so a row it offered would leave a
// dangling "#a:" in the message. The machines this machine knows are still listed — by the reminder
// and on the board — with the reason they cannot be pointed at yet.
func TestSearchSkipsNodesWithNoIdentity(t *testing.T) {
	src := New(staticPeers(
		Peer{Name: "nobody-has-reached-it", Note: "configured, never contacted"},
		Peer{Name: "known", NodeID: "cc33cc33cc33cc33"},
	))

	targets := src.Search("", 0)

	assert.Len(t, targets, 1, "only the machine that can be named")
	assert.Equal(t, "known", targets[0].Name)
	assert.Empty(t, src.Search("nobody", 0), "and it cannot be searched into existence either")
}

// A match on the name outranks a match on prose, because the name is what the reader is typing.
func TestSearchRanksNameHitsAboveProse(t *testing.T) {
	src := New(staticPeers(
		Peer{Name: "mac", NodeID: "aa00aa00aa00aa00", Description: "runs on the nas if it is up"},
		Peer{Name: "nas", NodeID: "bb00bb00bb00bb00", Note: "the box in the closet"},
		Peer{Name: "ugreen-nas", NodeID: "cc00cc00cc00cc00", Note: "家里的 NAS"},
	))

	targets := src.Search("nas", 0)

	assert.Equal(t, []string{"nas", "ugreen-nas", "mac"}, names(targets),
		"exact name, then name substring, then a mention in the description")
}

// A node whose row shows its own self-declared name still matches when the reader types the identity
// off that row: the identity is the string they are told to use.
func TestSearchMatchesTheIdentityToo(t *testing.T) {
	src := New(staticPeers(
		Peer{Name: "Tachi", NodeID: "aa11bb22cc33dd44aa11bb22cc33dd44"},
		Peer{Name: "其它", NodeID: "99887766554433229988776655443322"},
	))

	targets := src.Search("aa11bb22", 0)

	assert.Len(t, targets, 1)
	assert.Equal(t, "aa11bb22cc33dd44aa11bb22cc33dd44", targets[0].NodeID)
}

// An unknown id resolves to nothing. A near miss would point the reference at a DIFFERENT machine,
// which is worse than leaving it visible.
func TestResolveIsExactOrNothing(t *testing.T) {
	src := New(staticPeers(
		Peer{Label: "ugreen-nas", Name: "ugreen-nas", NodeID: "cd726b446c34386ece70d88fdd5fbf2a"},
		Peer{Label: "hk-vps", Name: "hk-vps", NodeID: "bb22"},
	))

	// The identity, whole.
	target, ok := src.Resolve("cd726b446c34386ece70d88fdd5fbf2a")
	assert.True(t, ok)
	assert.Equal(t, "cd726b446c34386ece70d88fdd5fbf2a", target.NodeID)

	// A LABEL does not resolve, even when it is exactly what the row shows: a name is what its owner
	// says it is, and a reference that quietly pointed elsewhere would be worse than one that stays
	// visible and unexpanded.
	_, ok = src.Resolve("ugreen-nas")
	assert.False(t, ok, "a label is not a way to name a machine")

	// The front of an id, which is what a reader who saw one on a row would copy.
	target, ok = src.Resolve("cd726b44")
	assert.True(t, ok)
	assert.Equal(t, "ugreen-nas", target.Name)

	// A prefix that names no machine is not a near miss.
	_, ok = src.Resolve("ffff")
	assert.False(t, ok, "a prefix of an id that matches nothing resolves to nothing")
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

func names(targets []atfile.AgentTarget) []string {
	out := make([]string, len(targets))
	for i, t := range targets {
		out[i] = t.Name
	}
	return out
}

// A node can be referenced by its name, by its whole id, or by a unique prefix of one — a reader who
// copied eight characters off a picker row is not making a mistake.
//
// A prefix that names two machines resolves to nothing, which is the same treatment an unknown name
// gets: the reference stays visible and unexpanded rather than quietly pointing at the wrong machine.
func TestResolveAcceptsAnIdentityPrefix(t *testing.T) {
	src := New(staticPeers(
		Peer{Name: "home-nas", NodeID: "abcd1111abcd1111abcd1111abcd1111"},
		Peer{Name: "hk-vps", NodeID: "abcd2222abcd2222abcd2222abcd2222"},
	))

	target, ok := src.Resolve("abcd1111")
	assert.True(t, ok, "a unique prefix of an id names a machine")
	assert.Equal(t, "home-nas", target.Name)

	_, ok = src.Resolve("abcd")
	assert.False(t, ok, "a prefix that names two machines is refused, not guessed")

	_, ok = src.Resolve("ffff")
	assert.False(t, ok, "an id this machine does not know resolves to nothing")
}

// How a peer became known is a SET, and a row states all of it: the two network ways fill different
// gaps (one crosses no router, the other cannot see a machine nobody pointed it at), so a machine that
// arrived by both is one row that came by both. The order is fixed — config first, because that is the
// one a reader can act on — so the same set is the same words everywhere, and an empty set says
// nothing rather than guessing at something.
func TestTheSourceWordStatesEveryWayAMachineIsKnown(t *testing.T) {
	assert.Equal(t, "", SourceWord(0), "a peer with no source says nothing")
	assert.Equal(t, "配置", SourceWord(SourceConfigured))
	assert.Equal(t, "mDNS", SourceWord(SourceMDNS))
	assert.Equal(t, "gossip", SourceWord(SourceGossip))
	assert.Equal(t, "配置 · mDNS", SourceWord(SourceConfigured|SourceMDNS))
	assert.Equal(t, "mDNS · gossip", SourceWord(SourceMDNS|SourceGossip))
	assert.Equal(t, "配置 · mDNS · gossip", SourceWord(SourceConfigured|SourceMDNS|SourceGossip),
		"all three at once, in the order a reader acts on them")
}
