package node

import (
	"context"
	"encoding/json"
	"iter"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/pkg/fileutil"
)

// The acceptance rules, tested where they are decided. What this file is really about is
// the promise that "accepted" means something: the record is on disk before any work
// runs, the same request gets the same task, a different request under the same id gets
// a refusal, and a process that died leaves its tasks marked as interrupted rather than
// running forever.

// countingExecutor completes like the echo executor but counts its executions, which is
// how "no second execution" stops being a claim about the response and becomes a count.
type countingExecutor struct {
	runs atomic.Int64
}

func (c *countingExecutor) Execute(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		c.runs.Add(1)
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

func (c *countingExecutor) Cancel(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(func(a2a.Event, error) bool) {}
}

func messageWithID(id, text string) *a2a.Message {
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	msg.ID = id
	return msg
}

func sendMessage(t *testing.T, h *Handle, id, text, contextID string) *a2a.Task {
	t.Helper()
	client := newTestClient(t, h)
	msg := messageWithID(id, text)
	msg.ContextID = contextID
	req := &a2a.SendMessageRequest{
		Message: msg,
		Config:  &a2a.SendMessageConfig{ReturnImmediately: true},
	}

	// The parked executor in these tests never completes, so the handle is what comes
	// back; the caller waits for the state it needs instead of for a result.
	result, err := client.SendMessage(context.Background(), req)
	require.NoError(t, err)
	task, ok := result.(*a2a.Task)
	require.True(t, ok, "expected a task handle, got %T", result)
	return task
}

func TestAcceptedTaskIsOnDiskBeforeTheWorkRuns(t *testing.T) {
	parked := newParkedExecutor()
	dir := t.TempDir()
	h := startWith(t, dir, func(o *Options) { o.Executor = parked })
	client := newTestClient(t, h)

	// Release the parked task before the test returns and wait for it to land: an execution
	// still in flight writes into the data dir, and those writes would race the temp-dir
	// cleanup (see TestAMessageNamingARunningTaskIsRefused).
	var taskID a2a.TaskID
	t.Cleanup(func() {
		close(parked.release)
		require.Eventually(t, func() bool {
			got, err := client.GetTask(context.Background(), &a2a.GetTaskRequest{ID: taskID})
			return err == nil && got.Status.State == a2a.TaskStateCompleted
		}, waitForWork, pollInterval, "the released task must reach its terminal state before teardown")
	})

	const messageID = "msg-persisted"
	result, err := client.SendMessage(context.Background(), &a2a.SendMessageRequest{
		Message: messageWithID(messageID, "persist me"),
		Config:  &a2a.SendMessageConfig{ReturnImmediately: true},
	})
	require.NoError(t, err)
	task := result.(*a2a.Task)
	taskID = task.ID

	<-parked.started // the task is really being worked on, not merely accepted

	// The executor having started and the SDK having WRITTEN that are two events; wait for the
	// second before reading the record, or the write lands during teardown (see
	// waitForParkedStatus).
	waitForParkedStatus(t, dir, task.ID, a2a.TaskStateWorking)

	// Read the files, not the store: the claim is about what would survive a crash.
	var rec taskRecord
	require.NoError(t, fileutil.ReadJSON(filepath.Join(dir, tasksDirName, string(task.ID)+".json"), &rec))
	require.Equal(t, task.ID, rec.Task.ID)
	require.Equal(t, messageID, rec.MessageID)
	require.NotEmpty(t, rec.Digest)

	var index indexDoc
	require.NoError(t, fileutil.ReadJSON(filepath.Join(dir, tasksDirName, indexFileName), &index))
	require.Equal(t, string(task.ID), index.Entries[messageID],
		"the replay key has to survive with the record")
}

func TestReplayReturnsTheSameTaskAndRunsOnce(t *testing.T) {
	counter := &countingExecutor{}
	h := startWith(t, t.TempDir(), func(o *Options) { o.Executor = counter })
	client := newTestClient(t, h)

	const messageID = "msg-replay"
	first := sendAndCompleteID(t, client, messageID, "do it once", "")
	second := sendAndCompleteID(t, client, messageID, "do it once", "")

	require.Equal(t, first.ID, second.ID, "a replay is the same task, not a new one")
	require.EqualValues(t, 1, counter.runs.Load(), "the work must not run twice")
}

func TestReusedMessageIDWithDifferentContentIsRefused(t *testing.T) {
	counter := &countingExecutor{}
	h := startWith(t, t.TempDir(), func(o *Options) { o.Executor = counter })
	client := newTestClient(t, h)

	const messageID = "msg-reused"
	sendAndCompleteID(t, client, messageID, "first content", "")

	// Raw, because this refusal has to be read as a CLIENT reads it: our own status,
	// reason and domain. A Go sentinel cannot cross the wire, and it must not have to —
	// that is exactly why this refusal is rendered by us instead of by the SDK's error
	// mapping (which would file our private reason under the protocol's domain).
	resp, raw := post(t, "http://"+h.RemoteAddr()+restSendMessagePath, "application/a2a+json",
		sendBody(messageID, "DIFFERENT content", ""), nil)

	require.Equal(t, http.StatusConflict, resp.StatusCode, "body=%s", raw)
	require.Contains(t, raw, reasonMessageIDReused)
	require.Contains(t, raw, errorDomain)
	require.NotContains(t, raw, a2a.ProtocolDomain,
		"a private reason must not borrow the protocol's domain")
	require.EqualValues(t, 1, counter.runs.Load(), "the refusal must not run the work again")
}

func TestRestartResolvesInFlightTasksAsInterrupted(t *testing.T) {
	parked := newParkedExecutor()
	dir := t.TempDir()
	h := startWith(t, dir, func(o *Options) { o.Executor = parked })

	task := sendMessage(t, h, "msg-interrupted", "long job", "")
	<-parked.started

	// The task has to be really RUNNING before this "crash" means anything — and the state is
	// the SDK's write, which may still be in flight (see waitForParkedStatus). Waiting for it
	// also keeps that write out of teardown.
	waitForParkedStatus(t, dir, task.ID, a2a.TaskStateWorking)

	// The process goes away with the task still running. Nothing closes it politely:
	// this is what a crash leaves behind.
	require.NoError(t, h.Close())

	reopened := start(t, dir)
	client := newTestClient(t, reopened)

	got, err := client.GetTask(context.Background(), &a2a.GetTaskRequest{ID: task.ID})
	require.NoError(t, err)
	require.Equal(t, a2a.TaskStateFailed, got.Status.State,
		"a task nobody can finish must not stay 'running' in the record")
	require.Equal(t, interruptReason, got.Status.Message.Parts[0].Text())
}

func TestIndexIsRebuiltFromTheRecords(t *testing.T) {
	dir := t.TempDir()
	h := start(t, dir)
	client := newTestClient(t, h)

	const messageID = "msg-rebuild"
	first := sendAndCompleteID(t, client, messageID, "once", "")
	require.NoError(t, h.Close())

	// The index is a cache: losing it may cost a scan, never a duplicate task.
	require.NoError(t, os.Remove(filepath.Join(dir, tasksDirName, indexFileName)))

	reopened := start(t, dir)
	replay := sendAndCompleteID(t, newTestClient(t, reopened), messageID, "once", "")
	require.Equal(t, first.ID, replay.ID)
}

func TestConcurrentUpdateIsRefused(t *testing.T) {
	store, err := openTaskStore(filepath.Join(t.TempDir(), tasksDirName))
	require.NoError(t, err)

	task := &a2a.Task{
		ID:        a2a.NewTaskID(),
		ContextID: a2a.NewContextID(),
		Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
	}
	version, err := store.Create(context.Background(), task)
	require.NoError(t, err)

	first := *task
	first.Status = a2a.TaskStatus{State: a2a.TaskStateWorking}
	_, err = store.Update(context.Background(), &taskstore.UpdateRequest{Task: &first, PrevVersion: version})
	require.NoError(t, err)

	// A second writer still holding the OLD version must be refused: that is what stops
	// a status update and an artifact update from overwriting each other.
	stale := *task
	stale.Status = a2a.TaskStatus{State: a2a.TaskStateCompleted}
	_, err = store.Update(context.Background(), &taskstore.UpdateRequest{Task: &stale, PrevVersion: version})
	require.ErrorIs(t, err, taskstore.ErrConcurrentModification)
}

func TestBusyRefusesNewWorkButNotAReplay(t *testing.T) {
	parked := newParkedExecutor()
	h := startWith(t, t.TempDir(), func(o *Options) { o.Executor = parked })
	client := newTestClient(t, h)

	const (
		messageID = "msg-busy-1"
		contextID = "ctx-busy"
	)
	task := sendMessage(t, h, messageID, "long job", contextID)
	<-parked.started

	// Every task this test leaves parked is released and waited for before it returns:
	// a background execution still writing into the data dir when the test ends would
	// race the temp-dir cleanup (and, at -race, turn a teardown into a failure).
	var alsoAccepted a2a.TaskID
	t.Cleanup(func() {
		close(parked.release)
		for _, id := range []a2a.TaskID{task.ID, alsoAccepted} {
			if id == "" {
				continue
			}
			require.Eventually(t, func() bool {
				got, err := client.GetTask(context.Background(), &a2a.GetTaskRequest{ID: id})
				return err == nil && got.Status.State == a2a.TaskStateCompleted
			}, waitForWork, pollInterval)
		}
	})

	// New work in a busy context: refused, with OUR reason and domain.
	resp, raw := post(t, "http://"+h.RemoteAddr()+restSendMessagePath, "application/a2a+json",
		sendBody("msg-busy-2", "another job", contextID), nil)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, "body=%s", raw)
	require.Contains(t, raw, reasonContextBusy)
	require.Contains(t, raw, errorDomain)
	require.NotContains(t, raw, a2a.ProtocolDomain,
		"a private reason must not borrow the protocol's domain")

	// The FIRST request repeated: not new work, so not busy — the caller gets its task.
	resp, raw = post(t, "http://"+h.RemoteAddr()+restSendMessagePath, "application/a2a+json",
		sendBody(messageID, "long job", contextID), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body=%s", raw)
	var got struct {
		Task *a2a.Task `json:"task"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &got))
	require.Equal(t, task.ID, got.Task.ID, "a replay is answered with the task it already accepted")

	// A different context is not busy: the refusal is about the context, not the node.
	resp, raw = post(t, "http://"+h.RemoteAddr()+restSendMessagePath, "application/a2a+json",
		sendBody("msg-busy-3", "same node, other conversation", "ctx-other"), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body=%s", raw)
	var third struct {
		Task *a2a.Task `json:"task"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &third))
	require.NotNil(t, third.Task)
	alsoAccepted = third.Task.ID
}

