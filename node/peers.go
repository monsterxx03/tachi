package node

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/monsterxx03/tachi/agent/agentref"
	"github.com/monsterxx03/tachi/config"
)

// peerDirectory is this machine's view of the other Tachi nodes it knows about: where they came
// from, what it calls them, and which identity each one proved.
//
// It is ONE shape over three sources, and the merge itself lives in peerdir.go. What is here is the
// part that costs something: resolving a peer for a tool call, and asking a peer for its Agent Card.
//
// The card is resolved lazily and cached: resolving on every turn would put an HTTP request in front
// of every model call, and a card changes only when the peer is reconfigured. A failed resolution is
// cached for the same reason it is reported — so a peer that is down does not cost a timeout per
// turn.
type peerDirectory struct {
	peers []config.PeerConfig
	// known is the pin record, and it is here for two things: telling a config line which identity it
	// has already proved (so a row can show it before the two have spoken in this process), and being
	// the set of identities a tool argument may name. nil means no record to read.
	known *knownPeers
	// discover is the live view of what mDNS has seen, or nil when discovery is off. It is called on
	// the CHEAP path (build), so it must answer from memory and never wait for the network.
	discover func() []discoveredPeer

	mu    sync.Mutex
	cards map[string]*peerCard

	// resolveEntry reads one peer's Agent Card over the addresses the entry lists, and answers with
	// the address that did. Injected so a test can answer without a live peer; nil means no card can
	// be read, which is what a directory with no sending side behind it looks like.
	resolveEntry func(ctx context.Context, e peerEntry) (*a2a.AgentCard, string, error)

	// cardTTL bounds how long a resolved card is trusted, and how long a FAILURE is remembered.
	// A peer that was down is retried after this; a card that was read is re-read after it.
	cardTTL time.Duration
}

// peerCard is what we know about one peer's address: the card, or the failure, and when.
type peerCard struct {
	card *a2a.AgentCard
	err  error
	at   time.Time
}

// UnknownPeerError is the refusal a tool argument gets for something this machine cannot name. It
// lists what it CAN name: the model asked for a machine, and the useful answer is the set it could
// have asked for — by identity, in the one spelling a tool argument takes.
type UnknownPeerError struct {
	Ref   string
	Known []string
}

func (e *UnknownPeerError) Error() string {
	if len(e.Known) == 0 {
		return fmt.Sprintf("node: %q is not a node this machine knows, and it knows none at all", e.Ref)
	}
	return fmt.Sprintf("node: %q is not a node this machine knows; it can name: %s",
		e.Ref, strings.Join(e.Known, ", "))
}

const defaultPeerCardTTL = 5 * time.Minute

// checkPeerURL refuses a peer address this machine must not delegate over, and the rule is https for
// EVERY host — including loopback, which used to be exempt.
//
// The exemption is gone because of what naming a peer by identity means: an identity comes out of a
// certificate, and a peer reached in plaintext presents none, so this machine can pin nothing for it
// — and a peer it can pin nothing for is a peer no tool argument can name. An http line was therefore
// a configuration entry that could never be used, which is a trap rather than a choice: the reader
// would see a row, no identity beside it, and no way to make the delegation the line looks like it
// offers. Refusing the line says so at the moment it can still be fixed.
//
// https is accepted for any HOST, including loopback: the certificate is self-signed and the pin,
// not a hostname, is what says which machine answered (node/tls.go). So a peer on this very machine
// can be delegated to — it just has to serve TLS, which it does as soon as its listen address leaves
// loopback (node/options.go isLoopbackListen).
func checkPeerURL(who, raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fmt.Errorf("node: %s has no url: a delegation needs an address to reach", who)
	}
	u, err := url.Parse(trimmed)
	if err != nil || u.Host == "" {
		return fmt.Errorf("node: %s has no usable url: %q", who, raw)
	}
	if u.Scheme == "https" {
		return nil
	}
	return fmt.Errorf("node: %s is %q: a peer must be https. A delegation carries the prompt and its "+
		"answer, so plaintext is the same conversation read aloud on the way — and a peer reached in "+
		"plaintext proves no identity, which is the only thing a delegation can name it by", who, raw)
}

