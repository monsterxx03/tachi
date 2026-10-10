package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// Which key each peer presented the FIRST time this machine reached it (TOFU — trust on first
// use), persisted so the promise survives a restart.
//
// Why a persistent record rather than a per-process cache: TOFU's whole value is that the SECOND
// conversation is checked. A record that lived in memory would hand an impostor a fresh
// first-contact window on every launch — the one moment the mechanism exists to close.
//
// What TOFU does NOT do is prove the first contact was genuine, and nothing here may pretend
// otherwise (design §4.2): the first pin is recorded and REPORTED as unverified, and an operator
// who wants a stronger promise fills in `peers[].identity` instead of relying on this file.
type knownPeers struct {
	path string
	log  *logger.Logger
	// urls is where each alias is configured to be reached — used for the pin's record of where
	// it was made, never for the check itself (the check is the key).
	urls map[string]string

	mu    sync.Mutex
	items map[string]knownPeer
}

// knownPeer is one peer's pinned identity.
type knownPeer struct {
	NodeID string `json:"node_id"`
	// URL is where this machine reached it when the pin was made, kept because it is the fact an
	// operator needs when a peer is reconfigured: "we pinned THIS identity while talking to THAT
	// address" is what makes an unexplained mismatch diagnosable.
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

// confirm checks a peer's presented identity against what was pinned, and pins it on first contact.
func (p *knownPeers) confirm(alias, nodeID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if known, ok := p.items[alias]; ok {
		if known.NodeID == nodeID {
			return nil
		}
		return fmt.Errorf("%w: %s was pinned as node %s (%s, first seen %s) but presented %s. "+
			"If that peer really was reinstalled, remove its entry from %s and pair again",
			ErrIdentityMismatch, alias, known.NodeID, known.URL,
			known.FirstSeen.Format(time.RFC3339), nodeID, p.path)
	}

	// First contact. The identity is remembered and SAID OUT LOUD, because this is the one moment
	// TOFU cannot protect: if the operator did not expect a new pin here, that is the signal — and
	// a silent record would have spent it.
	p.items[alias] = knownPeer{NodeID: nodeID, FirstSeen: time.Now().UTC(), URL: p.urls[alias]}
	if err := p.save(); err != nil {
		// The pin is what the NEXT conversation is checked against, so a pin that did not reach
		// disk must not count as verified.
		delete(p.items, alias)
		return fmt.Errorf("node: record the identity of %s: %w", alias, err)
	}
	p.log.Warn(context.Background(), "node: pinned a peer identity on first contact "+
		"(UNVERIFIED — trust on first use only remembers the first key it saw)",
		"peer", alias, "node_id", nodeID)
	return nil
}

// nodeID returns the identity recorded for a peer, which is by construction the VERIFIED one: it
// is written by the TLS handshake (node/tls.go) and by nothing else, so a peer this machine has
// only ever reached in plaintext has no entry here — and a record therefore must not invent one.
func (p *knownPeers) nodeID(alias string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	known, ok := p.items[alias]
	return known.NodeID, ok && known.NodeID != ""
}

// save writes the record atomically, aliases sorted so the file reads the same twice.
func (p *knownPeers) save() error {
	ordered := make(map[string]knownPeer, len(p.items))
	names := make([]string, 0, len(p.items))
	for name := range p.items {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		ordered[name] = p.items[name]
	}
	return fileutil.AtomicWriteJSON(p.path, ordered, 0o700, 0o600)
}
