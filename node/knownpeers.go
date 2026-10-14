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
// THE TABLE IS KEYED BY IDENTITY. That is what lets a rename be a rename (the key never moves, so
// the record follows the machine rather than the word a config happens to use for it), and what
// gives a node discovered rather than configured — mDNS, where there is no config line and
// therefore no name to key by — somewhere to live.
//
// The label and the address are BOOKKEEPING, not keys: `alias` is the label this machine used when
// the record was last written, `url` the address it reaches that machine at. Neither is checked
// against anything — they are here so a person can read the file and see what this machine knows,
// and so a config line can find the identity pinned for it before the two have spoken in this
// process (node/peerdir.go joins on the address).
type knownPeers struct {
	path string
	log  *logger.Logger

	mu    sync.Mutex
	items map[string]knownPeer
}

// knownPeer is one peer's record, stored under the identity it proved.
type knownPeer struct {
	// Alias is the label this machine used for the peer when the record was last written. It is a
	// LABEL, refreshed on every successful handshake — so a rename follows without anyone editing a
	// file, and it may be EMPTY, because a config line's label is optional.
	Alias string `json:"alias,omitempty"`
	// URL is the address this machine reaches that machine at — the configured one when there is a
	// config line, otherwise the address this machine dialled. It is kept because it is the fact an
	// operator needs when a peer is reconfigured ("we pinned THIS identity while talking to THAT
	// address"), and because it is how a config line finds its identity before the first contact in
	// this process.
	URL       string    `json:"url,omitempty"`
	FirstSeen time.Time `json:"first_seen"`
}