// sendBody is the raw REST message/send body — used where the test must control the
// message id and context exactly, including on the busy path where the SDK client would
// not produce the request we need.
func sendBody(messageID, text, contextID string) string {
	msg := map[string]any{
		"messageId": messageID,
		"role":      "ROLE_USER",
		"parts":     []map[string]any{{"text": text}},
	}
	if contextID != "" {
		msg["contextId"] = contextID
	}
	// returnImmediately, because these bodies are sent by hand and the executor in the
	// busy test never completes: a blocking send would wait for a terminal state that
	// deliberately never arrives.
	raw, err := json.Marshal(map[string]any{
		"message":       msg,
		"configuration": map[string]any{"returnImmediately": true},
	})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// sendAndCompleteID sends a blocking message with a chosen id.
func sendAndCompleteID(t *testing.T, client *a2aclient.Client, id, text, contextID string) *a2a.Task {
	t.Helper()
	msg := messageWithID(id, text)
	msg.ContextID = contextID
	result, err := client.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: msg})
	require.NoError(t, err)
	task, ok := result.(*a2a.Task)
	require.True(t, ok)
	require.Equal(t, a2a.TaskStateCompleted, task.Status.State)
	return task
}

// The task id becomes a filename in the node's data dir, and it is the one identifier in
// this file that arrives from outside the package. A write that escapes the directory we
// own is not a failure worth being merely unlikely, so the rule is asserted rather than
// assumed.
func TestTaskIDMustBeOneFileName(t *testing.T) {
	store, err := openTaskStore(filepath.Join(t.TempDir(), tasksDirName))
	require.NoError(t, err)

	for _, id := range []a2a.TaskID{"../../escaped", "a/b", ".", "..", "", a2a.TaskID(strings.Repeat("x", maxRecordIDLen+1))} {
		task := &a2a.Task{ID: id, ContextID: a2a.NewContextID()}
		_, err := store.Create(context.Background(), task)
		require.Error(t, err, "task id %q must be refused", id)
	}

	// A UUID — what the handler actually generates — goes through.
	ok := &a2a.Task{ID: a2a.NewTaskID(), ContextID: a2a.NewContextID()}
	_, err = store.Create(context.Background(), ok)
	require.NoError(t, err)
}

