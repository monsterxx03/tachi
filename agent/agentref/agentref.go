// Package agentref resolves #a: references and backs the node picker: "which of
// my machines is this node" is answered from what THIS machine knows about its
// peers, never from what a peer says about itself.
//
// Two facts make up an answer, and they come from two different places on purpose:
//
//   - the LABEL and the note are configuration — this machine's own name for a peer, shown on a row
//     and nothing more;
//   - the NODE ID is a record (config.PeerIdentitiesPath), written only by a TLS handshake on an
//     OUTBOUND call. A node this machine has never called therefore has no identity here, and
//     that is a correct answer rather than missing data: the alternative — the name the peer
//     sends — can be claimed by any key (design §4.2).
//
// Reachability is a third thing, and it is deliberately the odd one out: it is a fact about a
// MOMENT, so it is refreshed in the background and read as a snapshot (see node.PeerList). A
// picker row may show it; the text a reference expands into may not, because that text outlives
// the moment.
//
// A fourth — HOW this machine came to know the peer (PeerSource) — rides along for the node board
// and is not part of an answer a picker gives: it is a property of the row's existence rather than
// of the machine, and a reference has nothing to do with it.
package agentref

import (
	"fmt"
	"slices"
	"strings"

	"github.com/monsterxx03/tachi/agent/atfile"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

// DefaultLimit caps how many rows a search returns when the caller names no limit,
// matching the @-file picker's.
const DefaultLimit = 20

// Peer is one node this machine may delegate to, as the picker needs it.
type Peer struct {
	// Label is the name a config line gave the node, if it gave one. It is DISPLAY ONLY: a node is
	// named by its NodeID, in a reference and in a tool argument alike.
	Label string
	// Name is what to call the node in a sentence: the label when there is one, otherwise what the
	// node calls itself, otherwise the short form of its id. It is computed by the sending side
	// (node/peerdir.go) rather than here, because it is one rule about a peer directory and this
	// package would be a second copy of it.
	Name string
	// NodeID is the identity pinned on first outbound contact; empty when this machine has never
	// called that node. It is also the ONLY thing that may name the node: a reference carries it, and
	// so does the tool argument the reference expands into.
	NodeID string
	// Key matches this row to the last probe's answer about the same machine. It is plumbing between
	// two reads of one directory (the probe goes out and answers later), never displayed and never
	// used to name anything.
	Key string
	// Addr is where this machine will dial FIRST — the address the node advertises about itself when
	// it advertises one, the configured one otherwise. A picker row does not show it; the node board
	// does, because "which address did this machine try" is the first question about a row that says
	// unreachable.
	Addr string
	// Source is how this machine came to know the node, every way at once. A picker row does not
	// show it; the node board does, because "why is this row here at all" is the question a reader
	// has about a machine nobody configured.
	Source PeerSource
	// Note is the configured human line, used when the node has no description to offer.
	Note string
	// Description is the node's own account of itself, empty when its card could not be read.
	Description string
	// Reachable and Checked describe the LAST probe: Checked false means nobody has looked yet,
	// which is not the same as "could not be reached" and must never be shown as if it were.
	Reachable bool
	Checked   bool
}

// PeerList returns every configured node with whatever is known about each, cheapest
// first: the caller is a picker, which asks on every keystroke.
type PeerList func() []Peer

// Source resolves and searches this machine's nodes.
type Source struct {
	list PeerList
}

// New returns a Source over the peers list.
func New(list PeerList) *Source {
	if list == nil {
		return nil
	}
	return &Source{list: list}
}

// Warm asks the list to start whatever it refreshes in the background, without waiting
// for an answer.
//
// The picker calls it the moment the '#' mark is typed — before the reader has chosen a namespace,
// and long before a row is picked — because the answer costs a network round trip per node, and the
// typing that follows is exactly the time to spend on it. A namespace that has nothing to refresh
// (or a Source with no list at all) does nothing here.
func (s *Source) Warm() {
	if s == nil || s.list == nil {
		return
	}
	// Reading the list IS the request (see node.PeerList): the caller keeps the snapshot, this one
	// throws it away.
	s.list()
}

// Search ranks the nodes whose name, note or description matches query — a name hit
// first, because the name is the thing being typed. An empty query keeps every node
// this machine can NAME: that is the picker's opening list, and a node missing from
// it would read as "not configured".
//
// A node this machine holds no identity for is deliberately not offered here, and that is not the
// "never hide a peer" rule being broken (that rule is the REMINDER's and the board's, which list it
// with the reason): this picker exists to INSERT a reference, a reference carries an identity, and a
// row whose selection could only leave a dangling "#a:" in the message would be worse than no row —
// the reader would have to delete it. The machine is one probe away from being nameable, and the
// probe is exactly what opening this picker starts (Warm).
func (s *Source) Search(query string, limit int) []atfile.AgentTarget {
	if s == nil || s.list == nil {
		return nil
	}
	peers := s.list()

	type scored struct {
		target atfile.AgentTarget
		score  int
	}
	ranked := make([]scored, 0, len(peers))
	for _, p := range peers {
		if p.NodeID == "" {
			continue
		}
		score, ok := matchScore(query, p)
		if !ok {
			continue
		}
		ranked = append(ranked, scored{target: p.target(), score: score})
	}
	// Ties break on the name, not on insertion order: the list is whatever the directory merged,
	// and a picker whose rows shuffle between keystrokes is unusable.
	slices.SortStableFunc(ranked, func(a, b scored) int {
		if a.score != b.score {
			return b.score - a.score
		}
		return strings.Compare(a.target.Name, b.target.Name)
	})

	if limit = normalizeLimit(limit); len(ranked) > limit {
		ranked = ranked[:limit]
	}
	targets := make([]atfile.AgentTarget, len(ranked))
	for i, r := range ranked {
		targets[i] = r.target
	}
	return targets
}

// Resolve returns the node a #a: reference names: a node id, in full or as an unambiguous PREFIX of
// one, because a reader who saw an id in a transcript or on a picker row may well have copied only
// its front.
//
// ONLY identities resolve, and that is the same rule a tool argument follows (node/peers.go Lookup):
// a label belongs to whoever wrote the config, it may be changed, and a reference that quietly
// pointed at a DIFFERENT machine would be worse than one that stays visible and unexpanded — which
// is exactly what an unresolvable reference does (atfile). A prefix that names two machines is
// refused for the same reason; the ambiguity is the reader's to fix by typing more.
func (s *Source) Resolve(ref string) (atfile.AgentTarget, bool) {
	if s == nil || s.list == nil {
		return atfile.AgentTarget{}, false
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return atfile.AgentTarget{}, false
	}
	peers := s.list()
	ids := make([]string, 0, len(peers))
	byID := make(map[string]atfile.AgentTarget, len(peers))
	for _, p := range peers {
		if p.NodeID == "" {
			continue
		}
		ids = append(ids, p.NodeID)
		byID[p.NodeID] = p.target()
	}
	if id, ok, err := ResolveID(ref, ids); err == nil && ok {
		return byID[id], true
	}
	return atfile.AgentTarget{}, false
}

// target projects a Peer onto what an expansion needs. Reachability is left out on
// purpose (atfile.AgentTarget carries it for the picker only) — see the package doc.
func (p Peer) target() atfile.AgentTarget {
	return atfile.AgentTarget{
		Label:       p.Label,
		Name:        p.Name,
		NodeID:      p.NodeID,
		Note:        p.Note,
		Description: p.Description,
		Reachable:   p.Reachable,
		Checked:     p.Checked,
	}
}

// matchScore ranks one node against a query, higher first. An empty query matches
// everything at the same (lowest) score, which leaves the ordering to the sort. A query that could
// be the front of an identity is ranked as one, which is what makes a row's short form copyable.
func matchScore(query string, p Peer) (int, bool) {
	query = strings.TrimSpace(query)
	if query == "" {
		return 0, true
	}
	q := strings.ToLower(query)
	name := strings.ToLower(p.Name)
	switch {
	case name == q || strings.EqualFold(p.NodeID, query):
		return 4, true
	case strings.HasPrefix(name, q) || strings.HasPrefix(strings.ToLower(p.NodeID), q):
		return 3, true
	case strings.Contains(name, q):
		return 2, true
	case strings.Contains(strings.ToLower(p.Note), q),
		strings.Contains(strings.ToLower(p.Description), q):
		return 1, true
	default:
		return 0, false
	}
}

func normalizeLimit(limit int) int {
	if limit <= 0 {
		return DefaultLimit
	}
	return limit
}

// PrefixCandidate is one row the picker offers for the bare '#': the namespaces
// themselves, so the reader does not have to remember the spelling.
type PrefixCandidate struct {
	// Ref is the whole reference to insert, '#' and prefix included.
	Ref string
	// Title is the short label the row shows.
	Title string
	// Note is the one-line explanation of what that namespace names.
	Note string
}

// PrefixCandidates returns the '#' namespaces in the order the picker offers them.
//
// The spelling comes from atfile (RefForAgent/RefForSession build it), so the row
// that is inserted and the prefix the scan looks for cannot drift apart; the words
// live here because they are about the PICKER, which is this package's job.
func PrefixCandidates() []PrefixCandidate {
	return []PrefixCandidate{
		{
			Ref:   atfile.RefForAgent(""),
			Title: string(atfile.HashRefMark) + atfile.AgentRefPrefix,
			Note:  "agent：另一台机器上的 Tachi 节点，可把活派过去",
		},
		{
			Ref:   atfile.RefForSession(""),
			Title: string(atfile.HashRefMark) + atfile.SessionRefPrefix,
			Note:  "session：另一段对话（只引用它的位置，不内联全文）",
		},
	}
}

// PeerSource says how this machine came to know a peer, one bit per way — because the ways are not
// exclusive, and a reader asking "how do you know about it" wants all the answers there are: a
// machine can be written in the config AND be announcing itself, or have been found on the local
// network AND been vouched for by a neighbour.
//
// The three are the three suppliers of discovery candidates (docs/2026-10-15-a2a-gossip-design.md
// §一.1), and the words for them live here beside ReachWord's for the same reason: one fact, one
// phrase, every frontend.
type PeerSource uint8

const (
	// SourceConfigured: a person wrote this machine into this one's config. It is the only way a
	// peer is known WITHOUT evidence — the other two are things a network said — and the reason a
	// configured row never expires.
	SourceConfigured PeerSource = 1 << iota
	// SourceMDNS: this machine heard it announce itself on the local network, and reached it.
	SourceMDNS
	// SourceGossip: a peer this machine has itself reached named it. Note what is NOT here: which
	// peer said so. That is a log's business, not a panel's — a row has to stay one line, and the
	// identity that vouched rotates as the rounds go by.
	SourceGossip
)

// SourceWord is the source as a row states it: every way this machine knows the peer, in a fixed
// order, joined so that one row reads as one line. It is empty when there is nothing to state,
// which is the honest answer for a peer whose entries carry no source at all.
//
// The order is config-first because that is the one a reader can act on: a machine somebody put in
// the config is a machine somebody meant to have, and the two network ways are how it was found.
func SourceWord(s PeerSource) string {
	words := make([]string, 0, 3)
	if s&SourceConfigured != 0 {
		words = append(words, "配置")
	}
	if s&SourceMDNS != 0 {
		words = append(words, "mDNS")
	}
	if s&SourceGossip != 0 {
		words = append(words, "gossip")
	}
	return strings.Join(words, " · ")
}

// ReachWord is how a picker row states what the last look found — the same word in
// every frontend, because it is the same fact. "检查中…" is not a euphemism for
// unreachable: a node nobody has probed yet may be perfectly up, and a row that said
// otherwise would send the reader looking for a problem that is not there.
func ReachWord(p atfile.AgentTarget) string {
	switch {
	case !p.Checked:
		return "检查中…"
	case p.Reachable:
		return "可用"
	default:
		return "联系不上"
	}
}

// DefaultRowNoteRunes bounds a picker row's description: it is free text written by
// whoever runs that node, and a row has to stay one line.
const DefaultRowNoteRunes = 48

// idLabelChars is how much of a node id a picker row shows. Enough to tell a handful of
// machines apart at a glance, short enough to sit in a list column; the full id is in the
// reference the row expands into, which is where it can be copied from.
const idLabelChars = 8

// IDLabel is the identity as a picker row states it: the pinned node id, shortened —
// or the fact that this machine holds no record of one, which is a real answer rather than an empty
// cell (only outbound contact pins an identity).
func IDLabel(p atfile.AgentTarget) string {
	if p.NodeID == "" {
		return NoIdentityLabel
	}
	return ShortID(p.NodeID)
}

// NoIdentityLabel is what every frontend says for a peer this machine holds no identity for, in one
// place so the picker row, the reference pointer and the peer directory's own rows cannot drift into
// three spellings of the same fact. It is a real answer rather than an empty cell: only outbound
// contact pins an identity, so a peer that has only ever called US has none here.
const NoIdentityLabel = "身份未记录"

// ShortID is how a node id is shown where a whole one would not fit: its first characters. An empty
// id stays empty — "no identity" is a state, not a short one.
func ShortID(nodeID string) string {
	if len(nodeID) <= idLabelChars {
		return nodeID
	}
	return nodeID[:idLabelChars]
}

// ResolveID turns a node id, or an unambiguous PREFIX of one, into the id it names.
//
// ok is false when nothing matches; err is non-nil when the prefix names more than one machine, and
// it names the candidates. An ambiguous reference must be refused rather than guessed at — the guess
// would send work to a different machine — which is the rule @session: suffix resolution already
// follows. A full id always wins, so a prefix that is also somebody's whole id is not ambiguous.
func ResolveID(ref string, ids []string) (string, bool, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", false, nil
	}
	var matches []string
	for _, id := range ids {
		if id == ref {
			return id, true, nil
		}
		if strings.HasPrefix(id, ref) {
			matches = append(matches, id)
		}
	}
	switch len(matches) {
	case 0:
		return "", false, nil
	case 1:
		return matches[0], true, nil
	default:
		slices.Sort(matches)
		return "", false, fmt.Errorf("%q matches %d nodes: %s", ref, len(matches), strings.Join(matches, ", "))
	}
}

// RowNote renders a peer's one-line description for a picker row: the node's own
// account when it has one, the configured line otherwise.
func RowNote(p atfile.AgentTarget, maxRunes int) string {
	what := strings.TrimSpace(strutil.FirstLine(p.Description))
	if what == "" {
		what = strings.TrimSpace(strutil.FirstLine(p.Note))
	}
	if maxRunes <= 0 {
		maxRunes = DefaultRowNoteRunes
	}
	return strutil.TruncateFitted(what, maxRunes)
}
