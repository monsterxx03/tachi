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

	"github.com/monsterxx03/tachi/config"
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

	mu    sync.Mutex
	cards map[string]*peerCard

	// resolve is the card lookup, injected so a test can answer without a live peer. Nil means
	// the SDK's own resolver, which is what a real node uses.
	resolve func(ctx context.Context, baseURL string) (*a2a.AgentCard, error)

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
		if _, err := url.Parse(strings.TrimSpace(p.URL)); err != nil || strings.TrimSpace(p.URL) == "" {
			return nil, fmt.Errorf("node: peer %q has no usable url: %q", name, p.URL)
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

// Lookup returns one peer by alias, resolving its card if that has not been done recently.
//
// An unreachable peer is NOT an error here: the caller gets the configured identity plus why the
// card is missing, because "we cannot describe this node right now" and "there is no such peer"
// lead to different actions (report vs. pick another name).
func (d *peerDirectory) Lookup(ctx context.Context, name string) (peer, error) {
	if d == nil {
		return peer{}, &UnknownPeerError{Name: name}
	}
	for _, p := range d.peers {
		if p.Name != name {
			continue
		}
		out := peer{Name: p.Name, URL: p.URL, Note: p.Note}
		card, err := d.card(ctx, p)
		out.Card, out.Err = card, err
		return out, nil
	}
	return peer{}, &UnknownPeerError{Name: name, Known: d.Names()}
}

// Snapshot describes every configured peer, for the reminder. It never fails: each entry
// carries its own resolution state.
func (d *peerDirectory) Snapshot(ctx context.Context) []peer {
	if d == nil {
		return nil
	}
	out := make([]peer, 0, len(d.peers))
	for _, p := range d.peers {
		card, err := d.card(ctx, p)
		out = append(out, peer{Name: p.Name, URL: p.URL, Note: p.Note, Card: card, Err: err})
	}
	return out
}

// card returns the peer's Agent Card, from the cache when it is fresh.
func (d *peerDirectory) card(ctx context.Context, p config.PeerConfig) (*a2a.AgentCard, error) {
	d.mu.Lock()
	cached, ok := d.cards[p.Name]
	d.mu.Unlock()
	if ok && time.Since(cached.at) < d.cardTTL {
		return cached.card, cached.err
	}

	card, err := d.resolveCard(ctx, p.URL)

	d.mu.Lock()
	d.cards[p.Name] = &peerCard{card: card, err: err, at: time.Now()}
	d.mu.Unlock()

	if err != nil {
		return nil, fmt.Errorf("node: peer %s (%s) did not answer with a card: %w", p.Name, p.URL, err)
	}
	return card, nil
}

func (d *peerDirectory) resolveCard(ctx context.Context, baseURL string) (*a2a.AgentCard, error) {
	if d.resolve != nil {
		return d.resolve(ctx, baseURL)
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
