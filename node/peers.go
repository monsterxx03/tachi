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
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"

	"github.com/monsterxx03/tachi/agent/agentref"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/netutil"
)

// The peer directory: which other Tachi nodes this machine may delegate to.
//
// Two sources make one shape. The CONFIGURED entry is the alias the model uses and the address
// to reach it; the node's own Agent Card is what says what that node actually does. Both are
// kept apart in the struct rather than merged, because they fail differently: an unknown alias
// is a model mistake to refuse, while a configured peer that cannot be reached is a fact about
// the network — the design's rule is that a reminder may not describe a node it could not ask,
// so "unreachable / not resolved yet" is a state this directory reports (§8.6).
//
// The card is resolved lazily and cached: resolving on every turn would put an HTTP request in
// front of every model call, and the card changes only when the peer is reconfigured. A failed
// resolution is cached for the same reason it is reported — so a peer that is down does not
// cost a timeout per turn.
type peerDirectory struct {
	peers []config.PeerConfig
	// known is the TOFU record, and it is here for ONE thing: resolving a machine by its IDENTITY
	// (a node id, or an unambiguous prefix of one), which is how a reader — and, once nodes are
	// discovered rather than configured, the sender itself — names a peer. nil means no record to
	// read, and then only names resolve. Set at construction (node/sender.go).
	known *knownPeers

	mu    sync.Mutex
	cards map[string]*peerCard

	// resolve is the card lookup, injected so a test can answer without a live peer. Nil means
	// the SDK's own resolver, which is what a real node uses.
	resolve func(ctx context.Context, alias, baseURL string) (*a2a.AgentCard, error)

	// cardTTL bounds how long a resolved card is trusted, and how long a FAILURE is remembered.
	// A peer that was down is retried after this; a card that was read is re-read after it.
	cardTTL time.Duration
}

// peerCard is what we know about one peer: the card, or the failure, and when.
type peerCard struct {
	card *a2a.AgentCard
	err  error
	at   time.Time
}

// peer is the directory's answer about one node — what the tools and the reminder both need.
type peer struct {
	Name string
	URL  string
	// Card is the peer's own description of itself, nil when it could not be read.
	Card *a2a.AgentCard
	// Note is the configured human line, used when there is no card to describe the node.
	Note string
	// Err is why the card is missing (nil when the card was read).
	Err error
}

// UnknownPeerError is the refusal a tool argument gets for an alias this machine does not know.
// It lists what IS configured: the model picked a name, and the useful answer is the set it
// could have picked from.
type UnknownPeerError struct {
	Name  string
	Known []string
}

func (e *UnknownPeerError) Error() string {
	if len(e.Known) == 0 {
		return fmt.Sprintf("node: %q is not a configured peer, and no peers are configured at all", e.Name)
	}
	return fmt.Sprintf("node: %q is not a configured peer; configured peers are: %s",
		e.Name, strings.Join(e.Known, ", "))
}

const defaultPeerCardTTL = 5 * time.Minute

// configOnly returns one configured peer WITHOUT touching the network.
//
// Separate from Lookup on purpose: Lookup resolves the Agent Card, which is a request, and the
// request path needs the connection facts (address, credential, pin) on every call while it must
// not pay for a card fetch it did not ask for. The card is a DESCRIPTION of the peer; this is how
// to REACH it.
func (d *peerDirectory) configOnly(name string) (peer, bool) {
	if d == nil {
		return peer{}, false
	}
	for _, p := range d.peers {
		if p.Name == name {
			return peer{Name: p.Name, URL: p.URL, Note: p.Note}, true
		}
	}
	return peer{}, false
}

// configOnlyNames lists the configured aliases, sorted (the same order Names() gives, from the
// config rather than from a network round trip).
func (d *peerDirectory) configOnlyNames() []string {
	if d == nil {
		return nil
	}
	out := make([]string, 0, len(d.peers))
	for _, p := range d.peers {
		out = append(out, p.Name)
	}
	sort.Strings(out)
	return out
}

