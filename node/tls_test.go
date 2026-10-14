package node

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/fileutil"
)

// The two halves of the transport's identity story, tested end to end: the certificate a node
// serves is ITS identity, and a caller remembers the key it saw the first time (TOFU).

// tlsPeer stands up an HTTPS peer that answers like a node. Its certificate comes from id through
// the real serverTLSConfig, so what a client verifies is exactly what a real node would present —
// and the stub's 200 IS the handshake result, which is why the request body is irrelevant here.
func tlsPeer(t *testing.T, id *Identity) string {
	t.Helper()
	cert, err := loadOrCreateCertificate(t.TempDir(), id)
	require.NoError(t, err)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", a2aMediaType)
		_, _ = w.Write([]byte(`{"task":{"id":"t-1","contextId":"c-1","status":{"state":"TASK_STATE_COMPLETED"}}}`))
	}))
	// A server certificate only: these tests are about what the CALLER verifies, and a peer that
	// demands nothing of its callers is the simplest thing that can present one. The direction the
	// client certificate travels is tested from the other side (node/auth_test.go).
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.URL
}

// tlsNode starts a node whose remote surface is OFF loopback, which is what makes it serve TLS.
//
// The switch is the address, deliberately (node/options.go): leaving the machine is what makes
// encryption and identification necessary. So exercising the mTLS path at all means going off
// loopback, and a wildcard bind is the only safe way to do that in a test. It comes with its own
// requirement, which is the point of the pair below: a node that binds a wildcard has to SAY where
// callers reach it, or it will not start (node/a2a.go) — so the test picks a port, names it in
// public_url, and the Card then carries an address that really answers.
func tlsNode(t *testing.T, dir string, mutate func(*Options)) *Handle {
	t.Helper()
	port := freeWildcardPort(t)
	return startWith(t, dir, func(o *Options) {
		o.RemoteListen = net.JoinHostPort("0.0.0.0", strconv.Itoa(port))
		o.PublicURL = publicURLFor(t, port)
		if mutate != nil {
			mutate(o)
		}
	})
}

// dialAddr is where to reach a node bound to 0.0.0.0: the wildcard is not an address anything can
// dial, so the port is what matters and loopback is the host to use.
func dialAddr(t *testing.T, h *Handle) string {
	t.Helper()
	_, port, err := net.SplitHostPort(h.RemoteAddr())
	require.NoError(t, err)
	return net.JoinHostPort("127.0.0.1", port)
}

// newSenderIdentity is one machine's identity, in a data dir of its own: what the CALLER presents.
func newSenderIdentity(t *testing.T) *Identity {
	t.Helper()
	id, err := LoadOrCreateIdentity(t.TempDir())
	require.NoError(t, err)
	return id
}

// sendAs is one message:send from one caller's identity, over the real TLS surface.
//
// The SERVER's certificate is not checked here: these tests are about a server accepting a client,
// and the client's own check has its own tests (below). What is NOT skipped is the handshake —
// every call goes through it, which is the point.
//
// The body carries no `configuration`: the protocol's default is to wait for a terminal state, so
// nothing is left running when the test ends.
func sendAs(t *testing.T, h *Handle, id *Identity, text string) (int, string, error) {
	t.Helper()
	body := `{"message":{"messageId":"` + a2a.NewMessageID() +
		`","role":"ROLE_USER","parts":[{"text":"` + text + `"}]}}`
	return callAs(t, h, id, http.MethodPost, restSendPath, body)
}

// callAs is one request from one caller's identity against a node's remote surface.
func callAs(t *testing.T, h *Handle, id *Identity, method, path, body string) (int, string, error) {
	t.Helper()
	return callWithHeaders(t, h, id, method, path, body, nil)
}

// callWithHeaders is callAs with extra headers, for what a caller says about itself.
func callWithHeaders(t *testing.T, h *Handle, id *Identity, method, path, body string, headers map[string]string) (int, string, error) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, "https://"+dialAddr(t, h)+path, reader)
	require.NoError(t, err)
	if body != "" {
		req.Header.Set("Content-Type", a2aMediaType)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := testHTTPSClient(t, id).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(raw), nil
}

// readAs is one authenticated GET.
func readAs(t *testing.T, h *Handle, id *Identity, path string) (int, string) {
	t.Helper()
	status, raw, err := callAs(t, h, id, http.MethodGet, path, "")
	require.NoError(t, err)
	return status, raw
}

// testHTTPSClient presents id's certificate.
func testHTTPSClient(t *testing.T, id *Identity) *http.Client {
	t.Helper()
	cert, err := loadOrCreateCertificate(t.TempDir(), id)
	require.NoError(t, err)
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	}}}
}