// newPeerDirectory validates a peer list and returns the directory over it.
//
// Validation is here rather than at config load because these are the SENDER's constraints, and the
// machine that has to refuse them is the one about to delegate. A duplicated LABEL is refused, and
// that is a display rule rather than a resolution one: a tool argument names a peer by identity, so
// two machines sharing a label would be resolvable and still unreadable — two rows called "nas" in
// the picker, the board and the reminder is the entire point of having a label, defeated.
func newPeerDirectory(peers []config.PeerConfig) (*peerDirectory, error) {
	seen := make(map[string]bool, len(peers))
	for i, p := range peers {
		label := strings.TrimSpace(p.Name)
		if label != "" {
			key := strings.ToLower(label)
			if seen[key] {
				return nil, fmt.Errorf("node: peer name %q is configured twice; a label must name one "+
					"machine, or it reads as the same one twice", label)
			}
			seen[key] = true
		}
		if err := checkPeerURL(configPeerWords(i, label), p.URL); err != nil {
			return nil, err
		}
	}
	return &peerDirectory{
		peers:   append([]config.PeerConfig(nil), peers...),
		cards:   make(map[string]*peerCard),
		cardTTL: defaultPeerCardTTL,
	}, nil
}

// configPeerWords names a config line in a sentence about it: its label when it has one, and its
// position when it does not (the label is optional, so "the peer called ”" has to not happen).
func configPeerWords(index int, label string) string {
	if label != "" {
		return fmt.Sprintf("peers[%d] (%q)", index, label)
	}
	return fmt.Sprintf("peers[%d]", index)
}

// Entries is the whole merged directory, cheapest possible: config lines, whatever has been pinned
// for them, and what discovery has just seen. No network, ever.
func (d *peerDirectory) Entries() []peerEntry {
	if d == nil {
		return nil
	}
	return d.build()
}

// Lookup resolves a TOOL ARGUMENT to a peer: a node id, or an unambiguous prefix of one.
//
// Only identities are resolved. A name is not a second way in, and the reason is that a name is not
// a fact: it is what its owner says it is (and every machine's default is the same word), so a
// delegation that went wherever a name pointed would be a delegation to whoever claimed it most
// recently. The identity comes out of a TLS handshake and cannot be claimed.
//
// A ref that IS a configured label gets its own answer, because that is the mistake a reader will
// actually make — the reminder shows labels — and "no such node" would send them looking for a typo.
//
// An unreachable peer is NOT refused here: whether the machine is up is decided by the request
// itself, and a card that could not be read is not evidence that work cannot be accepted (§8.6).
func (d *peerDirectory) Lookup(ctx context.Context, ref string) (peerEntry, error) {
	if d == nil {
		return peerEntry{}, &UnknownPeerError{Ref: ref}
	}
	ref = strings.TrimSpace(ref)
	entries := d.build()

	if entries, err := d.resolveRef(entries, ref); err != nil || entries.Identity != "" {
		return entries, err
	}

	// Not in the directory yet — and it may be a machine this one has never MET, whose identity is
	// therefore not in the directory yet either. The one place that can change is a connection, and
	// the door it opens is the whole reason this is worth doing: the model can only name a machine by
	// an identity it read somewhere, and an entry point with no reminder (the pipe) is never told
	// one by anything else. So the candidates are asked ONCE (the card read that pins them, cached
	// like any card), and the resolution is retried against what came back.
	if d.probeStrangers(ctx) {
		if e, err := d.resolveRef(d.build(), ref); err != nil || e.Identity != "" {
			return e, err
		}
	}

	if d.known != nil && d.known.has(ref) {
		// A pinned identity with no entry: this machine HAS talked to that machine and is not being
		// offered it now — its config line is gone and nothing announces it. That is a different
		// answer from "never heard of it", and the fix is different too.
		return peerEntry{}, fmt.Errorf("node: %s is pinned as a machine this one has reached, but it is "+
			"neither configured nor announcing itself any more; configure that node again to reach it", ref)
	}
	if e, ok := d.byLabel(entries, ref); ok {
		what := "it has not been reached over TLS in this process, so no identity is pinned for it yet — " +
			"a node is named by its id, and it has one once a handshake has proved it"
		if e.Identity == "" {
			return peerEntry{}, fmt.Errorf("node: %q is the label of %s, and %s", ref, e.Display(), what)
		}
		return peerEntry{}, fmt.Errorf("node: %q is a label, not an identity; %s is %s", ref, e.Display(), what)
	}
	return peerEntry{}, &UnknownPeerError{Ref: ref, Known: describeEntries(entries)}
}

