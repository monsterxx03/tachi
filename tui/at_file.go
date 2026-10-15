package tui

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/agentref"
	"github.com/monsterxx03/tachi/agent/atfile"
	"github.com/monsterxx03/tachi/agent/sessionref"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/node"
	"github.com/monsterxx03/tachi/pkg/fileindex"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

// maxSessionRowTitle keeps a session row one line wide: a title is written from
// the first user message, so it can be a whole paragraph.
const maxSessionRowTitle = 48

// atMatch is one row of the reference-completion popup. Every namespace produces
// them — files under the working directory, this machine's nodes, the '#' namespaces
// themselves, other conversations — so the popup, its keys and the insertion have one
// shape to deal with.
type atMatch struct {
	// Ref is the whole reference the row inserts, the mark included. It comes from
	// the same helpers the expansion scanner reads back (atfile.RefForPath's
	// spelling, RefForAgent, RefForSession), so what the popup offers is what the
	// backend expands.
	Ref string
	// Label is what the row shows: a path, a node's alias and identity, a '#' namespace,
	// or a session's date and title.
	Label string
	// Note is the row's one-line explanation, when the namespace has something to say about the
	// row (a near node's own description, what a '#' namespace is for).
	Note  string
	IsDir bool
	// Session, Agent and Prefix mark which namespace a row came from — that is what names the
	// popup's header, so the header cannot disagree with the rows.
	Session bool
	Agent   bool
	// Prefix marks a row that is a NAMESPACE rather than a target: the '#' picker's opening list.
	// Picking one OPENS that namespace instead of finishing the reference, so the popup stays up
	// and nothing separates the inserted text from what comes next.
	Prefix bool
}

// newSessionSource wires the #s: namespace to the agent's session store:
// resolving a reference and searching the popup both read through it, and its
// list is memoized (one snapshot until a write, not one directory walk per
// keystroke — see docs/agents/session-store.md).
//
// nil means the session namespace offers nothing, which is what a frontend with
// no session manager gets.
func newSessionSource(a *agent.AIAgent) *sessionref.Source {
	if a == nil || a.SessionManager() == nil {
		return nil
	}
	dir, err := config.SessionDir()
	if err != nil {
		return nil
	}
	return sessionref.New(a.SessionManager().List, dir)
}

// newAgentSource wires the #a: namespace to this machine's nodes. Reading the list
// is also what ASKS for a fresh one: the snapshot is instant and may say "checking…", while the
// probe behind it refreshes in the background (see node.PeerList) — which is the only ordering a
// keystroke can afford.
//
// nil means the node namespace offers nothing, which is what a frontend with no
// node side gets: the '#' picker still offers the namespace, and it comes back empty.
func newAgentSource(peers *node.PeerList) *agentref.Source {
	if peers == nil {
		return nil
	}
	return agentref.New(func() []agentref.Peer {
		peers.Probe()
		return peers.Snapshot()
	})
}

// syncAgentSource wires the '#' picker's node namespace to this machine's peers. It is called when
// the config becomes available and when it changes, because the peers list is a leaf the reader can
// edit while the process runs — and a picker still offering a machine the config no longer names
// would be lying about what this machine can reach.
//
// The peers and the sender are resolved HERE, on the Update goroutine, and the list holds those
// values rather than reaching back into the model: the probe runs in the background, and reading the
// model's config from another goroutine is a race the reader can trigger by switching provider
// mid-probe. A fresh list per config is the other half of that: the probe's results are verdicts
// about the machines that used to be listed, and keeping them across a change would label a renamed
// node with a stranger's answer.
func (m *Model) syncAgentSource() {
	if m == nil || m.cfg == nil {
		m.input.SetAgentSource(nil)
		return
	}
	var probe func(ctx context.Context, force bool) []node.PeerReport
	if sender := m.sender(); sender != nil {
		probe = sender.PeerReports
	}
	peers := node.NewPeerList(node.RowsOf(m.sender), probe, m.agent.Logger())
	// Ask the nodes who they are NOW, in the background, and this is the whole of the startup
	// warmup: the first '#' opens on rows that were already answered (Probe is asynchronous and
	// rate-limited — node.PeerList), and reading those rows is what resolves the sending side, so
	// the discovery browser comes up with them. What the round also leaves behind is the pins and
	// the card cache the first turn's reminder reads (node.DelegatorCache.WarmUp, the same round
	// without a list to land in).
	peers.Probe()
	m.input.SetAgentSource(newAgentSource(peers))
}

// sender is this machine's sending side as the CONCRETE sender type, for the callers that need more
// than the tool layer's contract (the node board shows each peer's card and the address this machine
// dials). It goes through the same cache the delegating paths use, so both see one sender.
func (m *Model) sender() *node.Sender {
	if m.cfg == nil {
		return nil
	}
	if m.delegators == nil {
		m.delegators = node.NewDelegatorCache(m.cfg.Node.Name, node.DiscoveryOf(m.cfg), m.agent.Logger(), nil)
	}
	return m.delegators.ForSender(context.Background(), m.cfg.Peers)
}

// atRowsFor routes one popup query to its namespace: the mark says which kind of
// question is being asked ('@' a path, '#' an entity), and for '#' the typed prefix
// says which namespace. The routing itself lives in atfile, so the TUI and the desktop
// picker cannot answer the same keystrokes differently.
func (i *InputArea) atRowsFor(mark byte, query string) []atMatch {
	if mark != atfile.HashRefMark {
		return i.atFileRows(query)
	}
	// Typing '#' is the moment to start asking the nodes: the answer is a round trip per node, the
	// reader is about to spend a second choosing a namespace, and the rows that matter are the ones
	// they see when they get there.
	i.agents.Warm()
	switch ns, rest := atfile.HashQueryNamespace(query); ns {
	case atfile.HashNamespaceAgents:
		return i.atAgentRows(rest)
	case atfile.HashNamespaceSessions:
		return i.atSessionRows(rest)
	case atfile.HashNamespaceCandidates:
		return i.atPrefixRows(query)
	default:
		// Not a reference at all ("#include", "#123"): nothing to offer, and the popup stays
		// closed so ordinary text is not interrupted.
		return nil
	}
}