// A parked task is not a busy context. This is the distinction the reply path depends on:
// a task in INPUT_REQUIRED exists precisely so the caller's next message can continue it,
// and treating that message as "new work in a busy context" would lock the caller out of
// the conversation the node just asked it to continue.
func TestAParkedTaskDoesNotMakeTheContextBusy(t *testing.T) {
	store, err := openTaskStore(filepath.Join(t.TempDir(), tasksDirName))
	require.NoError(t, err)

	const contextID = "ctx-parked"
	parked := &a2a.Task{
		ID:        a2a.NewTaskID(),
		ContextID: contextID,
		Status:    a2a.TaskStatus{State: a2a.TaskStateInputRequired},
	}
	_, err = store.Create(context.Background(), parked)
	require.NoError(t, err)
	require.False(t, store.contextIsBusy(contextID),
		"a task waiting for the caller's answer must not refuse that answer")

	// Executing, on the other hand, still refuses new work in the same context.
	version, err := store.Create(context.Background(), &a2a.Task{
		ID:        a2a.NewTaskID(),
		ContextID: "ctx-working",
		Status:    a2a.TaskStatus{State: a2a.TaskStateWorking},
	})
	require.NoError(t, err)
	require.NotZero(t, version)
	require.True(t, store.contextIsBusy("ctx-working"))
}
