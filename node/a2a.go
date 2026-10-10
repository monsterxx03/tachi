package node

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net"
	"net/http"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
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

	// shutdownGrace is how long the two listeners get to finish what they are doing
	// before Close returns.
	shutdownGrace = 3 * time.Second
)

// a2aSurface is the node's REMOTE face: what another node talks to.
//
// It is a separate listener with its own mux, deliberately not the local control
// interface (design §3.3): a remote credential must never reach the local
// management surface, and a local client must not depend on the remote one.
//
// Caller authentication does not exist yet — it arrives with the pairing and TLS
// slice, and until then this surface binds LOOPBACK ONLY (Options.normalized
// enforces it) and the Card declares no security scheme. That is not an omission
// in the Card: it is the truth about this surface today, and the design's rule is
// that a Card declares what is actually implemented.
type a2aSurface struct {
	ln   net.Listener
	srv  *http.Server
	card *a2a.AgentCard
}

func startA2ASurface(o Options) (*a2aSurface, error) {
	ln, err := net.Listen("tcp", o.RemoteListen)
	if err != nil {
		return nil, fmt.Errorf("node: listen %s: %w", o.RemoteListen, err)
	}

	card := newAgentCard(ln.Addr().String())

	executor := o.Executor
	if executor == nil {
		executor = echoExecutor{}
	}
	// One source for two jobs: the capabilities the handler ENFORCES are the ones
	// the Card publishes, so "declared but not implemented" cannot happen by drift.
	handler := a2asrv.NewHandler(executor,
		a2asrv.WithCapabilityChecks(&card.Capabilities),
		a2asrv.WithCallInterceptors(protocolVersionInterceptor{}),
	)

	mux := http.NewServeMux()
	// The REST binding's routes are the spec's own paths (/message:send,
	// /tasks/{id}, /tasks/{id}:cancel, ...) with no prefix, so it mounts at the root
	// of this listener — which is why the remote surface gets a mux of its own.
	mux.Handle("/", a2asrv.NewRESTHandler(handler))
	mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: remoteReadHeaderTimeout}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			o.Logger.Error(context.Background(), "node: remote interface stopped", err, "addr", ln.Addr().String())
		}
	}()

	o.Logger.Info(context.Background(), "node: remote interface listening",
		"addr", ln.Addr().String(), "card", a2asrv.WellKnownAgentCardPath)

	return &a2aSurface{ln: ln, srv: srv, card: card}, nil
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

func (s *a2aSurface) close() error {
	if s == nil || s.srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	return s.srv.Shutdown(ctx)
}

// newAgentCard describes what this node actually does — no more.
//
// Every optional capability stays false and only one skill is declared, because
// that is the truth: the executor behind them is the protocol skeleton's
// placeholder. Declaring streaming, push notifications or skills that do not exist
// would be the "claim a capability you do not implement" mistake the design warns
// about, and it is worse here than in a document — a PEER plans around a Card.
// This list grows in the same slice that implements the real executor.
func newAgentCard(addr string) *a2a.AgentCard {
	return &a2a.AgentCard{
		Name:        "Tachi",
		Description: "A Tachi node: an agent that runs on the machine that owns it, reachable from the other Tachi nodes you have paired with.",
		Version:     AgentVersion,
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface("http://"+addr, a2a.TransportProtocolHTTPJSON),
		},
		Capabilities:       a2a.AgentCapabilities{},
		DefaultInputModes:  []string{mediaTypeText},
		DefaultOutputModes: []string{mediaTypeText},
		Skills: []a2a.AgentSkill{{
			ID:          "echo",
			Name:        "Echo",
			Description: "Returns the text it is given as an artifact. The protocol skeleton's placeholder, replaced by the node's real executor.",
			Tags:        []string{"placeholder"},
			Examples:    []string{"say something"},
		}},
	}
}

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
		if !yield(a2a.NewArtifactEvent(execCtx, a2a.NewTextPart(echoBody(execCtx.Message))), nil) {
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
func echoBody(msg *a2a.Message) string {
	if msg == nil {
		return ""
	}
	var out string
	for _, part := range msg.Parts {
		out += part.Text()
	}
	return out
}