// withBaseDir points this machine's base dir at a temp dir for the duration of a test: the pin
// record lives there (config.PeerIdentitiesPath), and a test must never touch the real one.
func withBaseDir(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	previous := config.BaseDir()
	config.SetBaseDir(base)
	t.Cleanup(func() { config.SetBaseDir(previous) })
	return base
}

func delegateTo(t *testing.T, sender *Sender, prompt string) (tools.RemoteTaskInfo, error) {
	t.Helper()
	return delegateToNamed(t, sender, "peer", prompt)
}

func delegateToNamed(t *testing.T, sender *Sender, label, prompt string) (tools.RemoteTaskInfo, error) {
	t.Helper()
	return sender.Delegate(context.Background(), tools.DelegationRequest{
		Peer: pinAndName(t, sender, label), Prompt: prompt, LogicalID: "conv-1",
	})
}

// pinAndName names a peer the way the MODEL does — by identity — after making the machine prove one.
//
// The probe is the same card read the reminder performs before the model acts, and it is what pins
// the identity (node/peerdir.go). A test that passed the LABEL instead would be using a path this
// program does not have: a label decides nothing about where work goes.
func pinAndName(t *testing.T, sender *Sender, label string) string {
	t.Helper()
	for _, p := range sender.Peers(context.Background()) {
		if p.Name == label && p.NodeID != "" {
			return p.NodeID
		}
	}
	t.Fatalf("no peer named %q proved an identity; a peer that serves plaintext proves none", label)
	return ""
}

func TestTheCertificateIsTheNodesOwnIdentity(t *testing.T) {
	dir := t.TempDir()
	id, err := LoadOrCreateIdentity(dir)
	require.NoError(t, err)

	cert, err := loadOrCreateCertificate(dir, id)
	require.NoError(t, err)
	require.NotNil(t, cert.Leaf)
	pub, ok := cert.Leaf.PublicKey.(ed25519.PublicKey)
	require.True(t, ok, "the certificate carries the ed25519 identity key")
	require.Equal(t, id.NodeID, NodeIDFor(pub),
		"the pinned node id IS this certificate's key — that is what makes the pin checkable")

	// Reloading gives the same certificate: a node that re-made one per start would re-pair every
	// time (and every peer's pin would break).
	again, err := loadOrCreateCertificate(dir, id)
	require.NoError(t, err)
	require.Equal(t, cert.Leaf.Raw, again.Leaf.Raw)

	// An identity that is not this one gets a certificate for ITSELF, even in the same directory:
	// presenting an identity the node does not have would be worse than presenting none.
	other, err := LoadOrCreateIdentity(t.TempDir())
	require.NoError(t, err)
	mine, err := loadOrCreateCertificate(dir, other)
	require.NoError(t, err)
	otherPub, ok := mine.Leaf.PublicKey.(ed25519.PublicKey)
	require.True(t, ok)
	require.Equal(t, other.NodeID, NodeIDFor(otherPub))
	require.NotEqual(t, cert.Leaf.Raw, mine.Leaf.Raw)
}

