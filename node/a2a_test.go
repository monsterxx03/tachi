package node

import (
	"context"
	"encoding/json"
	"io"
	"iter"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/monsterxx03/tachi/pkg/fileutil"
)

const (
	// waitForWork is how long a test waits for a task to reach its next state: the
	// round trip plus the executor's own scheduling, with room for a loaded runner.
	waitForWork = 5 * time.Second
	// pollInterval is how often such a wait re-checks.
	pollInterval = 20 * time.Millisecond
	// waitShort is the window in which something must NOT happen — a blocking send
	// returning early. Long enough to catch a synchronous return, short enough not to
	// slow the suite.
	waitShort = 300 * time.Millisecond
)

// The contract tests: a REAL SDK client against this node's real listener. A fake
// executor stands in for the agent, so what is under test is the protocol surface —
// the Card, discovery, the required operations and their error semantics.
//
// They resolve the Card over HTTP first, exactly as a peer will, so the well-known
// path and the address published in it are part of every one of them.

// startWith brings up a hosting node with Options tweaked by mutate.
func startWith(t *testing.T, dir string, mutate func(*Options)) *Handle {
	t.Helper()
	opts := testOptions(dir)
	if mutate != nil {
		mutate(&opts)
	}
	h, err := StartOrAttach(context.Background(), opts)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	require.True(t, h.Hosting())
	return h
}

func newTestClient(t *testing.T, h *Handle) *a2aclient.Client {
	t.Helper()
	card, err := agentcard.DefaultResolver.Resolve(context.Background(), "http://"+h.RemoteAddr())
	require.NoError(t, err)
	client, err := a2aclient.NewFromCard(context.Background(), card)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Destroy() })
	return client
}

func sendRequest(text string, returnImmediately bool) *a2a.SendMessageRequest {
	req := &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))}
	if returnImmediately {
		req.Config = &a2a.SendMessageConfig{ReturnImmediately: true}
	}
	return req
}

func get(t *testing.T, url string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	require.NoError(t, err)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, string(body)
}

func post(t *testing.T, url, contentType, body string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", contentType)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, string(raw)
}

func firstPartText(t *testing.T, a *a2a.Artifact) string {
	t.Helper()
	require.NotNil(t, a)
	require.NotEmpty(t, a.Parts)
	return a.Parts[0].Text()
}

func TestCardSaysOnlyWhatThisNodeActuallyDoes(t *testing.T) {
	h := start(t, t.TempDir())

	resp, body := get(t, "http://"+h.RemoteAddr()+a2asrv.WellKnownAgentCardPath, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var card a2a.AgentCard
	require.NoError(t, json.Unmarshal([]byte(body), &card))

	require.Len(t, card.SupportedInterfaces, 1)
	iface := card.SupportedInterfaces[0]
	require.Equal(t, "http://"+h.RemoteAddr(), iface.URL,
		"the Card must name the address that actually answers, not the one we asked for")
	require.Equal(t, a2a.TransportProtocolHTTPJSON, iface.ProtocolBinding)
	require.Equal(t, a2a.Version, iface.ProtocolVersion)
	require.Equal(t, []string{mediaTypeText}, card.DefaultInputModes)
	require.NotEmpty(t, card.Skills)

	// Nothing optional is implemented, so nothing optional may be claimed — and the
	// Card must not offer an authentication option that does not exist.
	require.False(t, card.Capabilities.Streaming)
	require.False(t, card.Capabilities.PushNotifications)
	require.Empty(t, card.SecuritySchemes)
}

// A node with an agent behind it and a node without one are two different nodes to a peer,
// and the Card is the only place a peer can tell them apart — so it must not still say
// "echo" once the real executor is wired, nor claim real work when it is not.
func TestARealNodeCardNamesTheRealWork(t *testing.T) {
	base := nodeHome(t)
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)

	h := startRealNode(t, nodeConfig(t, mock, filepath.Join(base, "ws"), "ReadFile"))
	require.Len(t, h.Card().Skills, 1)
	require.Equal(t, "task", h.Card().Skills[0].ID)
	require.NotContains(t, h.Card().Skills[0].Description, "placeholder")

	// A test-built node keeps its own honest declaration.
	placeholder := start(t, t.TempDir())
	require.Len(t, placeholder.Card().Skills, 1)
	require.Equal(t, "echo", placeholder.Card().Skills[0].ID)
}