// openKnownPeers loads the record; nothing is written until the first pin.
func openKnownPeers(path string, log *logger.Logger) (*knownPeers, error) {
	if log == nil {
		log = logger.New("node")
	}
	p := &knownPeers{path: path, log: log, items: make(map[string]knownPeer)}
	err := fileutil.ReadJSON(path, &p.items)
	switch {
	case err == nil:
		// Every key must LOOK like a node id. A file whose keys are names is the version before
		// this one, which keyed by alias (or it is damaged). Either way it must NOT be read as "a
		// record whose ids happen to be words": every identity check this file exists for would
		// silently miss, and the next contact would be pinned afresh — a new first-contact window,
		// the one thing the record is for. So: refuse, and say what to do about it.
		if bad := p.miskeyedCount(); bad > 0 {
			return nil, fmt.Errorf("node: %s holds %d entries keyed by something that is not a node "+
				"id — it was written before this file was keyed by identity (or it is damaged). Delete "+
				"it and reach those peers once more: each identity is then pinned again and reported as "+
				"UNVERIFIED, because a first contact is a first contact", path, bad)
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

// miskeyedCount counts entries whose key is not spelled like a node id, which is every entry of a
// record written before identity became the key.
func (p *knownPeers) miskeyedCount() int {
	bad := 0
	for key := range p.items {
		if !IsNodeID(key) {
			bad++
		}
	}
	return bad
}

// PeerAliases returns the peer label this machine has PINNED for each node id it has reached.
//
// The direction a reader needs is the opposite of the config's: a session records WHO opened it (the
// caller's node id, out of its certificate — the one thing a peer cannot choose), and the question
// is "which of my machines is that". Only the pinned record answers it. The caller's own name never
// does: any key can claim any name, which is why the name this node CALLS a peer is the local label
// and nothing from the other side (design §4.2).
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
	for key := range pinned {
		if !IsNodeID(key) {
			// A table in the older shape (or a damaged one): its keys are labels, so nothing here is
			// keyed by an identity. A label is not worth guessing at, and this path cannot repair
			// anything — openKnownPeers refuses the file outright and says what to do.
			delete(pinned, key)
		}
	}
	return pinned
}

// confirm checks a peer's presented identity against what was recorded, and records it on first
// contact.
//
// It is called from the TLS handshake (node/tls.go) and from nowhere else, which is what makes the
// pin unskippable: every way this machine talks to a peer over TLS goes through it.
//
// The single rule is the key: the IDENTITY is what is remembered, and the same machine presenting
// the same key under a new label — or at a new address — is the same machine. (The version before
// this one keyed by label, and so had to refuse "a label that belongs to two identities"; with the
// label demoted to a display string there is nothing left for that rule to protect, and a config
// line moved to a different machine is now simply a second machine.)
func (p *knownPeers) confirm(label, configuredAddr, nodeID string) error {
	if !IsNodeID(nodeID) {
		// The id comes from a hash of the presented key, so this can only be a bug this side. It is
		// refused rather than stored: a record keyed by something that is not an id is a record
		// nothing will ever match.
		return fmt.Errorf("node: refusing to record %q as an identity: it is not a node id", nodeID)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// One ADDRESS may not belong to two identities — the shape of the protection the old "one name,
	// one identity" rule gave, kept in the form that still means something now that the record is
	// keyed by identity. A key this machine has never seen answering at an address it HAS reached
	// before is exactly the case TOFU exists for: either the machine was reinstalled (the operator
	// deletes the entry, and the refusal says so) or something else is answering there.
	//
	// It is checked only for an address the record actually carries (a pin made at no address —
	// impossible today, but the field is optional in the file — has nothing to compare), and only
	// when the identity is NEW: the same key at the same address is the ordinary repeat.
	if configuredAddr != "" {
		if _, ok := p.items[nodeID]; !ok {
			if owner, held := p.byURLLocked(configuredAddr); held {
				known := p.items[owner]
				return fmt.Errorf("%w: %s was pinned as node %s (%s, first seen %s) and now presents %s. "+
					"If that peer really was reinstalled, remove its entry from %s and pair again",
					ErrIdentityMismatch, peerWords(known.Alias, configuredAddr), owner,
					configuredAddr, known.FirstSeen.Format(time.RFC3339), nodeID, p.path)
			}
		}
	}

	if known, ok := p.items[nodeID]; ok {
		// The same machine: refresh what it is called and where it was reached. The first sighting
		// is the one fact that must never move.
		if known.Alias != label || known.URL != configuredAddr {
			known.Alias, known.URL = label, configuredAddr
			p.items[nodeID] = known
			return p.saveLocked()
		}
		return nil
	}

	// First contact. The identity is remembered and SAID OUT LOUD, because this is the one moment
	// TOFU cannot protect: if the operator did not expect a new pin here, that is the signal — and a
	// silent record would have spent it.
	p.items[nodeID] = knownPeer{Alias: label, URL: configuredAddr, FirstSeen: time.Now().UTC()}
	if err := p.saveLocked(); err != nil {
		// The pin is what the NEXT conversation is checked against, so a pin that did not reach
		// disk must not count as verified.
		delete(p.items, nodeID)
		return fmt.Errorf("node: record the identity of %s: %w", peerWords(label, configuredAddr), err)
	}
	p.log.Warn(context.Background(), "node: pinned a peer identity on first contact "+
		"(UNVERIFIED — trust on first use only remembers the first key it saw)",
		"peer", peerWords(label, configuredAddr), "node_id", nodeID)
	return nil
}

// aliasFor returns the label this machine last used for an identity, or "" when it holds no record
// (or holds one whose label was empty, which a config line's optional label makes ordinary).
func (p *knownPeers) aliasFor(nodeID string) (string, bool) {
	if p == nil || nodeID == "" {
		return "", false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	known, ok := p.items[nodeID]
	return known.Alias, ok && known.Alias != ""
}

// pinnedIdentityAt is the identity this machine has pinned for a peer it reaches AT that address,
// which is how a config line meets the identity it will be showing before the two have spoken in
// this process.
//
// Addresses are not keys here and nothing is ever looked up BY one after this: a peer's addresses
// change, and more than one address can belong to one machine — this answers "have we been here
// before", and the answer is only used to label a row.
//
// More than one identity pinned at the same address means the address changed hands; the most
// recently first seen one wins, which is the machine actually there now.
func (p *knownPeers) pinnedIdentityAt(addr string) (string, bool) {
	if p == nil || addr == "" {
		return "", false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	best, found := "", false
	for nodeID, known := range p.items {
		if known.URL != addr {
			continue
		}
		if !found || known.FirstSeen.After(p.items[best].FirstSeen) {
			best, found = nodeID, true
		}
	}
	return best, found
}

// byURLLocked is the address → identity direction, used by confirm to refuse an address that has
// already been pinned to somebody else. Locked half: confirm holds mu.
func (p *knownPeers) byURLLocked(addr string) (string, bool) {
	for nodeID, known := range p.items {
		if known.URL == addr {
			return nodeID, true
		}
	}
	return "", false
}

// has reports whether this machine holds a pin for an identity — the record itself, not a reachable
// machine: a pin outlives the config line it was made under, which is why a refusal can say "this is
// a machine I have reached" while the directory has no entry for it.
func (p *knownPeers) has(nodeID string) bool {
	if p == nil || nodeID == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.items[nodeID]
	return ok
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

// peerWords names a peer in a sentence: the label a person gave it when there is one, and the
// address otherwise — because a config line's label is optional, and a message about "the peer
// called ”" helps nobody.
func peerWords(label, addr string) string {
	if label != "" {
		return label
	}
	if addr != "" {
		return addr
	}
	return "a peer"
}