// resolveRef is the identity half of Lookup, over a directory it is given: the entry the ref names, or
// an error, or an empty entry meaning "nothing here carries that identity" (which the caller may be
// able to change by connecting).
func (d *peerDirectory) resolveRef(entries []peerEntry, ref string) (peerEntry, error) {
	ids := make([]string, 0, len(entries))
	byID := make(map[string]int, len(entries))
	for i, e := range entries {
		if e.Identity == "" {
			continue
		}
		ids = append(ids, e.Identity)
		byID[e.Identity] = i
	}
	resolved, ok, err := agentref.ResolveID(ref, ids)
	if err != nil {
		// Ambiguous: named more than one machine, which must be refused rather than guessed at —
		// a guess sends the work to a different machine.
		return peerEntry{}, fmt.Errorf("node: %w; name the node by more of its id", err)
	}
	if !ok {
		return peerEntry{}, nil
	}
	return entries[byID[resolved]], nil
}

// probeStrangers reads the card of every entry this machine holds no identity for, and reports
// whether it had anything to do.
//
// It is the expensive half of Lookup and it runs only on the MISS path: a ref that resolved, and a
// ref that turns out to be a label, are both answered without a packet. What it buys is the case the
// design calls out — a configured peer is nameable once it has been reached, and the reaching has to
// happen SOMEWHERE; in an interactive turn the reminder's own probe does it, and in the pipe entry
// there is no reminder, so the delegation itself is what connects (design §二: 启动后/刷新时去读一次
// 卡片). The read is the ordinary card read, so its result and its failure are cached for the card
// TTL: a wrong identity costs one round of requests, not one per attempt.
func (d *peerDirectory) probeStrangers(ctx context.Context) bool {
	if d == nil || d.resolveEntry == nil {
		return false
	}
	asked := false
	for _, e := range d.build() {
		if e.Identity != "" || len(e.Addrs) == 0 {
			continue
		}
		asked = true
		if _, err := d.card(ctx, e); err != nil {
			continue
		}
	}
	return asked
}

// byLabel is the one lookup names still answer, and it exists only so a refusal can be specific. It
// is not a way to DELEGATE: nothing resolves a peer for a request this way.
func (d *peerDirectory) byLabel(entries []peerEntry, ref string) (peerEntry, bool) {
	for _, e := range entries {
		if e.Label != "" && strings.EqualFold(e.Label, ref) {
			return e, true
		}
	}
	return peerEntry{}, false
}

// byIdentity resolves a node id EXACTLY to its entry, which is how a RECORD reaches its peer: a
// record holds the identity it was bound to, so this lookup costs nothing and cannot be redirected
// by a rename, a re-pointed config line, or a name someone else claimed.
//
// A pinned identity with no entry is a real answer: it means the machine is no longer in the
// directory — its config line was removed and it is not announcing itself — and the caller says so
// rather than inventing a fallback. The pin itself is untouched (node/knownpeers.go).
func (d *peerDirectory) byIdentity(nodeID string) (peerEntry, bool) {
	if d == nil || nodeID == "" {
		return peerEntry{}, false
	}
	for _, e := range d.build() {
		if e.Identity == nodeID {
			return e, true
		}
	}
	return peerEntry{}, false
}