func TestSendMessageWaitsForTheTaskAndReturnsItsArtifact(t *testing.T) {
	h := start(t, t.TempDir())
	client := newTestClient(t, h)

	// returnImmediately left at its default: the protocol's blocking mode, which must
	// not come back before the task reaches a terminal state.
	result, err := client.SendMessage(context.Background(), sendRequest("hello node", false))
	require.NoError(t, err)

	task, ok := result.(*a2a.Task)
	require.True(t, ok, "starting work returns a task, got %T", result)
	require.Equal(t, a2a.TaskStateCompleted, task.Status.State)
	require.NotEmpty(t, task.ContextID, "the receiving side generates the context with the first task")
	require.Len(t, task.Artifacts, 1)
	require.Equal(t, "hello node", firstPartText(t, task.Artifacts[0]))
}

func TestGetTaskAndListTasksSeeWhatWasAccepted(t *testing.T) {
	h := start(t, t.TempDir())
	client := newTestClient(t, h)

	accepted := sendAndComplete(t, client, "first")
	second := sendAndComplete(t, client, "second")

	got, err := client.GetTask(context.Background(), &a2a.GetTaskRequest{ID: accepted.ID})
	require.NoError(t, err)
	require.Equal(t, accepted.ID, got.ID)
	require.Equal(t, a2a.TaskStateCompleted, got.Status.State)

	// Listing is a DIFFERENT permission from reading one task by id, and it is the one
	// that must not work without a caller identity: a list is a view of everything,
	// so the SDK refuses it for an unauthenticated caller. That is the same rule the
	// design states as "读取任务、列举任务都再次检查归属" — and it means tasks/list only
	// becomes usable in the slice that authenticates callers.
	_, err = client.ListTasks(context.Background(), &a2a.ListTasksRequest{})
	require.ErrorIs(t, err, a2a.ErrUnauthenticated,
		"an unauthenticated caller must not be able to enumerate tasks")

	// Two tasks from two messages that named no context are two conversations.
	require.NotEqual(t, accepted.ContextID, second.ContextID)
}

func TestUnknownTaskIsNotFound(t *testing.T) {
	h := start(t, t.TempDir())
	client := newTestClient(t, h)

	_, err := client.GetTask(context.Background(), &a2a.GetTaskRequest{ID: a2a.NewTaskID()})
	require.ErrorIs(t, err, a2a.ErrTaskNotFound)
}

func TestMessageToATerminalTaskIsRejected(t *testing.T) {
	h := start(t, t.TempDir())
	client := newTestClient(t, h)

	done := sendAndComplete(t, client, "done")

	// Continuing a finished task is not a thing the protocol allows: the client must
	// start a new task in the same context instead (design §7.1.2).
	//
	// Raw, because the refusal has to be readable as a CLIENT reads it: the SDK's own
	// generic unsupported-operation would say nothing about why, and the reason a caller can
	// act on ("it is finished, reference it in a new message") is one this node makes up —
	// which is exactly why §9.1.1 keeps it out of the SDK's error mapping.
	resp, raw := post(t, "http://"+h.RemoteAddr()+restSendMessagePath, "application/a2a+json",
		taskMessageBody("msg-after-terminal", done.ID, done.ContextID, nil,
			map[string]any{"text": "more"}), nil)

	require.Equal(t, http.StatusConflict, resp.StatusCode, "body=%s", raw)
	require.Contains(t, raw, reasonTaskFinished)
	require.Contains(t, raw, errorDomain)
	require.NotContains(t, raw, a2a.ProtocolDomain,
		"a private reason must not borrow the protocol's domain")
}

func TestReturnImmediatelyHandsBackARunningTask(t *testing.T) {
	parked := newParkedExecutor()
	h := startWith(t, t.TempDir(), func(o *Options) { o.Executor = parked })
	client := newTestClient(t, h)

	result, err := client.SendMessage(context.Background(), sendRequest("hold", true))
	require.NoError(t, err)
	task, ok := result.(*a2a.Task)
	require.True(t, ok)
	require.False(t, task.Status.State.Terminal(), "returnImmediately means the caller gets a handle, not a result")

	<-parked.started // the task is really being worked on, not merely accepted

	got, err := client.GetTask(context.Background(), &a2a.GetTaskRequest{ID: task.ID})
	require.NoError(t, err)
	require.Equal(t, a2a.TaskStateWorking, got.Status.State)

	close(parked.release)
	require.Eventually(t, func() bool {
		t, err := client.GetTask(context.Background(), &a2a.GetTaskRequest{ID: task.ID})
		return err == nil && t.Status.State == a2a.TaskStateCompleted
	}, waitForWork, pollInterval, "the released task must reach its terminal state")
}

