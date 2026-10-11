package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// Which identity each peer proved the FIRST time this machine reached it (TOFU — trust on first
// use), persisted so the promise survives a restart.
//
// Why a persistent record rather than a per-process cache: TOFU's whole value is that the SECOND
// conversation is checked. A record that lived in memory would hand an impostor a fresh
// first-contact window on every launch — the one moment the mechanism exists to close.
//
// What TOFU does NOT do is prove the first contact was genuine, and nothing here may pretend
// otherwise (design §4.2): the first pin is recorded and REPORTED as unverified. The way to close
// that window is out of band — read the peer's node id where it is published, and compare it with
// what this file holds after the first call; there is no configured identity to fill in instead
// (an earlier slice had one, and it was removed along with the caller whitelist it served:
// callers are identified, not authorised — node/options.go checkRemoteListen).
//
// THE TABLE IS KEYED BY IDENTITY, and the alias is a field. That is what lets a rename be a
// rename: the key never moves, so the record follows the machine rather than the word this config
// happens to use for it (design §4.1), and a node discovered rather than configured — mDNS or
// gossip, where there is no config line and therefore no alias to key by — has somewhere to live.
// The other direction (an alias, or a prefix of an id, arriving as a tool argument) is a LOOKUP
// over this table, not a second source of truth (node/peers.go Lookup).
type knownPeers struct {
	path string
	log  *logger.Logger
	// urls is where each alias is configured to be reached — used for the pin's record of where
	// it was made, never for the check itself (the check is the key).
	urls map[string]string

	mu    sync.Mutex
	items map[string]knownPeer
}

// knownPeer is one peer's record, stored under the identity it proved.
type knownPeer struct {
	// Alias is the name THIS machine used for it when the record was last written. A LABEL, and
	// refreshed on every successful handshake, so a rename follows without anyone editing a file:
	// the record is written by the handshake, and the handshake happens under the current name.
	Alias string `json:"alias"`
	// URL is where this machine reached it when the record was last written, kept because it is the
	// fact an operator needs when a peer is reconfigured: "we pinned THIS identity while talking to
	// THAT address" is what makes an unexplained mismatch diagnosable.
	URL       string    `json:"url,omitempty"`
	FirstSeen time.Time `json:"first_seen"`
}

// openKnownPeers loads the record; nothing is written until the first pin.
func openKnownPeers(path string, urls map[string]string, log *logger.Logger) (*knownPeers, error) {
	if log == nil {
		log = logger.New("node")
	}
	p := &knownPeers{path: path, log: log, urls: urls, items: make(map[string]knownPeer)}
	err := fileutil.ReadJSON(path, &p.items)
	switch {
	case err == nil:
		// Entries with no name are not a shape this program writes any more: it is the file as it
		// was keyed before (by alias), or a damaged one. Either way it must NOT be read as "a record
		// whose name we do not know" — the aliases are the KEYS there, so every check this file
		// exists for would silently miss, and the next contact would be pinned afresh. That is a new
		// first-contact window, which is the one thing the record is for. So: refuse, and say what
		// to do about it.
		if stale := len(p.items) - p.namedCount(); stale > 0 {
			return nil, fmt.Errorf("node: %s holds %d entries with no name — it was written before this "+
				"file was keyed by node id (or it is damaged). Delete it and reach that peer once more: "+
				"the identity is then pinned again and reported as UNVERIFIED, because a first contact "+
				"is a first contact", path, stale)
		}
		return p, nil
	case errors.Is(err, os.ErrNotExist):
		// No record yet: every peer is a first contact. Not an error — it is the state every
		// deployment starts in.
		return p, nil
	default:
		// A record that exists but cannot be read must NOT be treated as empty: that would
		// silently re-open the first-contact window this file exists to close. Refusing is the
		// only answer that keeps the promise.
		return nil, fmt.Errorf("node: read peer identities %s: %w", path, err)
	}
}

// namedCount counts the entries that carry a name, which is every entry of a record this program
// writes: an entry without one is the older shape (or damage), and openKnownPeers refuses it.
func (p *knownPeers) namedCount() int {
	n := 0
	for _, peer := range p.items {
		if peer.Alias != "" {
			n++
		}
	}
	return n
}

// PeerAliases returns the peer name this machine has PINNED for each node id it has reached.
//
// The direction a reader needs is the opposite of the config's: a session records WHO opened it (the
// caller's node id, out of its certificate — the one thing a peer cannot choose), and the question
// is "which of my machines is that". Only the pinned record answers it. The caller's own name never
// does: any key can claim any name, which is why the name this node CALLS a peer is the local
// alias and nothing from the other side (design §4.2).
//
// Only OUTBOUND contact writes that record, so an id this machine has never talked to has no answer
// here. That is a correct answer rather than a failure — the UI says so in its own words — and a
// receive-only machine is in that state for every caller it has.
//
// A record that cannot be read yields nothing: this is a label for a session row, and a sidebar that
// does not draw is worse than one that says "unknown".
func PeerAliases() map[string]string {
	pinned := readPinnedPeers()
	out := make(map[string]string, len(pinned))
	for nodeID, peer := range pinned {
		if peer.Alias != "" {
			out[nodeID] = peer.Alias
		}
	}
	return out
}