// Snapshot describes every peer, resolving the cards it has not read recently. It never fails: each
// entry carries its own resolution state, because an unreachable peer is one of the answers (§8.6 —
// "not listed" would read as "not configured").
//
// The peers are asked IN PARALLEL, and that is not a micro-optimization: the failures are what cost
// time. A peer that is down is one that does not answer, so a sequential walk pays every peer's
// timeout one after another — a fleet of three unreachable nodes would hold the picker (and the turn
// that triggered the reminder) for as long as their timeouts add up to, for an answer that was "none
// of them" from the first one. Each goroutine writes its own slot, so the result keeps the order the
// entries were merged in, which is the order a reminder reads in.
func (d *peerDirectory) Snapshot(ctx context.Context) []peerEntry {
	if d == nil {
		return nil
	}
	entries := d.build()
	out := make([]peerEntry, len(entries))
	var wg sync.WaitGroup
	for i, e := range entries {
		wg.Go(func() {
			card, err := d.card(ctx, e)
			e.Card, e.Err = card, err
			// The contact is where the identity comes from — the handshake that just ran, whether or
			// not the card itself could be read — so the entry picks it up HERE rather than on the
			// next rebuild. That is what makes a configured node nameable in the same turn that
			// discovered it, instead of one turn later (design §八 验收: 配置来的对端在这轮探测成功
			// 之后可被点名).
			if e.Identity == "" {
				if id, ok := d.known.pinnedIdentityAt(e.configAddr()); ok {
					e.Identity = id
				}
			}
			if card != nil {
				// The peer's own name for itself, and the addresses IT says it can be reached at —
				// which are tried in front of everything else (node/peerdir.go dialAddrs).
				e.SelfName = cardName(card)
				e.Addrs = prependAddrs(e.Addrs, cardAddrs(card))
			}
			out[i] = e
		})
	}
	wg.Wait()
	return out
}

// forgetCards drops every cached card, so the next Snapshot really asks the network.
//
// It exists for the board's refresh button: a card cache is what makes the reminder and the picker
// cheap, and a person who pressed 刷新 has asked for the cards as they are NOW — answering that with
// a cached read would be answering a question they did not ask. Failures are dropped with the
// successes, so a node that was down a second ago gets a fresh chance.
func (d *peerDirectory) forgetCards() {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.cards = make(map[string]*peerCard)
	d.mu.Unlock()
}

// cachedCard reads one address's cached answer, fresh or not — the caller decides what "fresh"
// means, because the cheap path (build) is happy with whatever is there and the expensive one
// (Snapshot) is not.
func (d *peerDirectory) cachedCard(addr string) (*peerCard, bool) {
	if d == nil || addr == "" {
		return nil, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	cached, ok := d.cards[addr]
	return cached, ok
}

// card returns the peer's Agent Card, from the cache when it is fresh.
//
// The cache is keyed by ADDRESS, not by identity: a card is what answered at an address, and before
// the first contact there is no identity to key by at all. Keying by identity would also mean the
// read this very call performs — which is where the identity comes from — could not find its own
// result on the next turn.
func (d *peerDirectory) card(ctx context.Context, e peerEntry) (*a2a.AgentCard, error) {
	for _, addr := range e.Addrs {
		if cached, ok := d.cachedCard(addr); ok && time.Since(cached.at) < d.cardTTL {
			return cached.card, cached.err
		}
	}
	addrs := e.dialAddrs()
	if d.resolveEntry == nil || len(addrs) == 0 {
		return nil, errors.New("this machine has no address for that peer it could ask over TLS")
	}

	card, addr, err := d.resolveEntry(ctx, e)
	key := addr
	if key == "" {
		key = addrs[0]
	}
	if key != "" {
		d.mu.Lock()
		d.cards[key] = &peerCard{card: card, err: err, at: time.Now()}
		d.mu.Unlock()
	}
	if err != nil {
		return nil, fmt.Errorf("node: %s did not answer with a card: %w", e.Display(), err)
	}
	return card, nil
}

// describeEntries lists the peers a refusal should read: how each is SHOWN (a label or a name, and
// always an identity). "Which nodes could I have named" is only useful if the answer is something
// the caller can act on, and that is the identity.
func describeEntries(entries []peerEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Display())
	}
	sort.Strings(out)
	return out
}
