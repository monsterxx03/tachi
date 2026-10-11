// Package agentref resolves #a: references and backs the node picker: "which of
// my machines is this node" is answered from what THIS machine knows about its
// peers, never from what a peer says about itself.
//
// Two facts make up an answer, and they come from two different places on purpose:
//
//   - the ALIAS and the note are configuration — this machine's own name for a peer, the string
//     DelegateTask takes;
//   - the NODE ID is a record (config.PeerIdentitiesPath), written only by a TLS handshake on an
//     OUTBOUND call. A node this machine has never called therefore has no identity here, and
//     that is a correct answer rather than missing data: the alternative — the name the peer
//     sends — can be claimed by any key (design §4.2).
//
// Reachability is a third thing, and it is deliberately the odd one out: it is a fact about a
// MOMENT, so it is refreshed in the background and read as a snapshot (see node.PeerList). A
// picker row may show it; the text a reference expands into may not, because that text outlives
// the moment.
package agentref

import (
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
	// Alias is the configured peer name — the model's argument to DelegateTask.
	Alias string
	// NodeID is the identity pinned on first outbound contact; empty when this machine has never
	// called that node.
	NodeID string
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

// Search ranks the nodes whose alias, note or description matches query — an alias
// hit first, because the alias is the thing being typed. An empty query keeps every
// node: that is the picker's opening list, and a node missing from it would read as
// "not configured".
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
		score, ok := matchScore(query, p)
		if !ok {
			continue
		}
		ranked = append(ranked, scored{target: p.target(), score: score})
	}
	// Ties break on the alias, not on insertion order: the list comes from configuration, where
	// nothing promises an order, and a picker whose rows shuffle between keystrokes is unusable.
	slices.SortStableFunc(ranked, func(a, b scored) int {
		if a.score != b.score {
			return b.score - a.score
		}
		return strings.Compare(a.target.Alias, b.target.Alias)
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

// Resolve returns the node a reference names: a configured alias, or a node id —
// the id is what a reader who saw one in a transcript (or in the pointer a previous
// reference expanded into) would paste back.
//
// An unknown name resolves to nothing rather than to a near miss: the reader typed
// something this machine does not know, and a reference quietly pointing at a
// DIFFERENT machine is worse than one that stays visible and unexpanded.
func (s *Source) Resolve(name string) (atfile.AgentTarget, bool) {
	if s == nil || s.list == nil {
		return atfile.AgentTarget{}, false
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return atfile.AgentTarget{}, false
	}
	peers := s.list()
	for _, p := range peers {
		if p.Alias == name || (p.NodeID != "" && p.NodeID == name) {
			return p.target(), true
		}
	}
	// Case only: a config alias is typed by hand in two places (the config file and
	// the message), and "NAS" for "nas" is the same machine.
	for _, p := range peers {
		if strings.EqualFold(p.Alias, name) {
			return p.target(), true
		}
	}
	return atfile.AgentTarget{}, false
}

// target projects a Peer onto what an expansion needs. Reachability is left out on
// purpose (atfile.AgentTarget carries it for the picker only) — see the package doc.
func (p Peer) target() atfile.AgentTarget {
	return atfile.AgentTarget{
		Alias:       p.Alias,
		NodeID:      p.NodeID,
		Note:        p.Note,
		Description: p.Description,
		Reachable:   p.Reachable,
		Checked:     p.Checked,
	}
}

// matchScore ranks one node against a query, higher first. An empty query matches
// everything at the same (lowest) score, which leaves the alias ordering to the sort.
func matchScore(query string, p Peer) (int, bool) {
	query = strings.TrimSpace(query)
	if query == "" {
		return 0, true
	}
	q := strings.ToLower(query)
	alias := strings.ToLower(p.Alias)
	switch {
	case alias == q:
		return 4, true
	case strings.HasPrefix(alias, q):
		return 3, true
	case strings.Contains(alias, q):
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
		return "身份未记录"
	}
	if len(p.NodeID) <= idLabelChars {
		return p.NodeID
	}
	return p.NodeID[:idLabelChars]
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
