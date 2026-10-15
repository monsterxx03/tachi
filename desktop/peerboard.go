package main

// The node board: what this machine knows about the machines it may delegate to, shown as a panel
// hanging off the composer's status row (the MCP panel's shape, for the same reason — one trigger
// beside the conversation, and the panel anchored to it).
//
// Three things it deliberately shows TOGETHER, because apart they are each misleading:
//
//   - the alias and the address THIS machine dials (configuration — what we asked for);
//   - the identity pinned by the handshake (the one fact a peer cannot claim) beside the name and
//     description the node sends about ITSELF (which any key can claim);
//   - whether it answered just now, and WHEN that was asked.

import (
	"context"
	"time"

	"github.com/monsterxx03/tachi/agent/agentref"
	"github.com/monsterxx03/tachi/agent/atfile"
	"github.com/monsterxx03/tachi/node"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

// peersChangedEvent is what the frontend listens for: the node reading moved, so everything drawn
// from it — the panel and the count on its trigger — is out of date. The event carries NOTHING, and
// that is deliberate: a payload would be a second copy of a row, read at a slightly different moment
// than the board itself (PeerBoard), and the two would disagree for no reason.
const peersChangedEvent = "agent:peers_changed"

// notifyPeersChanged says that the reading moved.
//
// It exists because the two views of this reading are snapshots nobody re-reads by themselves: the
// panel reads once when it opens and knows no other moment, and the count on its trigger reads with
// it. So without this, a machine discovery had just found showed up only after the panel was reopened
// — and the probe the open kicked off (whose answers are what turn 检查中 into a verdict) landed with
// nobody listening, leaving the row on 检查中… until 刷新 was pressed.
//
// What decides "moved" is the reading itself, never a clock: node.PeerList calls this only for a
// round of answers that differs from the last one, and the discovery browser only for a machine that
// entered the directory (or told this one it lives somewhere new). A repeating announcement, or a
// probe that says what the last one said, is silence — otherwise every re-read would ask for another
// probe and the pair would tick for ever.
func (d *desktopApp) notifyPeersChanged() {
	if d.app == nil {
		return
	}
	d.app.Event.Emit(peersChangedEvent, map[string]any{})
}

// peerDiscovered is what the discovery browser says when a machine it had not listed enters the
// directory — see node.DelegatorCache's onSighting.
//
// Two things follow, and both are about a reader who is already looking somewhere: the views are
// re-read (the row appears, as 检查中… — nothing has asked it anything yet), and the new machine is
// asked who it is, so that its verdict follows a moment later instead of waiting for a manual 刷新.
// The probe is the same rate-limited one the picker uses, so a chatty network cannot turn
// announcements into a request per announcement.
func (d *desktopApp) peerDiscovered() {
	d.notifyPeersChanged()
	if list := d.peerList(); list != nil {
		list.Probe()
	}
}

// PeerBoardVO is the whole panel: this machine's nodes, and when they were last looked at.
type PeerBoardVO struct {
	Peers []PeerBoardRowVO `json:"peers"`
	// CheckedAt is when the answers were taken, "" when nobody has asked yet. It is stamped on the
	// ROUND rather than on a row, because one probe answers for all of them.
	CheckedAt string `json:"checkedAt"`
	// AnyChecked is false before the first probe, which the panel says out loud instead of
	// rendering an empty state that looks like "no peers".
	AnyChecked bool `json:"anyChecked"`
}

// PeerBoardRowVO is one node as the panel draws it.
type PeerBoardRowVO struct {
	// Label is the name a config line gave the node, empty when it gave none.
	Label string `json:"label"`
	// Name is what to call it on the row: the label, else what the node calls itself, else the front
	// of its id. The row shows this, and never shows the node's own name as though it were known.
	Name string `json:"name"`
	// NodeID/NodeIDLabel are the pinned identity: full for a tooltip, short for the row. Empty and
	// "身份未记录" when this machine has never called that node outbound.
	NodeID      string `json:"nodeId"`
	NodeIDLabel string `json:"nodeIdLabel"`
	// URL is where this machine will dial FIRST — the address the node advertises about itself when
	// it advertises one, the configured address otherwise. Never what the card claims as the only
	// truth: it is one candidate among the ones this machine holds.
	URL  string `json:"url"`
	Note string `json:"note"`
	// Reach and Reachable are the last look's verdict, in the same words the picker uses.
	Reach     string `json:"reach"`
	Reachable bool   `json:"reachable"`
	// Checked separates "not looked at yet" from "looked at and down".
	Checked   bool   `json:"checked"`
	CheckedAt string `json:"checkedAt"`
	// Card is what the node says about itself, null when it did not answer.
	Card *PeerCardVO `json:"card"`
	// Err says why there is no card (hidden while there is one).
	Err string `json:"err"`
	// Ref is the reference that names this node in a message (atfile.RefForAgent), so the panel can
	// offer the same spelling the picker inserts. It is EMPTY for a node this machine holds no
	// identity for, and that is the same answer the picker gives (agentref.Search): there is nothing
	// to insert, and a bare "#a:" would be a mark that only looks like one. The identity column
	// beside it says why.
	Ref string `json:"ref"`
}

// PeerCardVO is the node's own account of itself, as much as the panel shows. Every field is
// self-reported.
type PeerCardVO struct {
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	Version      string            `json:"version"`
	Interfaces   []PeerInterfaceVO `json:"interfaces"`
	Skills       []PeerSkillVO     `json:"skills"`
	Capabilities []string          `json:"capabilities"`
}

// PeerInterfaceVO is one entry point the card advertises.
type PeerInterfaceVO struct {
	URL             string `json:"url"`
	ProtocolBinding string `json:"protocolBinding"`
	ProtocolVersion string `json:"protocolVersion"`
}

// PeerSkillVO is one thing the node says it can do.
type PeerSkillVO struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// PeerBoard returns the panel as it stands. It never waits for the network: the answers are the
// last probe's, and a fresh probe is started so that a panel left open catches up.
func (s *AgentService) PeerBoard() PeerBoardVO {
	list := s.desk.peerList()
	if list == nil {
		return PeerBoardVO{}
	}
	// A panel opened for the first time has no answers yet; this is what fills it in.
	list.Probe()
	return boardVO(list)
}

// RefreshPeerBoard asks every node what it is, WAITS for the answers, and returns them: this is the
// panel's 刷新 button, where a person pressed something and a spinner is honest. It also throws away
// the cached cards (node.Sender.PeerReports), because answering "what is it now" with a
// five-minute-old read would be answering a different question.
func (s *AgentService) RefreshPeerBoard() PeerBoardVO {
	list := s.desk.peerList()
	if list == nil {
		return PeerBoardVO{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), node.RefreshTimeout)
	defer cancel()
	list.Refresh(ctx)
	return boardVO(list)
}

// refForRow is the reference a panel row offers: the node's identity, or NOTHING when this machine
// holds none for it. An empty identity rendered as a reference would be `#a:` — a mark with nothing
// after it, which inserts text the expansion cannot resolve and the reader would only have to delete
// (the picker refuses such a row for the same reason, agentref.Search). The row's identity column
// says the fact instead.
func refForRow(nodeID string) string {
	if nodeID == "" {
		return ""
	}
	return atfile.RefForAgent(nodeID)
}

func boardVO(list *node.PeerList) PeerBoardVO {
	details := list.Details()
	out := PeerBoardVO{Peers: make([]PeerBoardRowVO, 0, len(details))}
	var checked time.Time
	for _, d := range details {
		row := PeerBoardRowVO{
			Label:       d.Label,
			Name:        d.Name,
			NodeID:      d.NodeID,
			NodeIDLabel: agentref.IDLabel(atfile.AgentTarget{NodeID: d.NodeID}),
			URL:         d.URL,
			Note:        d.Note,
			Reachable:   d.Reachable,
			Checked:     d.Checked,
			Err:         d.Err,
			Card:        cardVO(d.Card),
			// The reference names the node by IDENTITY, because that is what a tool argument takes;
			// the label rides along only as something to read. A node with no identity has no
			// reference to give — the panel leaves the field empty rather than handing out a `#a:`
			// with nothing after it, which is what the picker does too (agentref.Search).
			Ref: refForRow(d.NodeID),
			// The words come from agentref, the same ones the picker's rows use: one fact, one
			// phrase, three frontends.
			Reach: agentref.ReachWord(atfile.AgentTarget{Reachable: d.Reachable, Checked: d.Checked}),
		}
		if d.Checked {
			row.CheckedAt = d.CheckedAt.Format(strutil.TimeFormatDateTimeShort)
			if d.CheckedAt.After(checked) {
				checked = d.CheckedAt
			}
			out.AnyChecked = true
		}
		out.Peers = append(out.Peers, row)
	}
	if !checked.IsZero() {
		out.CheckedAt = checked.Format(strutil.TimeFormatDateTimeShort)
	}
	return out
}

func cardVO(card *node.PeerCard) *PeerCardVO {
	if card == nil {
		return nil
	}
	out := &PeerCardVO{
		Name:         card.Name,
		Description:  card.Description,
		Version:      card.Version,
		Capabilities: card.Capabilities,
	}
	for _, iface := range card.Interfaces {
		out.Interfaces = append(out.Interfaces, PeerInterfaceVO{
			URL:             iface.URL,
			ProtocolBinding: iface.ProtocolBinding,
			ProtocolVersion: iface.ProtocolVersion,
		})
	}
	for _, skill := range card.Skills {
		out.Skills = append(out.Skills, PeerSkillVO{Name: skill.Name, Description: skill.Description})
	}
	return out
}