func TestBlockingSendDoesNotReturnBeforeTheTaskEnds(t *testing.T) {
	parked := newParkedExecutor()
	h := startWith(t, t.TempDir(), func(o *Options) { o.Executor = parked })
	client := newTestClient(t, h)

	done := make(chan a2a.SendMessageResult, 1)
	errs := make(chan error, 1)
	go func() {
		res, err := client.SendMessage(context.Background(), sendRequest("hold", false))
		done <- res
		errs <- err
	}()

	<-parked.started // it is running; the call must still be waiting
	select {
	case <-done:
		t.Fatal("a blocking send returned while the task was still running")
	case <-time.After(waitShort):
	}

	close(parked.release)
	select {
	case err := <-errs:
		require.NoError(t, err)
	case <-time.After(waitForWork):
		t.Fatal("the blocking send never returned after the task completed")
	}
	task, ok := (<-done).(*a2a.Task)
	require.True(t, ok)
	require.Equal(t, a2a.TaskStateCompleted, task.Status.State)
}

func TestCancelResolvesRunningTaskToCanceled(t *testing.T) {
	parked := newParkedExecutor()
	h := startWith(t, t.TempDir(), func(o *Options) { o.Executor = parked })
	client := newTestClient(t, h)

	result, err := client.SendMessage(context.Background(), sendRequest("long", true))
	require.NoError(t, err)
	task := result.(*a2a.Task)
	<-parked.started

	canceled, err := client.CancelTask(context.Background(), &a2a.CancelTaskRequest{ID: task.ID})
	require.NoError(t, err)
	require.Equal(t, a2a.TaskStateCanceled, canceled.Status.State,
		"the state must come from the executor confirming it, not from the request being sent")

	// Canceling again is idempotent, not an error (spec §3.3.1): the task is already
	// in the state that was asked for.
	again, err := client.CancelTask(context.Background(), &a2a.CancelTaskRequest{ID: task.ID})
	require.NoError(t, err)
	require.Equal(t, a2a.TaskStateCanceled, again.Status.State)
}

// The other half of the cancel rule: a task that ended on its own terms is the one
// that cannot be canceled. It uses the real (echo) executor because the state has to
// be reached by the task finishing, not by a parked test double.
func TestCompletedTaskCannotBeCanceled(t *testing.T) {
	h := start(t, t.TempDir())
	client := newTestClient(t, h)

	done := sendAndComplete(t, client, "finished")

	_, err := client.CancelTask(context.Background(), &a2a.CancelTaskRequest{ID: done.ID})
	require.ErrorIs(t, err, a2a.ErrTaskNotCancelable)
}

func TestUnsupportedProtocolVersionIsRefused(t *testing.T) {
	h := start(t, t.TempDir())

	body, err := json.Marshal(map[string]any{
		"message": map[string]any{
			"messageId": a2a.NewMessageID(),
			"role":      "ROLE_USER",
			"parts":     []map[string]any{{"text": "hello"}},
		},
	})
	require.NoError(t, err)

	resp, raw := post(t, "http://"+h.RemoteAddr()+"/message:send", "application/a2a+json",
		string(body), map[string]string{a2a.SvcParamVersion: "0.9"})

	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Contains(t, raw, "VERSION_NOT_SUPPORTED",
		"the answer must name the spec's reason, not just fail")
	require.Contains(t, raw, a2a.ProtocolDomain,
		"a standard reason belongs to the standard's domain")
}

func TestTheTwoSurfacesAreSeparateListeners(t *testing.T) {
	h := start(t, t.TempDir())

	// The local control route is not on the address peers call...
	resp, _ := get(t, "http://"+h.RemoteAddr()+localRouteHandshake, nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode,
		"the remote surface must not expose the local management interface")

	// ...and the protocol route is not on the local one.
	resp, _ = post(t, "http://"+h.Addr()+"/message:send", "application/a2a+json", "{}", nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode,
		"the local surface must not carry the protocol")
}