func TestAPeerIsPinnedOnFirstContactAndCheckAfter(t *testing.T) {
	withBaseDir(t)
	real, err := LoadOrCreateIdentity(t.TempDir())
	require.NoError(t, err)
	realURL := tlsPeer(t, real)

	// First contact: nothing is pinned yet, so the key is recorded and the request goes through.
	sender, err := OpenSender([]config.PeerConfig{{Name: "peer", URL: realURL}}, "", nil, nil)
	require.NoError(t, err)
	view, err := delegateTo(t, sender, "hi")
	require.NoError(t, err)

	record, ok := sender.records.get(view.ID)
	require.True(t, ok)
	require.Equal(t, real.NodeID, record.PeerNodeID,
		"the identity on the record is the one the HANDSHAKE proved, not something the peer claimed")

	// It is on disk. TOFU that forgets is TOFU that has to trust a stranger again on every launch.
	var pinned map[string]knownPeer
	require.NoError(t, fileutil.ReadJSON(config.PeerIdentitiesPath(), &pinned))
	require.Equal(t, "peer", pinned[real.NodeID].Alias, "the record is keyed by the identity the handshake proved")
	require.False(t, pinned[real.NodeID].FirstSeen.IsZero(), "when the pin was made is part of the record")
	require.Equal(t, realURL, pinned[real.NodeID].URL, "and so is where it was made")

	// The same machine again — a NEW sender, so this reads the record rather than a cache.
	again, err := OpenSender([]config.PeerConfig{{Name: "peer", URL: realURL}}, "", nil, nil)
	require.NoError(t, err)
	_, err = delegateTo(t, again, "hi again")
	require.NoError(t, err)

	// A DIFFERENT machine answering AT that address: refused, and the refusal says where the pin is
	// so the operator has somewhere to go. This is the second conversation onwards — the first one
	// is only as trustworthy as the path to it.
	//
	// It is the one protection TOFU keeps under identity-keying, and the shape it takes is "one
	// ADDRESS, one identity" rather than the old "one name, one identity": a name stopped being a
	// key when a tool argument started naming machines by identity, while an address is still where
	// this machine was told to look. A different machine at a different address is simply another
	// machine (the design's rule), and the record keeps both.
	impostor, err := LoadOrCreateIdentity(t.TempDir())
	require.NoError(t, err)
	err = sender.known.confirm("peer", realURL, impostor.NodeID)
	require.ErrorIs(t, err, ErrIdentityMismatch, "a changed key at a known address is what the pin catches")
	require.Contains(t, err.Error(), "was pinned as node")
	require.Contains(t, err.Error(), real.NodeID, "both ids, so the operator can tell which is which")
	require.Contains(t, err.Error(), impostor.NodeID)
	require.Contains(t, err.Error(), filepath.Base(config.PeerIdentitiesPath()),
		"the refusal has to say which file to edit to pair again")

	// And the impostor is not recorded: a refusal that wrote the record anyway would have spent the
	// check it just made.
	require.NoError(t, fileutil.ReadJSON(config.PeerIdentitiesPath(), &pinned))
	require.Equal(t, realURL, pinned[real.NodeID].URL)
	_, recorded := pinned[impostor.NodeID]
	require.False(t, recorded)
}

// A delegation carries the prompt and its answer, so where it may travel is a rule rather than a
// preference — and the rule is https for EVERY host, loopback included.
//
// The exemption loopback used to have is gone because of what naming a peer by identity means: a peer
// reached in plaintext presents no certificate, so no identity can be pinned for it, so no tool
// argument can name it. An http line was a configuration entry that could never be used — a row
// offering a delegation it could not make — and refusing it says so while it can still be fixed.
func TestAPeerMustBeHTTPS(t *testing.T) {
	require.NoError(t, checkPeerURL("nas", "https://nas.example:7080"))
	require.NoError(t, checkPeerURL("local", "https://127.0.0.1:7080"),
		"https is accepted for ANY host: the pin, not a hostname, says which machine answered")

	for _, bad := range []string{
		"", "http://127.0.0.1:7080", "http://[::1]:7080", "http://nas.example:7080", "http://10.0.0.10:7080",
		"nas.example:7080", "://x", "ftp://nas.example",
	} {
		require.Error(t, checkPeerURL("nas", bad), "%q must be refused", bad)
	}
	require.Contains(t, checkPeerURL("nas", "http://nas.example:7080").Error(), "https",
		"the refusal has to say what would work")
}

// The sending half: a node introduces itself on every call, which is what lets the receiving side
// write a readable label. Asserted against a stub peer because the header is exactly the kind of
// plumbing that silently stops being set.
func TestACallerIntroducesItselfOnEveryCall(t *testing.T) {
	var mu sync.Mutex
	var seen []http.Header
	// A TLS peer, because a peer has to be one to be named at all — and the name still travels on
	// every binding, which is what is under test here rather than the transport.
	srv, _ := tlsStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		mu.Unlock()
		w.Header().Set("Content-Type", a2aMediaType)
		_, _ = w.Write([]byte(`{"task":{"id":"t-1","contextId":"c-1","status":{"state":"TASK_STATE_COMPLETED"}}}`))
	}))

	withBaseDir(t)
	sender, err := OpenSender([]config.PeerConfig{{Name: "peer", URL: srv.URL}}, "tachi@mac", nil, nil)
	require.NoError(t, err)
	_, err = delegateTo(t, sender, "hi")
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	// Two requests: the card fetch the directory does first, and the message itself. The name
	// belongs on the message — that is the one a peer files as work.
	var sends []http.Header
	for _, h := range seen {
		if h.Get("Content-Type") == a2aMediaType {
			sends = append(sends, h)
		}
	}
	require.Len(t, sends, 1, "one message went out")
	require.Equal(t, "tachi@mac", sends[0].Get(nodeNameHeader),
		"the caller declares its name on the way out — it is what the peer's records show")
}

