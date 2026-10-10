package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// Admission: the two questions this node answers before any work starts, in the order
// the design fixes (design §7.1.1):
//
//  1. Is this a REPLAY of a request already accepted? Then the answer is the task that
//     was accepted, and nothing runs a second time.
//  2. Is this NEW work for a context that is already busy? Then it is refused, and the
//     refusal says so in a way a client can trust (503 + our own error domain).
//
// They live in two different places for one reason: the replay answer is a RESULT, which
// the transport renders (returning it from an interceptor gives the caller exactly what
// the handler would have produced), while the busy answer is an ERROR that must NOT be
// rendered by the SDK — its error mapping would file our private reason under the
// standard domain as INTERNAL_ERROR, which is the "private convention dressed as a
// standard capability" mistake in its purest form. So the busy check stands in front of
// the protocol handler and writes its own response.

// Sentinels of this node's own admission rules. They are deliberately not the SDK's:
// nothing in the spec maps to "this context already has work in flight".
var (
	// errMessageIDReused — the same messageId arrived with different content. Returning
	// the stored task would be answering a question nobody asked.
	//
	// The guard in front of the binding decides this case in our own terms; this sentinel
	// exists for the backstop in the admission interceptor, and for tests that call the
	// layer directly.
	errMessageIDReused = errors.New("node: message id reused with different content")
)

const (
	// errorDomain is Tachi's own ErrorInfo domain. It is NOT a2a-protocol.org: the
	// reasons filed under it are ours, and claiming the standard's domain would tell a
	// client that the protocol defines them.
	errorDomain = "tachi"
	// reasonContextBusy is the reason a busy refusal carries.
	reasonContextBusy = "CONTEXT_BUSY"
	// reasonMessageIDReused is the reason a same-id-different-content refusal carries.
	reasonMessageIDReused = "MESSAGE_ID_REUSED"
)

// fingerprintKey carries the admission fingerprint from the interceptor (which sees the
// message) down to the store's Create (which persists it). Carrying it on the context is
// what keeps "the task record" and "the replay key" in one atomic file write instead of
// two writes and a window between them.
type fingerprintKey struct{}

func withFingerprint(ctx context.Context, fp fingerprint) context.Context {
	return context.WithValue(ctx, fingerprintKey{}, fp)
}

func fingerprintFrom(ctx context.Context) (fingerprint, bool) {
	fp, ok := ctx.Value(fingerprintKey{}).(fingerprint)
	return fp, ok
}

// admission is the replay half: it answers an already-accepted request with its task.
type admission struct {
	store *taskStore
}

var _ a2asrv.CallInterceptor = (*admission)(nil)

func (a *admission) Before(ctx context.Context, callCtx *a2asrv.CallContext, req *a2asrv.Request) (context.Context, any, error) {
	if a == nil || a.store == nil || req == nil {
		return ctx, nil, nil
	}
	sm, ok := req.Payload.(*a2a.SendMessageRequest)
	if !ok || sm == nil || sm.Message == nil || sm.Message.ID == "" {
		// Everything else (queries, cancellation) has its own identity and needs no
		// fingerprint. A message without an id cannot be deduped, and the spec requires
		// one, so there is nothing to guess.
		return ctx, nil, nil
	}

	caller := ""
	if callCtx != nil && callCtx.User != nil {
		caller = callCtx.User.Name
	}
	fp := fingerprint{caller: caller, messageID: sm.Message.ID, digest: digestMessage(sm.Message)}
	ctx = withFingerprint(ctx, fp)

	rec, found := a.store.lookup(fp)
	if !found {
		return ctx, nil, nil
	}

	// The guard in front of the binding normally decides this case in our own terms; this
	// is the backstop for any path that reaches the handler without it.
	if !rec.matches(fp) {
		return ctx, nil, fmt.Errorf("%w: message %s was already accepted with different content",
			errMessageIDReused, fp.messageID)
	}
	return ctx, rec.Task, nil
}

func (a *admission) After(context.Context, *a2asrv.CallContext, *a2asrv.Response) error {
	return nil
}

