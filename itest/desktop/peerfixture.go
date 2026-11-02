// peerfixture is a test fixture: another Tachi node, in one process, serving the Agent Card the
// Agents panel asks a peer for.
//
// It exists because the panel's expanded card has two halves, and the smoke sandbox only ever had
// one of them: a peer that does NOT answer (the panel then says 「联系不上」/「它没有回答」, which is
// a real answer and worth pinning). What the card's OWN fields look like — 自称, 能力, 入口, and the
// skills list — was verified by nobody, which is exactly how a list that repeated its label on every
// entry stayed in the tree.
//
// The card is built as an a2a.AgentCard and served as that type's own JSON, so the fixture cannot
// drift from what the app parses: the client reads this with the same struct, and a hand-written
// JSON literal would be a second description of the protocol.
//
// The fixture speaks **https** on loopback with its OWN ed25519 self-signed certificate (see
// selfSignedEd25519): `node/peers.go`
// refuses any peer that is not https, loopback included, because a plaintext peer proves no identity
// and an identity is the only thing a delegation can name a peer by. It said "on loopback" here once
// — that exemption is gone, and this fixture kept serving http long after it, so the whole
// directory was refused and the panel was empty (which is why `agents-panel` was red everywhere
// while its unit-test half, which sets cfg.Peers in code, stayed green).
//
// Original note (kept for the shape it describes): A peer's URL may be http only when the host is
// loopback (config.PeerConfig says why), and this fixture IS loopback — which also keeps the
// sandbox from needing a certificate. What that costs is the identity half: nothing is pinned over
// plaintext, so this peer reports no identity, and pinning stays where it belongs (the TLS path,
// pinned by node's own tests).
//
// Nothing here imports `testing`: the smoke runner runs it in its own process, as an ordinary HTTP
// server on a loopback port.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/monsterxx03/tachi/config"
)

// peerFixtureCardName and peerFixtureVersion are what the fixture node calls itself. They are
// asserted verbatim on screen, so they are spelled once.
const (
	peerFixtureCardName = "smoke-peer-node"
	peerFixtureVersion  = "9.9.9"
)

// peerFixture is one running fake node.
type peerFixture struct {
	// URL is the origin, e.g. "https://127.0.0.1:53421" — what the sandbox's `peers[].url` gets.
	// https because the app refuses anything else (see the file comment).
	URL string

	ts *httptest.Server
	// card is the Agent Card JSON, built once the origin above is known and read by the handler.
	card []byte

	mu sync.Mutex
	// cardRequests counts the card reads, so the Go half can tell "the panel asked" from "the
	// panel drew something it had cached".
	cardRequests int
}

// newPeerFixture starts the fake node on a loopback port. Close it when done.
func newPeerFixture() (*peerFixture, error) {
	f := &peerFixture{}
	// Unstarted first, so the card can be built once the origin is known (below): the card names the
	// address that actually answers, which is only fixed after the listener exists.
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != a2asrv.WellKnownAgentCardPath {
			http.NotFound(w, r)
			return
		}
		f.mu.Lock()
		f.cardRequests++
		card := f.card
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(card)
	}))
	// ed25519，不是 httptest 默认的 RSA：app 认的是**身份**（node/auth.go 拒任何非 ed25519 的对端），
	// 而 httptest 的自签证书是 RSA —— 那样握手能过、读卡会被拒，报
	// 「presented a *rsa.PublicKey key; this node pins ed25519 identities」。
	cert, err := selfSignedEd25519()
	if err != nil {
		ts.Close()
		return nil, err
	}
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	ts.StartTLS()
	f.ts, f.URL = ts, ts.URL

	// The card names the address that actually answers — the fixture's own origin — because that is
	// the rule a real node follows too (node/advertisedURL: a Card that named somewhere else sends
	// every caller to a port that does not answer).
	card, err := json.Marshal(a2a.AgentCard{
		Name:        peerFixtureCardName,
		Description: "沙箱里的假节点：让 Agent Card 的字段出现在面板上",
		Version:     peerFixtureVersion,
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface(f.URL, a2a.TransportProtocolHTTPJSON),
		},
		Capabilities:       a2a.AgentCapabilities{Streaming: true, PushNotifications: true},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		// Three skills, because the panel's job here is a LIST: one entry proves nothing about how
		// the second one is drawn.
		Skills: []a2a.AgentSkill{
			{ID: "docs-read", Name: "docs-read", Description: "读团队文档库里的文章、表格与会议纪要", Tags: []string{"docs"}},
			{ID: "log-search", Name: "log-search", Description: "按关键词查登录、登出与会话日志", Tags: []string{"log"}},
			{ID: "snapshot-backfill", Name: "snapshot-backfill", Description: "提交报表补数任务并轮询进度", Tags: []string{"report"}},
		},
	})
	if err != nil {
		ts.Close()
		return nil, err
	}

	f.mu.Lock()
	f.card = card
	f.mu.Unlock()
	return f, nil
}