// checkPeerURL refuses a peer address this machine must not delegate over.
//
// The rule is about what a delegation carries: the prompt and the answer ARE the conversation, so
// plaintext to another machine is not a smaller version of an encrypted one — it is the same
// conversation read aloud on the way. Loopback is exempt because there is no network to read it
// from (and because every deployment starts there), and https is accepted for any host because
// the pin, not the scheme, is what says which machine answered (node/tls.go).
func checkPeerURL(name, raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fmt.Errorf("node: peer %q has no url: a delegation needs an address to reach", name)
	}
	u, err := url.Parse(trimmed)
	if err != nil || u.Host == "" {
		return fmt.Errorf("node: peer %q has no usable url: %q", name, raw)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && netutil.IsLoopbackHost(u.Hostname()) {
		return nil
	}
	return fmt.Errorf("node: peer %q is %q: a delegation carries the prompt and its answer, so a peer "+
		"must be https — http is allowed only on loopback", name, raw)
}

// newPeerDirectory validates a peer list and returns the directory over it.
//
// Validation is here rather than at config load because these are the SENDER's constraints: a
// duplicate alias, an empty name or an address that is not a URL would each make a delegation
// ambiguous or impossible, and the machine that has to refuse them is the one about to delegate.
func newPeerDirectory(peers []config.PeerConfig) (*peerDirectory, error) {
	seen := make(map[string]bool, len(peers))
	for i, p := range peers {
		name := strings.TrimSpace(p.Name)
		if name == "" {
			return nil, fmt.Errorf("node: peers[%d] has no name; the model addresses a peer by name", i)
		}
		if seen[name] {
			return nil, fmt.Errorf("node: peer name %q is configured twice; a name must resolve to one node", name)
		}
		seen[name] = true
		if err := checkPeerURL(name, p.URL); err != nil {
			return nil, err
		}
	}
	return &peerDirectory{
		peers:   append([]config.PeerConfig(nil), peers...),
		cards:   make(map[string]*peerCard),
		cardTTL: defaultPeerCardTTL,
	}, nil
}

// Names lists the configured aliases, sorted so a refusal (and a reminder) reads the same way
// twice — Go's map iteration is random, and "which peers could I have used" must not shuffle.
func (d *peerDirectory) Names() []string {
	if d == nil {
		return nil
	}
	out := make([]string, 0, len(d.peers))
	for _, p := range d.peers {
		out = append(out, p.Name)
	}
	sort.Strings(out)
	return out
}

// Lookup returns one peer by NAME (its configured alias) or by IDENTITY (a node id, or an
// unambiguous prefix of one), resolving its card if that has not been done recently.
//
// Two ways in, because two different callers name a machine in two different ways: a model copies
// the alias the reminder listed, while a reader — and, once nodes are discovered rather than
// configured, the sender itself — knows a machine by the id it proved. The id path goes through the
// TOFU record (node/knownpeers.go), which is the only place that knows which name this machine last
// used for an identity.
//
// An unreachable peer is NOT an error here: the caller gets the configured identity plus why the
// card is missing, because "we cannot describe this node right now" and "there is no such peer"
// lead to different actions (report vs. pick another name).
func (d *peerDirectory) Lookup(ctx context.Context, name string) (peer, error) {
	if d == nil {
		return peer{}, &UnknownPeerError{Name: name}
	}
	name = strings.TrimSpace(name)

	// A configured alias first: that is what the reminder lists and what a tool argument carries.
	for _, p := range d.peers {
		if p.Name == name {
			return d.describe(ctx, p), nil
		}
	}

	// Then an identity. A record can name a machine this configuration no longer has an entry for
	// (it was renamed, and the rename has not been reached yet) — that is a different answer from
	// "never heard of it", and it says so, because the fix is different too.
	if alias, nodeID, err := d.byNodeID(name); err != nil {
		return peer{}, err
	} else if alias != "" {
		for _, p := range d.peers {
			if p.Name == alias {
				return d.describe(ctx, p), nil
			}
		}
		return peer{}, fmt.Errorf("node: %s is the identity last reached as %q, which is no longer a "+
			"configured peer; configure that node again to reach it", nodeID, alias)
	}

	return peer{}, &UnknownPeerError{Name: name, Known: d.describedNames()}
}

// describe reads one configured peer's card, for a Lookup that already found the entry.
func (d *peerDirectory) describe(ctx context.Context, p config.PeerConfig) peer {
	out := peer{Name: p.Name, URL: p.URL, Note: p.Note}
	card, err := d.card(ctx, p)
	out.Card, out.Err = card, err
	return out
}