// busyGuard answers the two refusals this node makes on its own terms, in front of the
// protocol handler:
//
//   - the same message id with DIFFERENT content (a private rule: the spec allows
//     idempotency, it does not require it — design §9.2);
//   - NEW work for a context that already has a task in flight (design §7.1.1).
//
// Both must be rendered HERE rather than by the protocol handler. The SDK maps an
// unrecognised error to 500/INTERNAL_ERROR filed under `a2a-protocol.org`, and a reason
// the spec never defines must not be presented as one it does. A replay (same id, same
// content) is not a refusal: it passes through to the admission interceptor, which
// answers it with the task that was already accepted.
func busyGuard(store *taskStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		msg, ok := readSendMessage(r)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}

		fp := fingerprint{messageID: msg.MessageID, digest: msg.digest}
		if rec, known := store.lookup(fp); known {
			if !rec.matches(fp) {
				writeRefusal(w, http.StatusConflict, "ALREADY_EXISTS", reasonMessageIDReused,
					"this message id was already accepted with different content",
					map[string]string{"messageId": msg.MessageID})
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		if msg.ContextID != "" && store.contextIsBusy(msg.ContextID) {
			writeRefusal(w, http.StatusServiceUnavailable, "UNAVAILABLE", reasonContextBusy,
				"this context already has a task in flight; query it, wait for it, or use another context",
				map[string]string{"contextId": msg.ContextID})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// guardedMessage is what the guard reads out of one message/send body.
type guardedMessage struct {
	MessageID string
	ContextID string
	digest    string
}

// readSendMessage buffers the body, puts it back untouched, and digests the message.
//
// The digest comes from the SAME function the admission interceptor uses to record it
// (digestMessage over the SDK's own message type), so "same content" has one definition
// rather than two that can drift.
//
// It reads the WHOLE body on purpose: a truncated one would reach the protocol handler as
// if the client had sent it that way, and a half-parsed request is worse than a slow one.
// A size cap belongs with the file-part policy (§6.4) — today the surface is loopback-only
// and any cap here would be guessing at limits the receiving side has not decided.
func readSendMessage(r *http.Request) (guardedMessage, bool) {
	var g guardedMessage
	if r.Method != http.MethodPost || r.URL.Path != restSendMessagePath || r.Body == nil {
		return g, false
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return g, false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	var envelope struct {
		Message *a2a.Message `json:"message"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Message == nil {
		// Not a message/send this guard can reason about: the binding reports it in the
		// protocol's own terms.
		return g, false
	}
	g.MessageID = envelope.Message.ID
	g.ContextID = envelope.Message.ContextID
	g.digest = digestMessage(envelope.Message)
	return g, true
}

// restSendMessagePath is the spec's REST path for message/send (§11.3.1), spelled here
// because the guard reads it before the binding does.
const restSendMessagePath = "/message:send"

// rpcStatus and friends render the google.rpc.Status shape the spec's HTTP binding uses
// for errors (§11.6), with our own ErrorInfo. Rendered by us rather than by the SDK
// because the reason and the domain ARE the answer: a client recognises our refusals by
// (trusted source + domain + reason), and only a domain of our own makes that safe.
type rpcErrorInfo struct {
	Type     string            `json:"@type"`
	Reason   string            `json:"reason"`
	Domain   string            `json:"domain"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type rpcError struct {
	Code    int            `json:"code"`
	Status  string         `json:"status"`
	Message string         `json:"message"`
	Details []rpcErrorInfo `json:"details"`
}

type rpcStatus struct {
	Error rpcError `json:"error"`
}

// writeRefusal writes one of this node's own refusals.
func writeRefusal(w http.ResponseWriter, status int, statusText, reason, message string, metadata map[string]string) {
	body := rpcStatus{}
	body.Error.Code = status
	body.Error.Status = statusText
	body.Error.Message = message
	body.Error.Details = []rpcErrorInfo{{
		Type:     "type.googleapis.com/google.rpc.ErrorInfo",
		Reason:   reason,
		Domain:   errorDomain,
		Metadata: metadata,
	}}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