func waitForParkedStatus(t *testing.T, dir string, id a2a.TaskID, want a2a.TaskState) {
	t.Helper()
	path := filepath.Join(dir, tasksDirName, string(id)+".json")
	require.Eventually(t, func() bool {
		var rec taskRecord
		if err := fileutil.ReadJSON(path, &rec); err != nil {
			return false
		}
		return rec.Task.Status.State == want
	}, waitForWork, pollInterval,
		"the parked execution's status write has to land before the record is relied on")
}

// sendAndComplete sends a blocking message and returns the finished task.
func sendAndComplete(t *testing.T, client *a2aclient.Client, text string) *a2a.Task {
	t.Helper()
	result, err := client.SendMessage(context.Background(), sendRequest(text, false))
	require.NoError(t, err)
	task, ok := result.(*a2a.Task)
	require.True(t, ok)
	require.Equal(t, a2a.TaskStateCompleted, task.Status.State)
	return task
}

// parkedExecutor holds a task in WORKING until release is closed. It is how a test
// gets a task that is genuinely in flight — to cancel it, to check that a blocking
// send is still waiting, or to see a handle come back instead of a result.
type parkedExecutor struct {
	release  chan struct{} // the test lets the work finish
	canceled chan struct{} // a Cancel call told the executor to stop
	once     sync.Once
	started  chan struct{}
}

func newParkedExecutor() *parkedExecutor {
	return &parkedExecutor{
		release:  make(chan struct{}),
		canceled: make(chan struct{}),
		started:  make(chan struct{}, 1),
	}
}

// stop ends the running execution. The SDK waits for that (its cancel path sends the
// cancellation into the running execution's queue and then waits for the execution to
// finish), so an executor that ignored Cancel and kept running would hang the caller —
// which is exactly what a real executor must not do either.
func (p *parkedExecutor) stop() { p.once.Do(func() { close(p.canceled) }) }

var _ a2asrv.AgentExecutor = (*parkedExecutor)(nil)

func (p *parkedExecutor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if execCtx.StoredTask == nil {
			if !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
				return
			}
		}
		if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, nil), nil) {
			return
		}
		select {
		case p.started <- struct{}{}:
		default:
		}

		select {
		case <-p.release:
			if !yield(a2a.NewArtifactEvent(execCtx, a2a.NewTextPart("released")), nil) {
				return
			}
			yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, nil), nil)
		case <-p.canceled:
			// Stopping here is the executor's half of a cancel: the state is written by
			// Cancel, and returning is what lets the caller's CancelTask return.
			return
		case <-ctx.Done():
			return
		}
	}
}

func (p *parkedExecutor) Cancel(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	p.stop()
	return func(yield func(a2a.Event, error) bool) {
		if execCtx.StoredTask == nil {
			return
		}
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}

// The spec's own REST shape, sent by hand: the path and the media type are what a
// third-party implementation reads out of the specification, and they must work
// without the SDK's client on the other end.
//
// Two things this pins that are easy to lose:
//   - the route is /message:send with NO version prefix (the SDK's v0.3 compat
//     binding is the one that adds "/v1", and this node speaks v1.0);
//   - the request may carry the spec's Content-Type. The SDK itself uses
//     "application/json" on both sides, while the spec spells it
//     "application/a2a+json" (§11.4) — we accept the spec's spelling, which is the
//     direction that matters for interop.
func TestSpecRestShapeIsAcceptedByHand(t *testing.T) {
	h := start(t, t.TempDir())

	body := `{"message":{"messageId":"` + a2a.NewMessageID() +
		`","role":"ROLE_USER","parts":[{"text":"by hand"}]}}`

	resp, raw := post(t, "http://"+h.RemoteAddr()+"/message:send", "application/a2a+json", body, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body=%s", raw)

	var got struct {
		Task *a2a.Task `json:"task"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &got))
	require.NotNil(t, got.Task, "the response carries the task: %s", raw)
	require.Equal(t, a2a.TaskStateCompleted, got.Task.Status.State)
	require.Len(t, got.Task.Artifacts, 1)
	require.Equal(t, "by hand", firstPartText(t, got.Task.Artifacts[0]))
}