// atPrefixRows are the '#' namespaces themselves, offered as soon as the mark is typed
// so the reader does not have to remember the spelling. Typing part of a prefix narrows this
// list, which is what makes "#" → "#a:" → the node list feel like one gesture.
func (i *InputArea) atPrefixRows(query string) []atMatch {
	typed := string(atfile.HashRefMark) + query
	var rows []atMatch
	for _, c := range agentref.PrefixCandidates() {
		if !strings.HasPrefix(c.Title, typed) {
			continue
		}
		rows = append(rows, atMatch{Ref: c.Ref, Label: c.Title, Note: c.Note, Prefix: true})
	}
	return rows
}

// atAgentRows renders this machine's nodes as popup rows. A node that cannot be
// reached is still listed — "not listed" would read as "not configured", the same rule the peers
// reminder follows — carrying what the last look found.
func (i *InputArea) atAgentRows(query string) []atMatch {
	if i.agents == nil {
		return nil
	}
	targets := i.agents.Search(query, fileindex.DefaultLimit)
	rows := make([]atMatch, len(targets))
	for idx, t := range targets {
		rows[idx] = atMatch{
			Ref:   atfile.RefForAgent(t.NodeID),
			Label: agentRowLabel(t),
			Note:  agentref.RowNote(t, 0),
			Agent: true,
		}
	}
	return rows
}

// agentRowLabel is what a node row shows: what the node is called, the identity this machine pinned
// for it — the string a tool argument takes, so the row is where it is read from — and whether the
// last look found it up.
//
// The identity is spelled out rather than omitted, and it is NEVER the name the node reports about
// itself: it is the one fact a peer cannot choose, which is exactly what makes it worth a column. A
// node this machine has never called outbound has no pinned id, and the row says so — the two
// machines may well be able to reach each other, but this one has no record of who is there, and
// without one the node cannot be named at all.
func agentRowLabel(t atfile.AgentTarget) string {
	return fmt.Sprintf("%s  %s  %s", t.Name, agentref.IDLabel(t), agentref.ReachWord(t))
}
// this stays cheap enough to run on every keystroke.
func (i *InputArea) searchAtFiles(query string) ([]fileindex.Match, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return i.files.Search(cwd, query, fileindex.DefaultLimit)
}

// atFileRows renders the file matches for query as popup rows.
func (i *InputArea) atFileRows(query string) []atMatch {
	matches, err := i.searchAtFiles(query)
	if err != nil {
		return nil
	}
	rows := make([]atMatch, len(matches))
	for idx, m := range matches {
		rows[idx] = atMatch{Ref: "@" + m.Path, Label: m.Path, IsDir: m.IsDir}
	}
	return rows
}

// atSessionRows renders other conversations as popup rows, ranked by the store
// (a match on the title, the id or the working directory's name).
func (i *InputArea) atSessionRows(query string) []atMatch {
	if i.sessions == nil {
		return nil
	}
	targets := i.sessions.Search(query, i.currentSessionID, fileindex.DefaultLimit)
	rows := make([]atMatch, len(targets))
	for idx, t := range targets {
		rows[idx] = atMatch{Ref: atfile.RefForSession(t.ID), Label: sessionRowLabel(t), Session: true}
	}
	return rows
}

// sessionRowLabel is what a session row shows: when it was last touched and what
// it is called — the two things a reader picks a conversation by.
//
// A session a SUBSYSTEM owns says so on the row. The title it would otherwise show was written
// from the remote prompt, so a row without this marking would present someone else's execution
// record as a conversation the reader had (design §2.1) — and the pointer this row expands into
// already carries the same fact (atfile.SessionTarget.ManagedKind), because the two sides are
// required to tell one story about one session.
func sessionRowLabel(t atfile.SessionTarget) string {
	title := t.Title
	if title == "" {
		title = "(untitled)"
	}
	title = strutil.TruncateFitted(title, maxSessionRowTitle)
	if t.UpdatedAt.IsZero() {
		return title
	}
	return t.UpdatedAt.Format(strutil.TimeFormatDateTimeShort) + "  " + title + managedRowSuffix(t.ManagedKind)
}

// managedRowSuffix is the marking a picker row carries for a session a subsystem owns, empty for
// an ordinary conversation. One spelling, used by the session picker and the #s: parser
// alike — a second one is how the two would start disagreeing.
func managedRowSuffix(kind string) string {
	if kind == "" {
		return ""
	}
	return fmt.Sprintf("  [子系统执行记录 %s · 只读]", kind)
}

// ExpandAtReferences expands the references in a user message for the LLM: '@' names a path (text
// files and directories are inlined, images become content parts for multi-modal input, binaries
// are annotated by path), and '#' names an entity — #s: becomes a pointer to another conversation's
// transcript, #a: a pointer to a node and what to do with it. Neither entity is ever inlined. The
// TUI keeps displaying the unexpanded text.
func (m *Model) ExpandAtReferences(message string) atfile.Result {
	cwd, _ := os.Getwd()
	result := atfile.Expand(cwd, message, atfile.Options{
		Sessions: m.input.SessionSource(),
		Agents:   m.input.AgentSource(),
	})
	if result.Refs > 0 {
		m.logger.Info(context.Background(), "at_file: expanded references in message",
			"refs", result.Refs, "images", len(result.Images))
	}
	return result
}
