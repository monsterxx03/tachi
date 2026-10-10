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
// Caller authentication does not exist yet — it arrives with the pairing and TLS
// slice, and until then this surface binds LOOPBACK ONLY (Options.normalized
// enforces it) and the Card declares no security scheme. That is not an omission
// in the Card: it is the truth about this surface today, and the design's rule is
// that a Card declares what is actually implemented.
type a2aSurface struct {
	ln   net.Listener
	srv  *http.Server
	card *a2a.AgentCard
	// state is the executor's half that lives OUTSIDE a request, when it has one: a question
	// waiting for an answer outlives the call that published it, and Close is what ends it
	// (see nodeState).
	state nodeState
}

func startA2ASurface(o Options, store *taskStore) (*a2aSurface, error) {
	ln, err := net.Listen("tcp", o.RemoteListen)
	if err != nil {
		return nil, fmt.Errorf("node: listen %s: %w", o.RemoteListen, err)
	}

	executor := o.Executor
	real := executor != nil
	if executor == nil {
		executor = echoExecutor{}
	}
	card := newAgentCard(ln.Addr().String(), real)
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
	handler := a2asrv.NewHandler(executor,
		a2asrv.WithCapabilityChecks(&card.Capabilities),
		// Our store is the authority for accepted tasks; the SDK's own default would be
		// an in-memory map that forgets them at exit (design §9.1).
		a2asrv.WithTaskStore(store),
		a2asrv.WithCallInterceptors(protocolVersionInterceptor{}, &admission{store: store}),
	)

	mux := http.NewServeMux()
	// The REST binding's routes are the spec's own paths (/message:send,
	// /tasks/{id}, /tasks/{id}:cancel, ...) with no prefix, so it mounts at the root
	// of this listener — which is why the remote surface gets a mux of its own.
	//
	// The guard wraps the binding rather than living inside it: a refusal that
	// carries OUR reason and domain must not be rendered by the SDK, whose error
	// mapping would file it as INTERNAL_ERROR under the standard domain.
	mux.Handle("/", busyGuard(store, questions, a2asrv.NewRESTHandler(handler)))
	mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: remoteReadHeaderTimeout}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			o.Logger.Error(context.Background(), "node: remote interface stopped", err, "addr", ln.Addr().String())
		}
	}()

	o.Logger.Info(context.Background(), "node: remote interface listening",
		"addr", ln.Addr().String(), "card", a2asrv.WellKnownAgentCardPath)

	return &a2aSurface{ln: ln, srv: srv, card: card, state: state}, nil
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

// newAgentCard describes what this node actually does — no more.
//
// Every optional capability stays false (nothing streams, nothing pushes) because that is
// the truth, and declaring one would be worse here than in a document: a PEER plans around a
// Card. Which skill it declares depends on the executor behind it, because those are two
// different nodes to a peer: the real one runs a Tachi turn, the placeholder echoes text
// (tests, and the protocol contract they pin). Claiming the real one from a placeholder would
// be exactly the "declared but not implemented" mistake this function exists to avoid.
func newAgentCard(addr string, real bool) *a2a.AgentCard {
	skill := a2a.AgentSkill{
		ID:          "task",
		Name:        "Remote task",
		Description: "Runs one turn of a Tachi agent on this node, in the workspace the operator configured and with the tools they allowed. The artifact is the turn's answer. A turn may stop to ask a question; the task then reads INPUT_REQUIRED and the message that answers it continues the same turn.",
		Tags:        []string{"agent"},
		Examples:    []string{"检查昨天的备份是否完整，返回异常清单", "这个目录里最大的十个文件是什么"},
	}
	if !real {
		skill = a2a.AgentSkill{
			ID:          "echo",
			Name:        "Echo",
			Description: "Returns the text it is given as an artifact. The protocol skeleton's placeholder, for a node that has not been wired to an agent.",
			Tags:        []string{"placeholder"},
			Examples:    []string{"say something"},
		}
	}
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
		Skills:             []a2a.AgentSkill{skill},
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
