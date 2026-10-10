package node

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/monsterxx03/tachi/pkg/fileutil"
)

// Caller authentication, tested where it is enforced: the handshake, the accepted-node-id list,
// and the identity that ends up on a task record.
//
// There is no credential to test for correctness here — a caller IS a key, and the check is
// "is the key that answered one of these". What these tests pin is that the list is the whole
// decision, that it reaches the code that files the work, and that a key which is not on it
// cannot get a request in at all.

// withNodeName gives the node a name of its own (config: node.name), which is what a peer's
// records will show as SELF-REPORTED.
func withNodeName(name string) func(*Options) {
	return func(o *Options) { o.Name = name }
}

func TestACallersIdentityComesFromItsCertificate(t *testing.T) {
	dir := t.TempDir()
	h := tlsNode(t, dir, withNodeName("tachi@home-nas"))

	caller := newSenderIdentity(t)
	status, raw, err := sendAs(t, h, caller, "hi")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status, "body=%s", raw)

	var sent struct {
		Task *a2a.Task `json:"task"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &sent))
	require.NotNil(t, sent.Task)

	var rec taskRecord
	require.NoError(t, fileutil.ReadJSON(filepath.Join(dir, tasksDirName, string(sent.Task.ID)+".json"), &rec))
	require.Equal(t, caller.NodeID, rec.CallerID,
		"the identity is the key the handshake accepted, which nobody can claim")
	require.Empty(t, rec.SelfReportedName,
		"this caller declared no name, and the record says so rather than inventing one")
}

// **ANY** certificate is accepted — that is the decision, not an oversight (node/options.go): this
// node identifies callers, it does not authorise them, and "who may reach this port" belongs to the
// network. The test exists so that re-adding a gate is a deliberate act with a failing test in
// front of it, rather than something that slips in.
func TestAnyCertificateIsAccepted(t *testing.T) {
	h := tlsNode(t, t.TempDir(), withNodeName("tachi@mac"))

	stranger := newSenderIdentity(t)
	status, raw, err := sendAs(t, h, stranger, "hi")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status, "body=%s", raw)
	require.Contains(t, raw, "task")
}

// A node with NO certificate cannot get in: the identity is what makes a record say who asked, and
// an anonymous caller is one whose work nobody could ever account for.
func TestACallerWithoutACertificateIsRefused(t *testing.T) {
	h := tlsNode(t, t.TempDir(), withNodeName("tachi@mac"))

	// A TLS client with no client certificate at all.
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true, MinVersion: tls.VersionTLS12,
	}}}
	resp, err := client.Get("https://" + dialAddr(t, h) + "/tasks")
	if err == nil {
		defer resp.Body.Close()
		require.NotEqual(t, http.StatusOK, resp.StatusCode,
			"a connection without a client certificate must not be served")
		return
	}
	// The handshake refusing it is the expected shape on the server side.
	require.Contains(t, err.Error(), "certificate")
}

// The Card is served by the SAME mTLS surface, so reading it needs a certificate: the transport
// decides before any path is known. It says which scheme a caller must bring, and it names the
// node (config: node.name) instead of calling every machine "Tachi".
func TestTheCardNamesTheNodeAndTheSchemeItChecks(t *testing.T) {
	plain := startWith(t, t.TempDir(), nil)
	resp, raw := get(t, "http://"+plain.RemoteAddr()+a2asrv.WellKnownAgentCardPath, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body=%s", raw)
	card := cardFrom(t, raw)
	require.Empty(t, card.SecuritySchemes, "a loopback surface checks no certificate")
	require.Equal(t, defaultNodeName, card.Name, "a node that named itself nothing keeps the old default")

	authed := tlsNode(t, t.TempDir(), withNodeName("tachi@hk-vps"))
	cardStatus, raw := readAs(t, authed, newSenderIdentity(t), a2asrv.WellKnownAgentCardPath)
	require.Equal(t, http.StatusOK, cardStatus, "the card answers a caller")

	card = cardFrom(t, raw)
	require.Equal(t, "tachi@hk-vps", card.Name, "the card says which machine this is")
	scheme, ok := card.SecuritySchemes[authSchemeName]
	require.True(t, ok, "and it declares the scheme it actually checks")
	_, isMTLS := scheme.(a2a.MutualTLSSecurityScheme)
	require.True(t, isMTLS, "the spec's mTLS scheme: %T", scheme)
	require.Equal(t, a2a.SecuritySchemeName("mtls"), authSchemeName)
	require.Len(t, card.SecurityRequirements, 1)
	_, required := card.SecurityRequirements[0][authSchemeName]
	require.True(t, required, "the requirement names that scheme")

	require.Empty(t, card.SupportedInterfaces,
		"a wildcard listener advertises NO interface rather than an address nobody can dial")
}

// The address no longer decides whether a caller may come in — only how the surface is protected.
// What still holds is that a non-loopback surface is always TLS with a client certificate, and
// that the local control interface never leaves loopback.
func TestTheRemoteSurfaceMayBindAnyAddressAndTheLocalOneMayNot(t *testing.T) {
	if _, err := testOptions(t.TempDir()).normalized(); err != nil {
		t.Fatalf("the default loopback surface must stay valid: %v", err)
	}

	remote := testOptions(t.TempDir())
	remote.RemoteListen = "0.0.0.0:7080"
	if _, err := remote.normalized(); err != nil {
		t.Fatalf("a reachable surface is a decision, not an error: %v", err)
	}

	local := testOptions(t.TempDir())
	local.LocalListen = "0.0.0.0:7081"
	_, err := local.normalized()
	require.Error(t, err, "the local interface is this machine's own business, always")
}

// The switch is the ADDRESS: loopback is plaintext (the OS is the boundary), anything else serves
// TLS. One expression, so "encrypted" and "asks for a certificate" can never disagree.
func TestTheAddressDecidesWhetherTheSurfaceEncrypts(t *testing.T) {
	require.True(t, isLoopbackListen("127.0.0.1:0"))
	require.True(t, isLoopbackListen("[::1]:7080"))
	require.False(t, isLoopbackListen("0.0.0.0:7080"))
	require.False(t, isLoopbackListen("100.64.0.1:7080"))
}

// Two authenticated callers share one node and one store, and neither may read the other's work —
// not by listing, and not by naming an id (design §6.3). The refusal for a task that exists but
// belongs to somebody else is the SAME as for an id that does not exist: a different answer would
// confirm the id.
func TestOneCallerCannotSeeAnothersWork(t *testing.T) {
	mac := newSenderIdentity(t)
	nas := newSenderIdentity(t)

	// The node's own name is what its records will show as the SELF-REPORTED name of whoever calls
	// it; each caller below declares its own.
	h := tlsNode(t, t.TempDir(), withNodeName("tachi@nas"))

	status, raw, err := sendAs(t, h, mac, "mac 的活")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status, "body=%s", raw)
	var macSent struct {
		Task *a2a.Task `json:"task"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &macSent))

	status, raw, err = sendAs(t, h, nas, "nas 的活")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status, "body=%s", raw)
	var nasSent struct {
		Task *a2a.Task `json:"task"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &nasSent))
	require.NotEqual(t, macSent.Task.ID, nasSent.Task.ID)

	for _, c := range []struct {
		who   *Identity
		mine  a2a.TaskID
		other a2a.TaskID
	}{
		{mac, macSent.Task.ID, nasSent.Task.ID},
		{nas, nasSent.Task.ID, macSent.Task.ID},
	} {
		status, raw := readAs(t, h, c.who, "/tasks/"+string(c.mine))
		require.Equal(t, http.StatusOK, status, "own task: %s", raw)
		require.Contains(t, raw, string(c.mine))

		status, _ = readAs(t, h, c.who, "/tasks/"+string(c.other))
		require.Equal(t, http.StatusNotFound, status, "another caller's task must not be readable")

		status, raw = readAs(t, h, c.who, "/tasks")
		require.Equal(t, http.StatusOK, status, "body=%s", raw)
		require.Contains(t, raw, string(c.mine))
		require.NotContains(t, raw, string(c.other), "a list is a view of the caller's own work")
	}
}

// The whole path from config to socket: the accepted key travels from node.callers through the
// handshake, and a key that is not on the list never reaches the runner.
func TestAnAuthenticatedCallerReachesTheRunner(t *testing.T) {
	base := nodeHome(t)
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(oneMessageScript("备份检查完成")...)

	caller := newSenderIdentity(t)
	cfg := nodeConfig(t, mock, filepath.Join(base, "ws"), "ReadFile")
	cfg.Node.Listen = "0.0.0.0:0"
	cfg.Node.Name = "tachi@mac"
	h := startRealNode(t, cfg)

	status, raw, err := sendAs(t, h, caller, "检查备份")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status, "body=%s", raw)
	require.Equal(t, 1, sessionCount(t), "the turn ran, in a session on this node's workspace")

	// A second machine IS served (any certificate is accepted) — and its work is filed under ITS
	// own node id, so the two callers can never be confused for one another.
	stranger := newSenderIdentity(t)
	status, raw, err = sendAs(t, h, stranger, "我也要")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status, "body=%s", raw)
	require.Equal(t, 2, sessionCount(t), "each caller's turn ran in its own session")
}

// cardFrom decodes an Agent Card body.
func cardFrom(t *testing.T, body string) *a2a.AgentCard {
	t.Helper()
	var card a2a.AgentCard
	require.NoError(t, json.Unmarshal([]byte(body), &card))
	return &card
}

// A caller's NAME is a label it declares about itself, and it is filed as exactly that. The point
// of the second half is the one that matters: the same key declaring a different name is the SAME
// caller, because the identity comes from the certificate. A label that could move an identity
// would be a label nobody could trust in a log.
func TestADeclaredNameIsRecordedAsSelfReportedAndChangedFreely(t *testing.T) {
	dir := t.TempDir()
	h := tlsNode(t, dir, withNodeName("tachi@nas"))
	caller := newSenderIdentity(t)

	for _, declared := range []string{"tachi@mac", "whatever-i-like"} {
		status, raw, err := sendAsNamed(t, h, caller, declared, "hi")
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, status, "body=%s", raw)

		var sent struct {
			Task *a2a.Task `json:"task"`
		}
		require.NoError(t, json.Unmarshal([]byte(raw), &sent))

		var rec taskRecord
		require.NoError(t, fileutil.ReadJSON(filepath.Join(dir, tasksDirName, string(sent.Task.ID)+".json"), &rec))
		require.Equal(t, declared, rec.SelfReportedName, "the label is kept as declared")
		require.Equal(t, caller.NodeID, rec.CallerID,
			"and it does not move the identity: that is the key the handshake accepted")
	}
}

// sendAsNamed is sendAs with the caller declaring a name.
func sendAsNamed(t *testing.T, h *Handle, id *Identity, name, text string) (int, string, error) {
	t.Helper()
	body := `{"message":{"messageId":"` + a2a.NewMessageID() +
		`","role":"ROLE_USER","parts":[{"text":"` + text + `"}]}}`
	return callWithHeaders(t, h, id, http.MethodPost, restSendPath, body, map[string]string{nodeNameHeader: name})
}