// A peer that serves a card AND answers work — enough for the two tests below, which are about
// what this machine decides BEFORE sending anything.
func cardAndTaskPeer(t *testing.T, id *Identity) (string, func() int) {
	t.Helper()
	cert, err := loadOrCreateCertificate(t.TempDir(), id)
	require.NoError(t, err)
	var mu sync.Mutex
	sends := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", a2aMediaType)
		if strings.HasSuffix(r.URL.Path, "agent-card.json") {
			_, _ = w.Write([]byte(`{"name":"tachi@the-machine","version":"test"}`))
			return
		}
		mu.Lock()
		sends++
		mu.Unlock()
		_, _ = w.Write([]byte(`{"task":{"id":"t-1","contextId":"c-1","status":{"state":"TASK_STATE_COMPLETED"}}}`))
	})
	srv := httptest.NewUnstartedServer(handler)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.URL, func() int { mu.Lock(); defer mu.Unlock(); return sends }
}

// The record binds an IDENTITY, so a rename on this side is still the same conversation — and the
// name shown for it follows the config rather than the record.
func TestARenamedPeerIsStillTheSameMachine(t *testing.T) {
	withBaseDir(t)
	machine, err := LoadOrCreateIdentity(t.TempDir())
	require.NoError(t, err)
	url, sends := cardAndTaskPeer(t, machine)

	first, err := OpenSender([]config.PeerConfig{{Name: "nas", URL: url}}, "tachi@mac", nil, nil)
	require.NoError(t, err)
	started, err := delegateToNamed(t, first, "nas", "第一轮")
	require.NoError(t, err)

	// Same machine, same address, NEW local name — and with the identity the record holds, the
	// follow-up still belongs to it (design §4.1: 别名变化不能改变任务归属).
	renamed, err := OpenSender([]config.PeerConfig{{Name: "home-nas", URL: url}}, "tachi@mac", nil, nil)
	require.NoError(t, err)
	_, err = renamed.Delegate(context.Background(), tools.DelegationRequest{
		Peer: pinAndName(t, renamed, "home-nas"), Prompt: "接着来", LogicalID: "conv-1", ContinueFrom: started.ID,
	})
	require.NoError(t, err, "a renamed peer is the same peer")

	// And the record is shown under the name the config uses NOW: a reminder that named it "nas"
	// would hand the model a name its own tool call would be refused for.
	listed := renamed.RemoteTasks("conv-1")
	require.NotEmpty(t, listed)
	require.Equal(t, "home-nas", listed[len(listed)-1].Node)
	require.GreaterOrEqual(t, sends(), 2, "the follow-up really went out")
}

// The rename again, but this time the follow-up names NOTHING but the record — the shape the tool
// actually produces (it refuses to take `node` and `continue_from` together).
//
// This is the case that needs the third step in peerForContinuation: the pin record is keyed by the
// alias a peer was reached under, so a name the config adopted a minute ago is in no pin yet, and
// the alias the record was created with is gone. Neither index has it — the machine has to be ASKED
// which one it is, which is what makes a rename survivable without the caller naming it.
//
// Without that step the model is stuck: continuing a renamed peer's conversation would be refused
// as "no configured peer answers as node X", and it may not pass `node` to say otherwise.
func TestAFollowUpWithoutANodeFindsItsMachineAfterARename(t *testing.T) {
	withBaseDir(t)
	machine, err := LoadOrCreateIdentity(t.TempDir())
	require.NoError(t, err)
	url, sends := cardAndTaskPeer(t, machine)

	first, err := OpenSender([]config.PeerConfig{{Name: "nas", URL: url}}, "tachi@mac", nil, nil)
	require.NoError(t, err)
	started, err := delegateToNamed(t, first, "nas", "第一轮")
	require.NoError(t, err)
	require.Equal(t, machine.NodeID, mustPeerNodeID(t, first.records, started.ID),
		"the record really does bind the identity — this whole path rests on it")

	renamed, err := OpenSender([]config.PeerConfig{{Name: "home-nas", URL: url}}, "tachi@mac", nil, nil)
	require.NoError(t, err)
	continued, err := renamed.Delegate(context.Background(), tools.DelegationRequest{
		Prompt: "接着来", LogicalID: "conv-1", ContinueFrom: started.ID,
	})
	require.NoError(t, err, "the identity is what the record binds, so the new name still resolves")
	require.Equal(t, "home-nas", continued.Node, "and the view names it the way the config does now")
	require.NotEqual(t, started.ID, continued.ID, "a follow-up is new work, not the old record again")
	require.GreaterOrEqual(t, sends(), 2, "the follow-up really went out")
}

