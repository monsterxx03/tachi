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
	Alias string `json:"alias"`
	// NodeID/NodeIDLabel are the pinned identity: full for a tooltip, short for the row. Empty and
	// "身份未记录" when this machine has never called that node outbound.
	NodeID      string `json:"nodeId"`
	NodeIDLabel string `json:"nodeIdLabel"`
	// URL is where this machine dials the peer (config), never what the card claims.
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
	// offer the same spelling the picker inserts.
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

func boardVO(list *node.PeerList) PeerBoardVO {
	details := list.Details()
	out := PeerBoardVO{Peers: make([]PeerBoardRowVO, 0, len(details))}
	var checked time.Time
	for _, d := range details {
		row := PeerBoardRowVO{
			Alias:       d.Alias,
			NodeID:      d.NodeID,
			NodeIDLabel: agentref.IDLabel(atfile.AgentTarget{NodeID: d.NodeID}),
			URL:         d.URL,
			Note:        d.Note,
			Reachable:   d.Reachable,
			Checked:     d.Checked,
			Err:         d.Err,
			Card:        cardVO(d.Card),
			Ref:         atfile.RefForAgent(d.Alias),
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
