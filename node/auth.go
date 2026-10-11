package node

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

// Who called, and how that fact reaches the code that files the work.
//
// The IDENTITY is not a credential to check — it is established by the TLS handshake and cannot be
// claimed: a caller presents its own certificate, and `NodeIDFor` derives the node id from the
// public key inside it, so nobody can present a key that derives somebody else's id. There is no
// list of accepted callers: whether a machine MAY call is the network's business (Options
// checkRemoteListen), and this node's job is to know WHO did.
//
// The NAME is a label and is treated as one. `node.name` travels with every call (see
// nodeNameHeader) so a record can read like a sentence, and it is filed under
// `self_reported_name` because anyone with a valid key can claim any name — the field's own name
// says so, so a later reader cannot mistake it for identity. The design's rule is the same one
// (§2.2: 来源名称从可信目录解析，未知名称保留明确的类型标注), just with the peer's own word kept
// in the one place where it is obviously a word.
//
// Two small pieces carry both values to where the rest of the node reads them:
//
//   - a middleware moves them from the connection and the request headers into the request
//     context. It decides nothing: the handshake already happened.
//   - a CallInterceptor writes the identity into CallContext.User. This half cannot be done in the
//     middleware: the REST transport builds a FRESH CallContext per request
//     (a2asrv.NewCallContext shadows whatever was attached), so a User set outside the request
//     handler is discarded before any handler sees it.
//
// Downstream is unchanged and does not care how the identity was established: callerFrom reads
// CallContext.User, and the task record, the dedupe key's caller half, List's filter, Get's
// ownership rule and the context-ownership check all key on that one value.

// nodeNameHeader is the private header a caller uses to declare its own name.
//
// A header rather than an A2A field on purpose: this is out-of-band metadata about the CALLER, not
// part of the task, and the design's rule is that private data must not be dressed up as a
// standard capability (§6.1). A third party that does not send it is not a problem — the record
// simply says "a node id and no name", which is the truth.
const nodeNameHeader = "X-Tachi-Node-Name"

// verifiedCallerKey carries what the handshake and the headers established, from the middleware to
// the interceptor. It is process-internal: nothing outside this file can put a caller in it, which
// is what makes "the identity came from a certificate" true rather than a claim.
type verifiedCallerKey struct{}

// verifiedCaller is a caller as this node knows it: an identity it cannot be fooled about, and a
// name the caller chose for itself.
type verifiedCaller struct {
	nodeID string
	// selfReportedName is exactly that. It is never compared to anything and never decides
	// anything — see the file comment for why it is kept anyway.
	selfReportedName string
}

func withVerifiedCaller(ctx context.Context, c verifiedCaller) context.Context {
	return context.WithValue(ctx, verifiedCallerKey{}, c)
}

func verifiedCallerFrom(ctx context.Context) (verifiedCaller, bool) {
	c, ok := ctx.Value(verifiedCallerKey{}).(verifiedCaller)
	return c, ok && c.nodeID != ""
}

// selfReportedNameFrom is the name the caller declared, as filed on the record. It is empty when
// the caller sent none (a third-party client, or a caller configured with no name).
func selfReportedNameFrom(ctx context.Context) string {
	c, ok := verifiedCallerFrom(ctx)
	if !ok {
		return ""
	}
	return c.selfReportedName
}

// callerIdentityMiddleware carries the identity from the TLS connection, and the declared name
// from the request headers, into the request context.
//
// It refuses a connection without a certificate, which on a correctly wired surface cannot happen
// (ClientAuth requires one) — the check exists so that a surface wired without TLS fails closed
// rather than treating every caller as anonymous.
func callerIdentityMiddleware(log *logger.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nodeID, err := callerNodeID(r)
		if err != nil {
			if log != nil {
				log.Warn(context.Background(), "node: refused a request whose connection carried no client certificate",
					"cause", err)
			}
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		name := strings.TrimSpace(r.Header.Get(nodeNameHeader))
		if len(name) > maxNodeNameLen {
			// A declared name is display data; an enormous one is a client that cannot be trusted
			// with the memory its record would cost. Truncated rather than refused: the identity is
			// unaffected, and losing a name is not a reason to lose the work. Cut by BYTES — that IS
			// the cost being bounded — and never mid-character, which is what leaves invalid UTF-8
			// on the record (json escapes the tail into a replacement character).
			name = strutil.TruncateBytes(name, maxNodeNameLen)
		}
		next.ServeHTTP(w, r.WithContext(withVerifiedCaller(r.Context(), verifiedCaller{
			nodeID:           nodeID,
			selfReportedName: name,
		})))
	})
}

// maxNodeNameLen bounds the name this node will store for a caller. It is a label; nothing
// legitimate needs more, and this one arrives from a machine this node does not control.
const maxNodeNameLen = 120

// callerNodeID reads the node id out of the certificate this connection presented.
//
// It reads the connection's state — `http.Request.TLS` is populated by the server from the same
// handshake `verifyCallerCertificate` checked — rather than anything the request carries. A
// request cannot name its own identity: there is no header, field or metadata the model (or a
// caller) could set that reaches this value.
func callerNodeID(r *http.Request) (string, error) {
	if r == nil || r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return "", errors.New("no client certificate on this connection")
	}
	pub, ok := r.TLS.PeerCertificates[0].PublicKey.(ed25519.PublicKey)
	if !ok {
		return "", errors.New("the client certificate does not carry an ed25519 key")
	}
	return NodeIDFor(pub), nil
}

// callerIdentityInterceptor moves the verified identity into the CallContext the rest of the node
// reads: the task store's per-caller hiding, callerFrom (task records, the dedupe scope, context
// ownership) and every refusal that names a caller.
type callerIdentityInterceptor struct{}

var _ a2asrv.CallInterceptor = callerIdentityInterceptor{}

func (callerIdentityInterceptor) Before(ctx context.Context, callCtx *a2asrv.CallContext, _ *a2asrv.Request) (context.Context, any, error) {
	c, ok := verifiedCallerFrom(ctx)
	if !ok {
		// The middleware is the only thing that puts a caller in the context, so arriving here
		// without one means a path that bypassed it. Refused rather than treated as anonymous: an
		// "anonymous" identity is one that could then be handed other callers' work.
		return ctx, nil, a2a.ErrUnauthenticated
	}
	// The USER is the node id: the identity, not the label.
	callCtx.User = a2asrv.NewAuthenticatedUser(c.nodeID, nil)
	return ctx, nil, nil
}

func (callerIdentityInterceptor) After(context.Context, *a2asrv.CallContext, *a2asrv.Response) error {
	return nil
}
