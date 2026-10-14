package node

import (
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/monsterxx03/tachi/agent/agentref"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

// The peer directory: which machines this machine knows about, what it calls each of them, and where
// it can reach them.
//
// TWO THINGS, and they must not be confused (design §三):
//
//   - the PIN (node/knownpeers.go) is the durable fact: which identity a machine proved, keyed by
//     node id, never deleted. It is what makes the second conversation with a peer a check rather
//     than a fresh trust.
//   - the DIRECTORY is a VIEW over three sources — the config's lines, the pins, and whatever
//     discovery has just seen — rebuilt on demand and holding nothing that cannot be derived.
//
// The identity is the key of the view, and everything a person reads is a label. That split is what
// lets a rename be a rename (the key never moves), lets a discovered machine (no config line,
// therefore no name to key by) live beside a configured one, and lets the ONE fact a peer cannot
// choose — its identity — be the thing a delegation names.

const (
	// maxDiscoveredPeers caps the discovered half of the directory. Anyone on the local network can
	// multicast, so the number of machines discovery can add is not something this machine chooses —
	// and a table that grows with a stranger's packet rate is a table that has no bound at all.
	// Configured peers are not counted against it and are never dropped: those are a person's.
	maxDiscoveredPeers = 64

	// selfNameMaxRunes bounds a peer's SELF-DECLARED name wherever it is shown. The name is text a
	// peer writes about itself, so its length is not something this machine gets to assume; the
	// identity beside it is the field with a fixed size.
	selfNameMaxRunes = 40
)

// peerEntry is one machine in this machine's peer directory, as every reader of "which nodes are
// there" is handed it — the reminder, the picker, the board, and the resolution behind a delegation.
//
// It carries both halves of the answer and keeps them apart: what this machine VERIFIED (the
// identity, out of a TLS handshake) and what a peer SAYS about itself (its name). A row shows the
// second and always appends the first, because the first is the only half nobody else can claim.
type peerEntry struct {
	// Identity is the node id this machine has pinned for the peer, empty until it has one. A peer
	// reached only in plaintext (loopback) never gets one — there is no certificate to derive it
	// from — and neither does a config line nobody has contacted yet.
	Identity string
	// Label is the name a config line gave it. Optional, display only.
	Label string
	// Note is the config line's human line, used when the peer's own description is missing.
	Note string
	// SelfName is what the peer calls itself in its Card. Untrusted: any key can claim any name, it
	// may change whenever the operator of that machine likes, and every machine's default is the
	// same word. Read it as a claim, never as an identification.
	SelfName string
	// Addrs is where this machine may dial the peer, in the order it should try them: what the peer
	// says about itself first, the config line last (the rule is stated once, in prependAddrs and
	// combineEntries).
	Addrs []string
	// cfgAddr is the address a config line names, when one does. Kept apart from Addrs because it
	// is a different fact: Addrs is where the machine might BE, this is where this machine was told
	// to look — and it is the address the pin is filed under (node/knownpeers.go pinnedIdentityAt).
	cfgAddr string
	// Configured and Discovered say where this entry came from, and they are not exclusive: one
	// machine that has a config line AND is announcing itself is a single entry with both set.
	Configured bool
	Discovered bool
	// LastSeen is when discovery last saw it, which is what the discovered half expires on.
	LastSeen time.Time
	// Card is the peer's own description of itself, nil when it could not be read.
	Card *a2a.AgentCard
	// Err is why the Card is missing (nil when there is one).
	Err error
}

// Name is what to CALL this peer in a sentence: the label a person gave it, then what it calls
// itself, then — when nothing else is left — the short form of its identity.
//
// The middle step is deliberately second: a peer's own name is a claim, but a claim is still better
// than hex for reading, and the identity is printed beside it wherever this is used.
func (e peerEntry) Name() string {
	if label := strings.TrimSpace(e.Label); label != "" {
		return label
	}
	if self := strutil.OneLine(e.SelfName, selfNameMaxRunes); self != "" {
		return self
	}
	if e.Identity != "" {
		return agentref.ShortID(e.Identity)
	}
	return "未知节点"
}

// Display is the peer as a row, a reminder line or a log line states it: the name, and the identity
// in parentheses.
//
// The identity is not decoration and not optional: every machine's default name is the same, a name
// is whatever its owner says it is, and the parentheses are the only part of the string that cannot
// be claimed by someone else. A peer this machine holds no identity for says so in words instead of
// leaving the parentheses out — "we have not reached it yet" is an answer, and an empty pair would
// read as one too.
func (e peerEntry) Display() string {
	if e.Identity == "" {
		return e.Name() + " (" + agentref.NoIdentityLabel + ")"
	}
	return e.Name() + " (" + agentref.ShortID(e.Identity) + ")"
}

// configAddr is the address a config line names, or "" when no line does — which is the address the
// pin records, so that a config line can find its identity again before the two have spoken in this
// process (node/knownpeers.go pinnedIdentityAt).
func (e peerEntry) configAddr() string { return e.cfgAddr }

// pinAddr is the address a first contact files its pin under: the config line's when there is one,
// the address actually dialled otherwise.
//
// The configured address wins for a reason that is only visible one process later — the pin's address
// is how a config line finds its identity BEFORE this machine has spoken to that peer again
// (pinnedIdentityAt). Filing a pin under an address the peer advertised about itself would leave the
// config line unable to recognise the identity it made, and a restart would show the machine as
// unknown until the first turn happened to contact it.
func pinAddr(e peerEntry, dialed string) string {
	if cfg := e.configAddr(); cfg != "" {
		return cfg
	}
	return dialed
}

// key is the entry as a MAP KEY for a probe's answers, which is a different question from "which
// machine is this": an identity is the answer when there is one, and otherwise the address that was
// dialled — the two things an answer can be about before a handshake has happened.
func (e peerEntry) key() string {
	if e.Identity != "" {
		return e.Identity
	}
	return "@" + e.configAddr()
}

// Reachable is whether the card could be read, which is the whole test and deliberately so: it is
// the only thing that proves the machine at the other end is speaking A2A right now. It is a fact
// about a MOMENT.
func (e peerEntry) Reachable() bool { return e.Card != nil }

// discoveredPeer is one machine discovery has SEEN on the local network — the shape the browser
// produces, so that the browser itself stays outside this package's idea of a directory (and can be
// faked in a test without a network).
//
// What it may NOT carry is a name: a multicast announcement is text anyone can send, and the two
// facts this machine needs from it are "which identity" and "at which addresses", both of which are
// CHECKED afterwards (the identity against the peer's certificate, the addresses by dialling them).
// Design §5.
type discoveredPeer struct {
	// Identity is the node id the announcement claims (TXT `id`). It is a CLAIM until a handshake
	// confirms it: a discovery sighting may never enter the directory on its own say-so.
	Identity string
	// Addrs are the addresses the announcement resolved to (SRV + A/AAAA).
	Addrs []string
	// Seen is when this was last observed.
	Seen time.Time
}

// build merges the directory's sources into the list every reader sees, without touching the
// network: it is the path a picker calls on every keystroke.
func (d *peerDirectory) build() []peerEntry {
	if d == nil {
		return nil
	}
	b := &dirBuilder{}
	// 1. The config's lines: a person's own statement of who exists, so they are the entries that
	// never expire and that keep their place in the list.
	for _, p := range d.peers {
		// The identity this line has already proved, if any. Before the first contact there is
		// none — the address is all either side shares — and the row says so.
		identity, _ := d.known.pinnedIdentityAt(p.URL)
		b.merge(peerEntry{
			Identity:   identity,
			Label:      strings.TrimSpace(p.Name),
			Note:       p.Note,
			Addrs:      []string{p.URL},
			cfgAddr:    p.URL,
			Configured: true,
		})
	}
	// 2. What discovery has seen just now. Merged BY IDENTITY, so a machine that is both configured
	// and announcing itself is one row rather than two — the whole point of keying the directory on
	// identity rather than on a name (design §一.5).
	for _, seen := range d.sightings() {
		b.merge(peerEntry{
			Identity:   seen.Identity,
			Addrs:      append([]string(nil), seen.Addrs...),
			Discovered: true,
			LastSeen:   seen.Seen,
		})
	}
	entries := b.entries
	// 3. The card each entry has already answered with, from the cache: the peer's own name for
	// itself, and the addresses IT says it can be reached at — which go in front of everything else.
	for i := range entries {
		d.attachCard(&entries[i])
	}
	return capDiscovered(entries)
}

// dirBuilder accumulates entries keyed by identity, which is the one key that survives a rename, an
// address change and two sources describing the same machine.
type dirBuilder struct {
	entries []peerEntry
	at      map[string]int
}

func (b *dirBuilder) merge(e peerEntry) {
	if b.at == nil {
		b.at = make(map[string]int, 4)
	}
	if e.Identity != "" {
		if i, ok := b.at[e.Identity]; ok {
			b.entries[i] = combineEntries(b.entries[i], e)
			return
		}
		b.at[e.Identity] = len(b.entries)
	}
	b.entries = append(b.entries, e)
}

// combineEntries folds one description of a machine into the entry already held for it.
//
// The rules are the design's (§一.5, §三): the LABEL comes from the configuration, because a config
// line is what a person wrote and a discovered machine has no label at all; an address once learned
// is never dropped, only reordered; and an entry that is configured stays configured — that is what
// keeps it from expiring.
func combineEntries(have, add peerEntry) peerEntry {
	out := have
	if out.Label == "" {
		out.Label = add.Label
	}
	if out.Note == "" {
		out.Note = add.Note
	}
	if out.SelfName == "" {
		out.SelfName = add.SelfName
	}
	if out.cfgAddr == "" {
		out.cfgAddr = add.cfgAddr
	}
	out.Configured = out.Configured || add.Configured
	out.Discovered = out.Discovered || add.Discovered
	if add.LastSeen.After(out.LastSeen) {
		out.LastSeen = add.LastSeen
	}
	// Fresh addresses go in FRONT: whatever said "I am here" most recently is the best guess, and
	// the configured address is the entry of first contact — which is why it is added last, and
	// never removed.
	out.Addrs = prependAddrs(out.Addrs, add.Addrs)
	if out.Card == nil && add.Card != nil {
		out.Card, out.Err = add.Card, add.Err
	}
	return out
}

// attachCard fills in what this machine already knows about the peer's own account of itself, from
// the card cache. It never touches the network.
func (d *peerDirectory) attachCard(e *peerEntry) {
	if d == nil || len(e.Addrs) == 0 {
		return
	}
	for _, addr := range e.Addrs {
		cached, ok := d.cachedCard(addr)
		if !ok {
			continue
		}
		e.Card, e.Err = cached.card, cached.err
		if cached.card != nil {
			e.SelfName = cardName(cached.card)
			// The addresses the peer names for ITSELF are the freshest thing this machine knows
			// about where it lives, so they are tried first (see dialOrder).
			e.Addrs = prependAddrs(e.Addrs, cardAddrs(cached.card))
		}
		return
	}
}

// dialAddrs is the addresses this machine may DIAL, in order: https only, and why is the point of
// the whole directory.
//
// A peer is named by the identity it proves, and the proof is a certificate — so an address that
// cannot present one (http) is an address where nothing can be verified. Dressing up a plaintext
// connection as a delegation here would be worse than failing: the request would carry the prompt
// and the answer in the clear AND arrive at a machine this one cannot say it talked to. The
// configured addresses are already https (checkPeerURL refuses the rest); this is about the ones a
// PEER advertises about itself, which it can say anything at all about — a card that names an http
// address is a card naming a place where nothing is proven, and it is skipped rather than dialled.
func (e peerEntry) dialAddrs() []string {
	out := make([]string, 0, len(e.Addrs))
	for _, addr := range e.Addrs {
		if u, err := url.Parse(addr); err == nil && u.Scheme == "https" && u.Host != "" {
			out = append(out, addr)
		}
	}
	return out
}

// prependAddrs puts `fresh` in front of `have` in their given order, dropping anything already
// present.
//
// This IS the address order rule (design §三), and it lives in one place so it cannot drift:
//
//  1. the addresses the peer advertised most recently — the ones it says about itself;
//  2. the configured address, kept last because it is the entry of first contact and is never
//     deleted.
//
// Trying the self-advertised one first is what lets a machine that moved be found again without
// anyone editing a file. The price is real and worth stating: a laptop away from home tries its
// peer's LAN address first and waits for a connection that cannot work — which is why, when there is
// more than one address to try, every attempt but the last gets a SHORT deadline
// (remoteClient.dialAddrs) instead of the full request budget.
func prependAddrs(have, fresh []string) []string {
	if len(fresh) == 0 {
		return have
	}
	out := make([]string, 0, len(have)+len(fresh))
	seen := make(map[string]bool, len(have)+len(fresh))
	for _, group := range [][]string{fresh, have} {
		for _, a := range group {
			if a == "" || seen[a] {
				continue
			}
			seen[a] = true
			out = append(out, a)
		}
	}
	return out
}

// capDiscovered bounds how many machines the discovered half of the directory will hold at all.
//
// Expiry is NOT here, and that is a decision about where the rule lives: what "last seen" means is
// something only the browser knows (it is the component that observes and the one that re-observes),
// so it is the browser that forgets — a sighting it stops reporting simply stops being an entry
// (node/discovery.go discoveryTTL). A second clock here would be a second answer to "how long is a
// machine that went quiet kept", and the two would drift.
//
// The cap is different in kind: it is a limit on a LIST, which is this type's business, and it exists
// because anyone on a network can multicast — the number of machines discovery can add is not
// something this machine chooses.
//
// Configured entries are never dropped and never counted: a person put them there, and "it did not
// answer" is not a reason to hide a machine somebody asked this one to know about.
func capDiscovered(entries []peerEntry) []peerEntry {
	kept := entries[:0]
	discovered := make([]int, 0, len(entries))
	for _, e := range entries {
		if e.Discovered && !e.Configured {
			discovered = append(discovered, len(kept))
		}
		kept = append(kept, e)
	}
	if len(discovered) > maxDiscoveredPeers {
		// Oldest first: the ones that announced longest ago are the ones to lose, and the order is
		// decided on LastSeen rather than on arrival so that a machine that keeps announcing keeps
		// its place.
		sort.SliceStable(discovered, func(i, j int) bool {
			return kept[discovered[i]].LastSeen.Before(kept[discovered[j]].LastSeen)
		})
		drop := make(map[int]bool, len(discovered)-maxDiscoveredPeers)
		for _, i := range discovered[:len(discovered)-maxDiscoveredPeers] {
			drop[i] = true
		}
		out := make([]peerEntry, 0, len(kept))
		for i, e := range kept {
			if !drop[i] {
				out = append(out, e)
			}
		}
		kept = out
	}
	return kept
}

// sightings is what discovery has seen, filtered to what may be considered at all.
func (d *peerDirectory) sightings() []discoveredPeer {
	if d == nil || d.discover == nil {
		return nil
	}
	found := d.discover()
	out := make([]discoveredPeer, 0, len(found))
	for _, seen := range found {
		// A sighting with no identity cannot be keyed, merged or checked, so it is not an entry
		// this machine can hold. Dropping it here is not a policy about the network: the browser is
		// what accepted the announcement, and an announcement without a usable identity is one the
		// browser already refused to act on (node/discovery.go).
		if !IsNodeID(seen.Identity) {
			continue
		}
		out = append(out, seen)
	}
	return out
}

// cardName is a peer's SELF-DECLARED name, made safe to show: control characters out, length
// bounded. Everything about it is untrusted (see peerEntry.SelfName), and this is where that is
// enforced rather than at each of the places that print it.
func cardName(card *a2a.AgentCard) string {
	if card == nil {
		return ""
	}
	return strutil.OneLine(card.Name, selfNameMaxRunes)
}

// cardAddrs are the addresses a card advertises. They are the peer's own account of where it can be
// reached — sometimes not where this machine can reach it — which is exactly why they are candidates
// and not an answer: each one still has to prove the identity before it is used for anything.
func cardAddrs(card *a2a.AgentCard) []string {
	if card == nil {
		return nil
	}
	out := make([]string, 0, len(card.SupportedInterfaces))
	for _, iface := range card.SupportedInterfaces {
		if iface == nil {
			continue
		}
		if url := strings.TrimSpace(iface.URL); url != "" {
			out = append(out, url)
		}
	}
	return out
}