// mustPeerNodeID is the identity a record holds, for tests that need to assert the binding itself.
func mustPeerNodeID(t *testing.T, records *delegationStore, id string) string {
	t.Helper()
	rec, ok := records.get(id)
	require.True(t, ok, "no record %s", id)
	return rec.PeerNodeID
}

// The other half, and the case the identity check exists for: TWO configured peers, and a
// follow-up that names the wrong one. Nothing else can catch this — each alias has its own pin, so
// the handshake is happy to talk to either machine; only comparing the identity on the record
// against the identity that answers can refuse it. The refusal must also name the two node ids,
// because the alias comparison cannot: it is the same machine the caller claims to continue.
func TestAContinuationIsRefusedBeforeAMessageReachesAnotherMachine(t *testing.T) {
	withBaseDir(t)
	home, err := LoadOrCreateIdentity(t.TempDir())
	require.NoError(t, err)
	homeURL, _ := cardAndTaskPeer(t, home)

	other, err := LoadOrCreateIdentity(t.TempDir())
	require.NoError(t, err)
	otherURL, otherSends := cardAndTaskPeer(t, other)

	// Both machines are configured, under their own names: this is the everyday cluster.
	sender, err := OpenSender([]config.PeerConfig{
		{Name: "nas", URL: homeURL},
		{Name: "hk-vm", URL: otherURL},
	}, "tachi@mac", nil, nil)
	require.NoError(t, err)

	started, err := delegateToNamed(t, sender, "nas", "第一轮")
	require.NoError(t, err)

	_, err = sender.Delegate(context.Background(), tools.DelegationRequest{
		Peer: pinAndName(t, sender, "hk-vm"), Prompt: "接着来", LogicalID: "conv-1", ContinueFrom: started.ID,
	})
	require.Error(t, err, "a conversation with the NAS cannot continue on the VPS")
	require.Contains(t, err.Error(), home.NodeID, "the refusal names the node the record belongs to")
	require.Contains(t, err.Error(), other.NodeID, "…and the node that answered")
	require.Zero(t, otherSends(), "no message may reach the wrong machine, not even a harmless one")
}

// Both halves together, which is the only shape a real deployment has: OUR sender against OUR
// server, over TLS, with the caller's certificate read by the receiving side and the card fetched
// through the same connection. Each half has its own tests above; the seam between them is where
// an integration assumption would hide (the client certificate not actually being presented, the
// card fetch not going through the pinned client, an identity that never lands on the record).
//
// The node here is the PLACEHOLDER executor (testOptions wires no agent), so what comes back is the
// prompt echoed as an artifact — enough to prove the round trip, which is what this test is for.
func TestTwoRealNodesTalkOverTLS(t *testing.T) {
	base := withBaseDir(t)
	serverID, err := LoadOrCreateIdentity(t.TempDir())
	require.NoError(t, err)

	h := tlsNode(t, t.TempDir(), func(o *Options) {
		o.Identity = serverID
		o.Name = "tachi@home-nas"
	})
	url := "https://" + dialAddr(t, h)

	// The caller is a machine of its own: the identity OpenSender makes under this base dir is what
	// the server reads, and it must be a DIFFERENT key than the server's.
	sender, err := OpenSender([]config.PeerConfig{{Name: "nas", URL: url}}, "tachi@mac", nil, nil)
	require.NoError(t, err)

	view, err := delegateToNamed(t, sender, "nas", "echo this back")
	require.NoError(t, err, "a delegation over mTLS reaches the node")
	require.Equal(t, "completed", view.State)
	require.Contains(t, view.Result, "echo this back", "and its answer comes back")

	// The record keeps the machine it really talked to, proved by the handshake — which is what the
	// continuation checks use.
	record, ok := sender.records.get(view.ID)
	require.True(t, ok)
	require.Equal(t, serverID.NodeID, record.PeerNodeID)

	// The card was readable through the same connection: `Lookup` tolerates a card failure, so
	// this is the assertion that proves the fetch plumbing (self-signed certificate + client
	// certificate) rather than assuming it.
	peers := sender.Peers(context.Background())
	require.Len(t, peers, 1)
	require.True(t, peers[0].Reachable, "the card must be readable over mTLS: %+v", peers[0])

	// And the server side knows who asked: its own identity is not the caller's.
	own, err := LoadOrCreateIdentity(filepath.Join(base, "a2a"))
	require.NoError(t, err)
	require.NotEqual(t, serverID.NodeID, own.NodeID, "a caller must not be the machine it called")
}