// CardRequests reports how many times the card was read.
func (f *peerFixture) CardRequests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cardRequests
}

// Close stops the fake node.
func (f *peerFixture) Close() { f.ts.Close() }

// peerSeed is one entry of the sandbox's `peers:` list, declared by a scenario.
//
// It is structured rather than a YAML string in the scenario's `config` for the same reason the MCP
// OAuth fixture is: the answering peer's port is only known once the runner has it up, and a second
// `peers:` key in the same YAML would replace the first rather than extend it — so the whole list is
// rendered together, at the point where every URL is known.
type peerSeed struct {
	name string
	// url is the configured address for a peer the runner does NOT serve. An unreachable peer has to
	// be unreachable ON PURPOSE: 127.0.0.1:1 refuses instantly instead of waiting out a timeout,
	// which is what keeps the panel's rows fast enough to assert on.
	url  string
	note string
	// answers makes the runner serve this peer's Agent Card (peerFixture above), and the seed's url
	// is then ignored — the fixture's own origin is the only address that can be true.
	answers bool
}

// startPeerFixtures brings up the answering peers and returns the `peers:` YAML for the sandbox
// config, plus what it started (so the Go half can assert on the requests).
func startPeerFixtures(seeds []peerSeed) (string, []*peerFixture, error) {
	var (
		peers   []config.PeerConfig
		fixture []*peerFixture
	)
	for _, s := range seeds {
		url := s.url
		if s.answers {
			f, err := newPeerFixture()
			if err != nil {
				for _, started := range fixture {
					started.Close()
				}
				return "", nil, err
			}
			fixture = append(fixture, f)
			url = f.URL
		}
		peers = append(peers, config.PeerConfig{Name: s.name, URL: url, Note: s.note})
	}
	if len(peers) == 0 {
		return "", nil, nil
	}
	raw, err := yamlMarshalPeers(peers)
	if err != nil {
		for _, started := range fixture {
			started.Close()
		}
		return "", nil, err
	}
	return raw, fixture, nil
}

// yamlMarshalPeers renders the list as the top-level `peers:` block of config.yaml. It goes through
// config.PeerConfig — the type the app loads — rather than a format string, so a scenario cannot hand
// the app an entry its loader would read differently (the same reason sandbox.writeConfig marshals
// config.PricingConfig).
func yamlMarshalPeers(peers []config.PeerConfig) (string, error) {
	raw, err := yaml.Marshal(map[string]any{"peers": peers})
	if err != nil {
		return "", fmt.Errorf("marshal peers: %w", err)
	}
	return string(raw), nil
}

// peerCardReads is how many card reads the running fixtures saw. The Go half asserts on it because a
// card on screen can only have come from one: 「面板把缓存的卡片画出来了」 and 「面板问了那个节点」
// look identical in the DOM.
func peerCardReads(fixtures []*peerFixture) int {
	total := 0
	for _, f := range fixtures {
		total += f.CardRequests()
	}
	return total
}

// selfSignedEd25519 makes the certificate a real Tachi node would present: self-signed and ed25519.
// The key type is the load-bearing part — the app pins identities by ed25519 public key and refuses
// anything else (node/auth.go), so an RSA certificate cannot even be looked at.
func selfSignedEd25519() (tls.Certificate, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "smoke-peer"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, nil
}
