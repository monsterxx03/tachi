package node

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"iter"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/monsterxx03/tachi/pkg/logger"
)

// AgentVersion is what the Card publishes as the agent's own version. The entry
// points set it from the build's version string (the root main does it beside
// llm.Version); it stays "dev" for a build that has none.
var AgentVersion = "dev"

const (
	// mediaTypeText is what this node accepts and produces today: one text part in,
	// one text part out. Declared in the Card, and checked by the handler against it.
	mediaTypeText = "text/plain"

	// remoteReadHeaderTimeout bounds reading a request's headers. It is deliberately
	// NOT a whole-request timeout: a blocking message/send is allowed to take as long
	// as the task takes (design §6.2), so a deadline here would cut off exactly the
	// calls the protocol expects to wait.
	remoteReadHeaderTimeout = 10 * time.Second
)

// a2aSurface is the node's REMOTE face: what another node talks to.
//
// It is a separate listener with its own mux, deliberately not the local control
// interface (design §3.3): a remote credential must never reach the local
// management surface, and a local client must not depend on the remote one.
//
// WHO may call is the network's business; who DID call is this surface's (Options.checkRemoteListen
// states the trade). Off loopback it serves TLS and requires a client certificate, so a request
// that reaches the handler always carries an identity, and the Card declares exactly that. On
// loopback it serves plaintext and identifies nobody — nothing can read the conversation off a
// machine, and there is no caller to name.
type a2aSurface struct {
	ln   net.Listener
	srv  *http.Server
	card *a2a.AgentCard
	// state is the executor's half that lives OUTSIDE a request, when it has one: a question
	// waiting for an answer outlives the call that published it, and Close is what ends it
	// (see nodeState).
	state nodeState
}

