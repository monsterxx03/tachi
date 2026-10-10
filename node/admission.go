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

// Admission: the questions this node answers before any work starts, in the order the
// design fixes (design §7.1.1, §7.4):
//
//  1. Is this a REPLAY of a request already accepted? Then the answer is the task that
//     was accepted, and nothing runs a second time.
//  2. Does this message NAME A TASK? Then it is either the answer to a question that task
//     is waiting on, or an attempt to send work to a task that cannot take it — and both
//     answers are this node's own.
//  3. Is this NEW work for a context that already has a task in flight, or one PARKED on a
//     question? Then it is refused, and the refusal says so in a way a client can trust
//     (our own error domain behind a standard status).
//
// They live in two different places for one reason: the replay answer is a RESULT, which
// the transport renders (returning it from an interceptor gives the caller exactly what
// the handler would have produced), while the refusals are ERRORS that must NOT be
// rendered by the SDK — its error mapping would file our private reasons under the
// standard domain as INTERNAL_ERROR, which is the "private convention dressed as a
// standard capability" mistake in its purest form. So the checks stand in front of the
// protocol handler and write their own response.

// Sentinels of this node's own admission rules. They are deliberately not the SDK's:
// nothing in the spec maps to "this context already has work in flight", and nothing in it
// maps to "your answer names the wrong question" either.
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
	// reasonContextBusy is the reason a busy refusal carries: this context has work in
	// flight, and NEW work for it is what is refused.
	reasonContextBusy = "CONTEXT_BUSY"
	// reasonMessageIDReused is the reason a same-id-different-content refusal carries.
	reasonMessageIDReused = "MESSAGE_ID_REUSED"
	// reasonContextParked is the reason a refusal carries when the context's task is
	// waiting for an answer: the message has to name that task, because that is what
	// answering it means.
	reasonContextParked = "CONTEXT_PARKED"
	// reasonTaskBusy is the reason a message naming a RUNNING task is refused with.
	reasonTaskBusy = "TASK_BUSY"
	// reasonTaskFinished is the reason a message naming a finished task is refused with.
	reasonTaskFinished = "TASK_FINISHED"
	// reasonUnknownTask is the reason a message naming a task this node does not hold is
	// refused with.
	reasonUnknownTask = "UNKNOWN_TASK"
	// reasonNoWaitingQuestion is the reason a message naming a task that says it is waiting
	// is refused with: this process is not holding that question, so there is nothing for an
	// answer to reach.
	reasonNoWaitingQuestion = "NO_WAITING_QUESTION"
	// reasonQuestionMismatch is the reason an answer that names a different question — or no
	// question at all — is refused with. The current question's id travels with the refusal,
	// so the caller can answer it without guessing.
	reasonQuestionMismatch = "QUESTION_MISMATCH"
	// reasonAnswerShape is the reason an answer that cannot be mapped onto the waiting
	// questions is refused with (§7.4: the shape of an answer is this node's own rule).
	reasonAnswerShape = "ANSWER_SHAPE"
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
	if sm.Message.TaskID != "" {
		// A message that NAMES a task continues it: it creates nothing, so it is not a
		// replay of an accepted request and the creation key does not apply to it. (Its id
		// could even collide with a creation id by accident, and answering the caller with
		// that other task would be worse than useless.)
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

// questionSource is the one thing the guard needs to know about the turns this node is
// holding: which question a parked task is waiting on. The runner is behind it; a node
// running the placeholder executor has no source, and then no task is ever waiting.
type questionSource interface {
	waitingQuestion(taskID a2a.TaskID) (*parkedAsk, bool)
}

// busyGuard answers the refusals this node makes on its own terms, in front of the protocol
// handler:
//
//   - the same message id with DIFFERENT content (a private rule: the spec allows
//     idempotency, it does not require it — design §9.2);
//   - NEW work for a context that already has a task in flight, or parked on a question
//     (design §7.1.1);
//   - a message that names a task which cannot take it: one that is running, one that is
//     finished, one this node does not hold, and — the answer path — one whose question is
//     not the question this message answers (design §7.4).
//
// All of them must be rendered HERE rather than by the protocol handler. The SDK maps an
// unrecognised error to 500/INTERNAL_ERROR filed under `a2a-protocol.org`, and a reason the
// spec never defines must not be presented as one it does. A replay (same id, same content)
// is not a refusal: it passes through to the admission interceptor, which answers it with
// the task that was already accepted.
func busyGuard(store *taskStore, questions questionSource, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		msg, ok := readSendMessage(r)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}

		if msg.message != nil && msg.message.TaskID != "" {
			if ref := namedTaskRefusal(store, questions, msg); ref != nil {
				ref.write(w)
				return
			}
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

		if msg.ContextID != "" {
			// A context whose task is PARKED is not busy — the next message is the answer it
			// is waiting for. But an answer has to name the task it answers: without a task
			// id this message would create a SECOND task in that context, and two turns in
			// one conversation is what §7.1.1 refuses everywhere else.
			if parked, ok := store.parkedTaskIn(msg.ContextID); ok {
				ref := refusal{
					status:     http.StatusConflict,
					statusText: "FAILED_PRECONDITION",
					reason:     reasonContextParked,
					message: "this context is waiting for an answer; name that task in the message " +
						"so the answer reaches the question it answers",
					metadata: map[string]string{"contextId": msg.ContextID, "taskId": string(parked.Task.ID)},
				}
				if q := QuestionID(parked.Task.Status.Message); q != "" {
					ref.metadata[QuestionIDMetadataKey] = q
				}
				ref.write(w)
				return
			}
			if store.contextIsBusy(msg.ContextID) {
				writeRefusal(w, http.StatusServiceUnavailable, "UNAVAILABLE", reasonContextBusy,
					"this context already has a task in flight; query it, wait for it, or use another context",
					map[string]string{"contextId": msg.ContextID})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// namedTaskRefusal decides what a message that names a task is, and returns the refusal when
// it is not an answer this node can deliver.
//
// The answer path is the only one that passes, and it passes only when every part of the
// question-answer pairing lines up: the task is parked, this process is holding the question
// it is parked on, the message names THAT question, and its shape maps onto the questions
// that were asked. Anything less is refused with the reason — and, where there is one, with
// the id of the question that IS open, so a caller can answer it without guessing (§7.4:
// expired or mismatched answers are refused, not handed to another waiting slot).
func namedTaskRefusal(store *taskStore, questions questionSource, msg guardedMessage) *refusal {
	rec, known := store.record(msg.message.TaskID)
	if !known {
		return &refusal{
			status:     http.StatusNotFound,
			statusText: "NOT_FOUND",
			reason:     reasonUnknownTask,
			message:    "this node holds no task with that id",
			metadata:   map[string]string{"taskId": string(msg.message.TaskID)},
		}
	}

	state := rec.Task.Status.State
	if state != a2a.TaskStateInputRequired {
		if state.Terminal() {
			return &refusal{
				status:     http.StatusConflict,
				statusText: "FAILED_PRECONDITION",
				reason:     reasonTaskFinished,
				message: "this task is finished and does not reopen; to continue the conversation, " +
					"send a new message that references it",
				metadata: map[string]string{"taskId": string(rec.Task.ID), "state": string(state)},
			}
		}
		return &refusal{
			status:     http.StatusConflict,
			statusText: "ABORTED",
			reason:     reasonTaskBusy,
			message:    "this task is still running; wait for it or cancel it",
			metadata:   map[string]string{"taskId": string(rec.Task.ID), "state": string(state)},
		}
	}

	if questions == nil {
		return noWaitingQuestionRefusal(rec)
	}
	ask, waiting := questions.waitingQuestion(rec.Task.ID)
	if !waiting {
		// The record says a question is open and this process is not holding it — the state a
		// restart leaves behind, and one that must never be answered as if it were live.
		return noWaitingQuestionRefusal(rec)
	}
	if got := QuestionID(msg.message); got != ask.questionID {
		return &refusal{
			status:     http.StatusConflict,
			statusText: "FAILED_PRECONDITION",
			reason:     reasonQuestionMismatch,
			message: "this task is waiting for another question; the answer must name the question " +
				"it answers (message metadata " + QuestionIDMetadataKey + ")",
			metadata: map[string]string{
				"taskId":              string(rec.Task.ID),
				QuestionIDMetadataKey: ask.questionID,
			},
		}
	}
	if _, err := answersFor(ask.questions, msg.message); err != nil {
		return &refusal{
			status:     http.StatusBadRequest,
			statusText: "INVALID_ARGUMENT",
			reason:     reasonAnswerShape,
			message:    err.Error(),
			metadata:   map[string]string{"taskId": string(rec.Task.ID)},
		}
	}
	return nil
}

// noWaitingQuestionRefusal is the refusal a task that says it is waiting gets when this
// process is not holding the question: nothing could deliver an answer to it. The question's
// id is deliberately NOT sent back — the answer would be addressed to a question no loop is
// waiting to receive.
func noWaitingQuestionRefusal(rec *taskRecord) *refusal {
	return &refusal{
		status:     http.StatusConflict,
		statusText: "FAILED_PRECONDITION",
		reason:     reasonNoWaitingQuestion,
		message: "this task is waiting for an answer, but no turn in this process is holding " +
			"that question any more",
		metadata: map[string]string{"taskId": string(rec.Task.ID)},
	}
}

// guardedMessage is what the guard reads out of one message/send body.
type guardedMessage struct {
	MessageID string
	ContextID string
	digest    string
	// message is the decoded message itself, kept for the checks that need more than an id:
	// the task it names, the question it answers, and the parts its answer is made of.
	// Decoding it here costs nothing — the digest already required the whole body.
	message *a2a.Message
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
	g.message = envelope.Message
	return g, true
}

// restSendMessagePath is the spec's REST path for message/send (§11.3.1), spelled here
// because the guard reads it before the binding does.
const restSendMessagePath = "/message:send"

// refusal is one of this node's own answers to a request it will not accept.
//
// It carries the standard status a client can act on AND the private reason that says what
// this node means by it (design §9.1.1): a caller that trusts the source can tell "not
// accepted, retry the same message" (CONTEXT_BUSY) from "this task is over" (TASK_FINISHED)
// without either reason pretending to be part of the protocol.
type refusal struct {
	status     int
	statusText string
	reason     string
	message    string
	metadata   map[string]string
}

func (x refusal) write(w http.ResponseWriter) {
	writeRefusal(w, x.status, x.statusText, x.reason, x.message, x.metadata)
}

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