// byNodeID resolves a node id (or a unique prefix of one) to the name this machine last used for
// it, via the TOFU record. An empty alias with no error means "no record of that identity".
func (d *peerDirectory) byNodeID(ref string) (alias, nodeID string, err error) {
	if d == nil || d.known == nil || ref == "" {
		return "", "", nil
	}
	identities := d.known.identities()
	ids := make([]string, 0, len(identities))
	for id := range identities {
		ids = append(ids, id)
	}
	resolved, ok, err := agentref.ResolveID(ref, ids)
	if err != nil || !ok {
		return "", "", err
	}
	return identities[resolved], resolved, nil
}

// describedNames lists the configured peers the way a refusal should read: the alias, and the
// identity beside it when this machine holds one. "Which peers could I have used" is only useful if
// the answer is something the caller can act on, and both of those are names they can.
func (d *peerDirectory) describedNames() []string {
	if d == nil {
		return nil
	}
	out := make([]string, 0, len(d.peers))
	for _, p := range d.peers {
		if d.known != nil {
			if id, ok := d.known.nodeID(p.Name); ok {
				out = append(out, fmt.Sprintf("%s (%s)", p.Name, agentref.ShortID(id)))
				continue
			}
		}
		out = append(out, p.Name)
	}
	sort.Strings(out)
	return out
}

// Snapshot describes every configured peer, for the reminder and the node picker. It never fails:
// each entry carries its own resolution state.
//
// The peers are asked IN PARALLEL, and that is not a micro-optimization: the failures are what cost
// time. A peer that is down is one that does not answer, so a sequential walk pays every peer's
// timeout one after another — a fleet of three unreachable nodes would hold the picker (and the
// turn that triggered the reminder) for as long as their timeouts add up to, for an answer that was
// "none of them" from the first one. Each goroutine writes its own slot, so the result keeps the
// configured order, which is the order a reminder reads in.
func (d *peerDirectory) Snapshot(ctx context.Context) []peer {
	if d == nil {
		return nil
	}
	out := make([]peer, len(d.peers))
	var wg sync.WaitGroup
	for i, p := range d.peers {
		wg.Go(func() {
			// d.card does its own locking (the card cache is shared), so concurrent callers are
			// what it is written for.
			card, err := d.card(ctx, p)
			out[i] = peer{Name: p.Name, URL: p.URL, Note: p.Note, Card: card, Err: err}
		})
	}
	wg.Wait()
	return out
}

// forgetCards drops every cached card, so the next Snapshot really asks the network.
//
// It exists for the board's refresh button: a card cache is what makes the reminder and the picker
// cheap, and a person who pressed 刷新 has asked for the cards as they are NOW — answering that
// with a cached read would be answering a question they did not ask. Failures are dropped with the
// successes, so a node that was down a second ago gets a fresh chance.
func (d *peerDirectory) forgetCards() {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.cards = make(map[string]*peerCard)
	d.mu.Unlock()
}

// card returns the peer's Agent Card, from the cache when it is fresh.
func (d *peerDirectory) card(ctx context.Context, p config.PeerConfig) (*a2a.AgentCard, error) {
	d.mu.Lock()
	cached, ok := d.cards[p.Name]
	d.mu.Unlock()
	if ok && time.Since(cached.at) < d.cardTTL {
		return cached.card, cached.err
	}

	card, err := d.resolveCard(ctx, p.Name, p.URL)

	d.mu.Lock()
	d.cards[p.Name] = &peerCard{card: card, err: err, at: time.Now()}
	d.mu.Unlock()

	if err != nil {
		return nil, fmt.Errorf("node: peer %s (%s) did not answer with a card: %w", p.Name, p.URL, err)
	}
	return card, nil
}

func (d *peerDirectory) resolveCard(ctx context.Context, alias, baseURL string) (*a2a.AgentCard, error) {
	if d.resolve != nil {
		return d.resolve(ctx, alias, baseURL)
	}
	card, err := agentcard.DefaultResolver.Resolve(ctx, baseURL)
	if err != nil {
		return nil, err
	}
	if card == nil {
		return nil, errors.New("the resolver returned no card")
	}
	return card, nil
}