func startA2ASurface(o Options, store *taskStore, identity *Identity) (*a2aSurface, error) {
	ln, err := net.Listen("tcp", o.RemoteListen)
	if err != nil {
		return nil, fmt.Errorf("node: listen %s: %w", o.RemoteListen, err)
	}

	// ONE switch decides three things, and they cannot drift because it is one expression: whether
	// this surface encrypts, whether it asks for a client certificate, and whether the identity
	// that certificate establishes is carried into the request context.
	//
	// The switch is the ADDRESS, not a config flag: leaving the machine is what makes encryption
	// and identification necessary, staying on loopback is what makes them pointless. Who may call
	// is deliberately NOT part of it — see checkRemoteListen.
	encrypted := !isLoopbackListen(o.RemoteListen)
	if encrypted {
		cert, err := loadOrCreateCertificate(o.DataDir, identity)
		if err != nil {
			_ = ln.Close()
			return nil, err
		}
		ln = tls.NewListener(ln, serverTLSConfig(cert, o.Logger))
	}

	executor := o.Executor
	if executor == nil {
		executor = echoExecutor{}
	}
	// Where callers can reach this node, which is not the same fact as where it binds. Deriving it can
	// FAIL, and that failure is the start's problem rather than something to work around: a wildcard
	// address names no host, so a Card built from it would carry no interface URL at all, and the
	// machines that read Cards use those as the addresses to try. A node that serves others and cannot
	// say where it is has nothing to announce and nothing to answer with — better to refuse to start
	// and say how to fix it than to run as a surface nobody can find (design §一.13).
	advertised := advertisedURL(o.PublicURL, ln.Addr().String(), encrypted)
	if advertised == "" {
		// The port is named in the fix because it is the one part of the address this process knows
		// and the operator does not (a listener on port 0 was given one by the kernel).
		_, port, _ := net.SplitHostPort(ln.Addr().String())
		_ = ln.Close()
		return nil, fmt.Errorf("node: node.listen is %s, a wildcard address, so this node cannot say "+
			"where callers reach it — and a node that serves other machines has to be able to. Set "+
			"node.public_url (e.g. https://<this-machine>:%s) or bind node.listen to a concrete address",
			o.RemoteListen, port)
	}
	card := newAgentCard(o.Name, advertised, o.Description, o.Skills, encrypted)
	// The store is opened by the host, which is AFTER the executor is built (the store owns
	// the startup recovery, so there is exactly one). An executor that keeps state outside a
	// request — a question waiting for an answer, and its deadline — needs it, and this is the
	// one place both exist. One instance, or the two halves would hold two views of the same
	// tasks and their version checks would fight.
	var state nodeState
	if ns, ok := executor.(nodeState); ok {
		ns.consumeTaskStore(store)
		// Repairs what the last process left behind, now that both halves of the node's state
		// exist — and BEFORE this surface accepts anything. A hold whose task or conversation
		// is gone is released here, so a crash cannot leave a session protected forever.
		ns.reconcile()
		state = ns
	}
	var questions questionSource
	if qs, ok := executor.(questionSource); ok {
		questions = qs
	}
	// One source for two jobs: the capabilities the handler ENFORCES are the ones
	// the Card publishes, so "declared but not implemented" cannot happen by drift.
	//
	// The identity interceptor is added by the SAME condition that turned TLS on, and that is the
	// point rather than a coincidence: a caller certificate and the identity it establishes are one
	// arrangement, so a surface that asks for the first cannot forget the second. On loopback
	// neither exists — nobody is identified, and the Card declares no scheme.
	//
	// When it is on, the interceptor goes FIRST among the interceptors: a caller this node cannot
	// identify has no business learning whether its protocol version is supported either.
	interceptors := []a2asrv.CallInterceptor{protocolVersionInterceptor{}, &admission{store: store}}
	if encrypted {
		// See node/auth.go for why the identity has to be attached HERE and not by the middleware
		// that already read it off the connection.
		interceptors = append([]a2asrv.CallInterceptor{callerIdentityInterceptor{}}, interceptors...)
	}
	handler := a2asrv.NewHandler(executor,
		a2asrv.WithCapabilityChecks(&card.Capabilities),
		// Our store is the authority for accepted tasks; the SDK's own default would be
		// an in-memory map that forgets them at exit (design §9.1).
		a2asrv.WithTaskStore(store),
		a2asrv.WithCallInterceptors(interceptors...),
	)

	mux := http.NewServeMux()
	// The REST binding's routes are the spec's own paths (/message:send,
	// /tasks/{id}, /tasks/{id}:cancel, ...) with no prefix, so it mounts at the root
	// of this listener — which is why the remote surface gets a mux of its own.
	//
	// The guard wraps the binding rather than living inside it: a refusal that
	// carries OUR reason and domain must not be rendered by the SDK, whose error
	// mapping would file it as INTERNAL_ERROR under the standard domain.
	protocol := busyGuard(store, questions, a2asrv.NewRESTHandler(handler))
	if encrypted {
		// It refuses nothing — the handshake already required a certificate, so by the time a
		// request exists the caller is identified. What it does is carry that identity, and the
		// name the caller declared, into the request context: node/auth.go.
		protocol = callerIdentityMiddleware(o.Logger, protocol)
	}
	mux.Handle("/", protocol)
	mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))
	// The private route for discovery by hearsay, beside the Card rather than behind the A2A handler:
	// it is not a spec route, so its refusals are this node's own words, and it needs no task store
	// (node/gossip.go explain what an answer means, which is "one candidate each").
	mux.Handle(gossipPath, gossipHandler(o.Gossip, identity.NodeID, o.ContactTTL, o.Logger))

	srv := &http.Server{
		Handler:           limitBody(mux, maxRequestBodyBytes),
		ReadHeaderTimeout: remoteReadHeaderTimeout,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			o.Logger.Error(context.Background(), "node: remote interface stopped", err, "addr", ln.Addr().String())
		}
	}()

	o.Logger.Info(context.Background(), "node: remote interface listening",
		"addr", ln.Addr().String(), "card", a2asrv.WellKnownAgentCardPath)

	return &a2aSurface{ln: ln, srv: srv, card: card, state: state}, nil
}

// limitBody caps every request body this listener will read.
//
// It is on the LISTENER rather than in the guard because the guard is one route: a body is read by
// whatever handles the request, and a limit covering only `message:send` would leave the rest of
// the surface as a way to make this process allocate whatever the caller likes. The guard keeps its
// own answer for the body it reads (a clear refusal in this node's own terms, see busyGuard), while
// this is the floor under all of them.
//
// MaxBytesReader is what makes it a bound rather than a post-mortem: reads fail once the limit is
// passed and the server is asked to close the connection, so a body nobody will finish reading is
// not buffered to its end first.
func limitBody(next http.Handler, limit int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
}