// readPinnedPeers reads the TOFU record, keyed by identity. It answers with an empty map when the
// file is absent or unreadable — the CALLERS are labels and pickers, where a blank answer beats a
// refusal. The node's own identity check reads the record through openKnownPeers instead, which is
// strict about exactly that case, because there a blank record would re-open the first-contact
// window.
//
// It is read live rather than cached for the same reason: an operator who fixed the record by hand
// sees the effect immediately, and the picker's per-keystroke read is a small file.
func readPinnedPeers() map[string]knownPeer {
	var pinned map[string]knownPeer
	if err := fileutil.ReadJSON(config.PeerIdentitiesPath(), &pinned); err != nil {
		return nil
	}
	for key, peer := range pinned {
		if peer.Alias == "" {
			// A table in the older shape (or a damaged one): its keys are aliases, so nothing here
			// is keyed by an identity. A label is not worth guessing at, and this path cannot
			// repair anything — openKnownPeers refuses the file outright and says what to do.
			delete(pinned, key)
		}
	}
	return pinned
}

// confirm checks a peer's presented identity against what was recorded, and records it on first
// contact.
//
// Two rules, and the second is the one the old shape used to get for free by keying on the alias:
//
//   - the IDENTITY is the key, so the same machine presenting the same key under a NEW alias is a
//     rename — accepted, and the record's alias follows it;
//   - the same ALIAS may not belong to two identities. That is the protection this table exists for:
//     a config line pointed at a different machine (or an impostor claiming a known name) has to be
//     refused, naming both ids, rather than silently accepted as "a new peer".
func (p *knownPeers) confirm(alias, nodeID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if owner, ok := p.byAliasLocked(alias); ok && owner != nodeID {
		known := p.items[owner]
		return fmt.Errorf("%w: %s was pinned as node %s (%s, first seen %s) but presented %s. "+
			"If that peer really was reinstalled, remove its entry from %s and pair again",
			ErrIdentityMismatch, alias, owner, known.URL,
			known.FirstSeen.Format(time.RFC3339), nodeID, p.path)
	}

	if known, ok := p.items[nodeID]; ok {
		// The same machine. Refresh the label and the address it was reached at; the first sighting
		// is the one fact that must never move.
		url := p.urls[alias]
		if known.Alias == alias && known.URL == url {
			return nil
		}
		known.Alias, known.URL = alias, url
		p.items[nodeID] = known
		return p.saveLocked()
	}

	// First contact. The identity is remembered and SAID OUT LOUD, because this is the one moment
	// TOFU cannot protect: if the operator did not expect a new pin here, that is the signal — and
	// a silent record would have spent it.
	p.items[nodeID] = knownPeer{Alias: alias, FirstSeen: time.Now().UTC(), URL: p.urls[alias]}
	if err := p.saveLocked(); err != nil {
		// The pin is what the NEXT conversation is checked against, so a pin that did not reach
		// disk must not count as verified.
		delete(p.items, nodeID)
		return fmt.Errorf("node: record the identity of %s: %w", alias, err)
	}
	p.log.Warn(context.Background(), "node: pinned a peer identity on first contact "+
		"(UNVERIFIED — trust on first use only remembers the first key it saw)",
		"peer", alias, "node_id", nodeID)
	return nil
}

// nodeID returns the identity recorded for an alias, which is by construction the VERIFIED one: it
// is written by the TLS handshake (node/tls.go) and by nothing else, so a peer this machine has only
// ever reached in plaintext has no entry here — and a record therefore must not invent one.
//
// The scan is over the configured peers (a handful); this is not a table to index until discovery
// makes it thousands of entries wide.
func (p *knownPeers) nodeID(alias string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.byAliasLocked(alias)
}

// aliasFor returns the name this machine last used for an identity, or "" when it holds no record.
func (p *knownPeers) aliasFor(nodeID string) (string, bool) {
	if p == nil || nodeID == "" {
		return "", false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	known, ok := p.items[nodeID]
	return known.Alias, ok && known.Alias != ""
}

// identities returns a copy of the record as node id → alias, for callers that resolve identities
// rather than names (the node picker's rows, a tool argument naming a machine).
func (p *knownPeers) identities() map[string]string {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]string, len(p.items))
	for nodeID, peer := range p.items {
		if peer.Alias != "" {
			out[nodeID] = peer.Alias
		}
	}
	return out
}

func (p *knownPeers) byAliasLocked(alias string) (string, bool) {
	for nodeID, peer := range p.items {
		if peer.Alias == alias {
			return nodeID, true
		}
	}
	return "", false
}

// saveLocked writes the record atomically, ids sorted so the file reads the same twice. It is the
// locked half: every caller already holds mu (confirm has to, because the write and the decision
// about whether to make it are one step).
func (p *knownPeers) saveLocked() error {
	ids := make([]string, 0, len(p.items))
	for nodeID := range p.items {
		ids = append(ids, nodeID)
	}
	sort.Strings(ids)
	ordered := make(map[string]knownPeer, len(p.items))
	for _, nodeID := range ids {
		ordered[nodeID] = p.items[nodeID]
	}
	return fileutil.AtomicWriteJSON(p.path, ordered, 0o700, 0o600)
}
