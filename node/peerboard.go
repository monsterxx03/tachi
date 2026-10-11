package node

import (
	"context"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// What the node board shows. It exists because two different questions get asked about a peer, and
// they must not be answered by the same shape:
//
//   - the tools and the reminder ask "may I delegate here, and what does that node do" — they want
//     a NAME and a one-line description (tools.PeerSummary);
//   - a person looking at the board asks "is that machine up, and what exactly is it" — they want
//     the address this machine dials, the identity THIS machine pinned, and the card in full.
//
// Keeping them apart is why the board's report carries no promise about a delegation and the
// tool-facing summary carries no address or card.

// PeerCard is what a node says about ITSELF, as much of it as the board shows.
//
// Every field here is self-reported, and the names say so rather than leaving a reader to assume:
// a name, a description, a version and a skill list are all things any key can claim, which is why
// the identity a reader should actually judge by is the pinned node id beside them (design §4.2).
type PeerCard struct {
	Name        string
	Description string
	Version     string
	// Interfaces are the entry points the card advertises (address, binding, protocol version) —
	// also its own account, and sometimes not what this machine can actually reach.
	Interfaces []PeerInterface
	// Skills are what it says it can do.
	Skills []PeerSkill
	// Capabilities are what it says it supports, already worded for a person.
	Capabilities []string
}

// PeerInterface is one advertised entry point.
type PeerInterface struct {
	URL             string
	ProtocolBinding string
	ProtocolVersion string
}

// PeerSkill is one thing a node says it can do.
type PeerSkill struct {
	Name        string
	Description string
}

// PeerReport is everything this machine's sending side can say about one peer: what the
// configuration asks it to reach, what the node answered, and why there is no answer when there is
// none.
//
// Reachable is "the card was read", and that is deliberately the whole test: it is the only thing
// that proves the machine on the other end is speaking A2A right now. It is a fact about a MOMENT,
// so a caller renders it with when it was taken.
type PeerReport struct {
	Alias string
	// URL is where THIS machine dials the peer (config `peers[].url`), not what the card claims.
	URL  string
	Note string
	// NodeID is the identity pinned on first outbound contact; empty when this machine has never
	// called that node (only outbound contact pins one).
	NodeID    string
	Reachable bool
	// Card is nil when the node did not answer.
	Card *PeerCard
	// Err is why there is no card (empty when there is one). An unreachable peer is NOT an error
	// of the board's: it is one of the answers.
	Err string
}

// PeerReports describes every configured peer, asking the network only when force asks it to.
//
// force (the board's refresh button) also throws away the card cache: a human pressed a button and
// is waiting, and answering that with a five-minute-old cached card would be answering a different
// question. Without force the cache stands, which is what keeps the picker's per-keystroke probe
// from costing a request per node.
//
// The peers are asked in PARALLEL (peerDirectory.Snapshot): the failures are what cost time, and a
// sequential walk pays every dead node's timeout one after another.
func (s *Sender) PeerReports(ctx context.Context, force bool) []PeerReport {
	if s == nil {
		return nil
	}
	if force {
		s.peers.forgetCards()
	}
	pinned := pinnedNodeIDs()
	snapshot := s.peers.Snapshot(ctx)
	out := make([]PeerReport, 0, len(snapshot))
	for _, p := range snapshot {
		report := PeerReport{
			Alias:     p.Name,
			URL:       p.URL,
			Note:      p.Note,
			NodeID:    pinned[p.Name],
			Reachable: p.Card != nil,
			Card:      summarizeCard(p.Card),
		}
		if p.Err != nil {
			report.Err = p.Err.Error()
		}
		out = append(out, report)
	}
	return out
}

// summarizeCard projects an Agent Card onto what the board shows, dropping the parts that are
// protocol plumbing. A nil card stays nil: "no answer" and "an empty card" are different states.
func summarizeCard(card *a2a.AgentCard) *PeerCard {
	if card == nil {
		return nil
	}
	out := &PeerCard{
		Name:        card.Name,
		Description: card.Description,
		Version:     card.Version,
	}
	for _, iface := range card.SupportedInterfaces {
		if iface == nil {
			continue
		}
		out.Interfaces = append(out.Interfaces, PeerInterface{
			URL:             iface.URL,
			ProtocolBinding: string(iface.ProtocolBinding),
			ProtocolVersion: string(iface.ProtocolVersion),
		})
	}
	for _, skill := range card.Skills {
		out.Skills = append(out.Skills, PeerSkill{Name: skill.Name, Description: skill.Description})
	}
	// Booleans become words here, once, so every frontend writes the same two: the card's own
	// vocabulary (Streaming/PushNotifications) is about the protocol, not about what a person
	// gets from it.
	if card.Capabilities.Streaming {
		out.Capabilities = append(out.Capabilities, "流式响应")
	}
	if card.Capabilities.PushNotifications {
		out.Capabilities = append(out.Capabilities, "推送通知")
	}
	if card.Capabilities.ExtendedAgentCard {
		out.Capabilities = append(out.Capabilities, "扩展卡片")
	}
	return out
}