// Addr is the bound address, empty when there is no surface.
func (s *a2aSurface) Addr() string {
	if s == nil || s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Card is what this node publishes about itself.
func (s *a2aSurface) Card() *a2a.AgentCard {
	if s == nil {
		return nil
	}
	return s.card
}

// close drains the A2A listener, then terminates what is left (see shutdownGraceful: a
// bounded wait followed by termination is the design's own rule, not a failure).
//
// It ends the executor's out-of-request state FIRST: a question waiting for an answer is not
// attached to any connection, so draining the listener would leave its turn waiting for a
// message no one can send. (The record is not rewritten here — the next start resolves what
// was still open as interrupted, parked questions included: see openTaskStore.)
func (s *a2aSurface) close(log *logger.Logger) {
	if s == nil {
		return
	}
	if s.state != nil {
		s.state.shutdown()
	}
	shutdownGraceful(s.srv, log, "remote interface")
}

// advertisedURL is what the Card tells a caller to dial.
//
// `public_url` wins when the operator set it (a mesh name, a port forwarder, a real host name are
// all facts this process cannot derive). Otherwise the bound address is used — and when the bound
// address is unspecified there is nothing to derive, which the CALLER treats as a startup failure:
// 0.0.0.0 is not an address anyone can call, and a Card that named it would send every caller
// somewhere that does not answer, which is worse than a Card that admits it does not know (design
// §4.2, §一.13).
func advertisedURL(publicURL, boundAddr string, https bool) string {
	if u := strings.TrimSpace(publicURL); u != "" {
		return u
	}
	host, port, err := net.SplitHostPort(boundAddr)
	if err != nil || host == "" || host == "0.0.0.0" || host == "::" {
		return ""
	}
	scheme := "http"
	if https {
		scheme = "https"
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}

// newAgentCard describes what this node actually does — no more.
//
// Every optional capability stays false (nothing streams, nothing pushes) because that is
// the truth, and declaring one would be worse here than in a document: a PEER plans around a
// Card.
//
// The skills are exactly the skills THIS MACHINE's agent has — nothing is added for the protocol's
// sake. An earlier version also published a built-in "Remote task" entry (an "Echo" one behind the
// placeholder executor), and it was removed: a Card's skills are a claim about what can be done
// here, and a hardcoded entry is a claim about this SOFTWARE that every node makes identically,
// which tells a peer nothing about which machine to send work to. What a peer decides on is the
// machine's own skills and the description beside them.
//
// The description and the skills are self-declared, like the name: a peer records what this node
// says about itself, and none of it is evidence of anything but what the node chose to publish
// (design §4.2).
func newAgentCard(name, url, description string, machineSkills []a2a.AgentSkill, authenticated bool) *a2a.AgentCard {
	if strings.TrimSpace(name) == "" {
		name = defaultNodeName
	}
	if strings.TrimSpace(description) == "" {
		description = defaultNodeDescription
	}

	card := &a2a.AgentCard{
		Name:               name,
		Description:        description,
		Version:            AgentVersion,
		Capabilities:       a2a.AgentCapabilities{},
		DefaultInputModes:  []string{mediaTypeText},
		DefaultOutputModes: []string{mediaTypeText},
		Skills:             machineSkills,
	}
	if url != "" {
		card.SupportedInterfaces = []*a2a.AgentInterface{
			a2a.NewAgentInterface(url, a2a.TransportProtocolHTTPJSON),
		}
	}
	if authenticated {
		// What this node actually requires, in the spec's own vocabulary: mutual TLS. The client
		// brings a certificate and this node reads the caller's identity out of it — which is what
		// the spec's mTLS scheme means, and it is declared exactly when the surface serves TLS (a
		// Card that demanded a scheme the server does not check would be worse than one that asks
		// for nothing).
		//
		// Worth stating because the confusion runs the other way too: certificate PINNING on the
		// client side is not by itself mutual TLS (design §6.1). What makes this mutual is that
		// the SERVER asks for a client certificate as well — one certificate per node, the same
		// key in both directions.
		card.SecuritySchemes = a2a.NamedSecuritySchemes{
			authSchemeName: a2a.MutualTLSSecurityScheme{
				Description: "A Tachi node's own self-signed certificate. Any such certificate is accepted: this node identifies callers rather than authorising them.",
			},
		}
		card.SecurityRequirements = a2a.SecurityRequirementsOptions{
			a2a.SecurityRequirements{authSchemeName: {}},
		}
	}
	return card
}

// authSchemeName is the key the Card's scheme and its requirement share. One constant, because a
// requirement naming a scheme the Card does not declare is a Card that describes a different node.
const authSchemeName = a2a.SecuritySchemeName("mtls")

// defaultNodeName is what a node calls itself when the operator named it nothing: the string every
// node published before node.name existed, kept so an unconfigured node looks exactly as it did.
const defaultNodeName = "Tachi"

// defaultNodeDescription is what a node says it is for when the operator said nothing (config
// `node.description`). It describes what a Tachi node IS rather than what this one is for, which is
// why a cluster is better off filling the field in: a peering operator reading three identical
// descriptions has learned nothing about which machine to send work to.
const defaultNodeDescription = "A Tachi node: an agent that runs on the machine that owns it, reachable from the other Tachi nodes you have paired with."

// protocolVersionInterceptor rejects a call that names an A2A protocol version this
// node does not speak.
//
// The SDK sends A2A-Version on every request but never checks it on the way in, so
// this is the app's half of the negotiation: a client asking for another version
// gets the spec's VersionNotSupportedError instead of a silently different shape of
// answers. Absence is allowed — a client that names no version is asking for
// whatever this node speaks.
type protocolVersionInterceptor struct{}

var _ a2asrv.CallInterceptor = protocolVersionInterceptor{}

func (protocolVersionInterceptor) Before(ctx context.Context, callCtx *a2asrv.CallContext, _ *a2asrv.Request) (context.Context, any, error) {
	versions, ok := callCtx.ServiceParams().Get(a2a.SvcParamVersion)
	if !ok {
		return ctx, nil, nil
	}
	for _, v := range versions {
		if v != string(a2a.Version) {
			return ctx, nil, fmt.Errorf("%w: %q, this node speaks %q",
				a2a.ErrVersionNotSupported, v, a2a.Version)
		}
	}
	return ctx, nil, nil
}

func (protocolVersionInterceptor) After(context.Context, *a2asrv.CallContext, *a2asrv.Response) error {
	return nil
}

// echoExecutor is the placeholder that stands where the receiving side's real
// executor will be: it reports WORKING, returns the task text as an artifact, and
// completes. Cancel resolves the task to CANCELED, which the SDK does not do for
// the executor (it writes what the executor yields and warns if the state is not
// what cancellation asked for).
//
// It exists so the protocol surface can be verified end to end before there is a
// task store or an agent loop to run. It touches nothing: no filesystem, no tools,
// no model.
type echoExecutor struct{}

var _ a2asrv.AgentExecutor = echoExecutor{}

func (echoExecutor) Execute(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		// A NEW task has to be announced by the executor: the server records what the
		// executor yields, so a status update alone would be an answer about a task
		// nobody created ("first event must be a Task or a message"). A continuation
		// already has one, which is what StoredTask says.
		if execCtx.StoredTask == nil {
			if !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
				return
			}
		}
		if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, nil), nil) {
			return
		}
		if !yield(a2a.NewArtifactEvent(execCtx, a2a.NewTextPart(messageText(execCtx.Message))), nil) {
			return
		}
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, nil), nil)
	}
}

// Cancel writes the state cancellation asks for. The SDK does not do it for the
// executor — it forwards what Cancel yields and warns if that is not CANCELED —
// which is why "the executor confirmed it" is the only honest source for this state.
func (echoExecutor) Cancel(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if execCtx.StoredTask == nil {
			return
		}
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}

// echoBody is the artifact text: the incoming message's text parts, in order.
func messageText(msg *a2a.Message) string {
	if msg == nil {
		return ""
	}
	var out string
	for _, part := range msg.Parts {
		out += part.Text()
	}
	return out
}
