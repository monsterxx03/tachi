package node

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/session"
)

// The node's half of §9.4: a remote conversation's session must survive a count-based cleanup
// run by ANY process over this data dir — including one that has never heard of this node —
// and it must stop surviving when the conversation's own window ends.

// otherManager is the "somebody else" of these tests: a manager over the same data dir, built
// the way another frontend builds one, with a cap of its own.
func otherManager(t *testing.T) *session.Manager {
	t.Helper()
	sm, err := session.NewManager(logger.New("node-test"))
	require.NoError(t, err)
	return sm
}

// sessionsDir is where the node's sessions (and the retention record beside them) live.
func sessionsDir(t *testing.T) string {
	t.Helper()
	dir, err := config.SessionDir()
	require.NoError(t, err)
	return dir
}

func holdsFor(t *testing.T, sessionID string) []session.Hold {
	t.Helper()
	retention, err := session.NewRetention(sessionsDir(t), logger.New("node-test"))
	require.NoError(t, err)
	holds, err := retention.Held(sessionID)
	require.NoError(t, err)
	return holds
}

// boundSession reads the session a context is held in, from the binding record.
func boundSession(t *testing.T, base, contextID string) string {
	t.Helper()
	var binding contextBinding
	require.NoError(t, fileutil.ReadJSON(
		filepath.Join(base, "node", contextsDirName, contextID+".json"), &binding))
	return binding.SessionID
}

// nodeRunner reaches the runner behind a hosting handle: these tests are about the node's own
// bookkeeping, and reading it is the difference between asserting a behaviour and asserting a
// log line.
func nodeRunner(t *testing.T, h *Handle) *runner {
	t.Helper()
	require.NotNil(t, h.remote)
	ex, ok := h.remote.state.(*nodeExecutor)
	require.True(t, ok, "the real executor must be behind this handle")
	require.NotNil(t, ex.runner)
	return ex.runner
}

// sendInContext sends a message that continues an EXISTING conversation without naming a task:
// §7.1.2's "a follow-up question after the previous task finished".
func sendInContext(t *testing.T, client interface {
	SendMessage(context.Context, *a2a.SendMessageRequest) (a2a.SendMessageResult, error)
}, text, contextID string) (a2a.SendMessageResult, error) {
	t.Helper()
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	msg.ContextID = contextID
	return client.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: msg})
}

func TestARemoteSessionsSurvivesAnotherManagersCleanup(t *testing.T) {
	base := nodeHome(t)
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(oneMessageScript("备份检查完成")...)

	h := startRealNode(t, nodeConfig(t, mock, filepath.Join(base, "ws"), "ReadFile"))
	client := newTestClient(t, h)
	result, err := client.SendMessage(context.Background(), sendRequest("检查备份", false))
	require.NoError(t, err)
	task := result.(*a2a.Task)

	// The sweep below runs in a DIFFERENT manager, over the same data dir, knowing nothing
	// about this node: its `current` is not the node's, and it has no memory of a task. That is
	// the desktop-versus-node case the protection exists for.
	desktop := otherManager(t)
	desktop.SetMaxKeep(1)
	for i := range 3 {
		_, err := desktop.New("mock", ".")
		require.NoError(t, err, "session #%d", i)
	}

	// The remote conversation's session is the oldest of the lot, so it is a candidate.
	require.Equal(t, 0, desktop.CleanupOldSessions(),
		"a sweep must not delete the session a remote conversation is held in")

	// And the same manager cannot delete it explicitly either: the refusal names the
	// conversation, because "close that conversation first" is the way out.
	held := boundSession(t, base, task.ContextID)
	err = desktop.Delete(held)
	var refused *session.SessionHeldError
	require.ErrorAs(t, err, &refused)
	require.Contains(t, err.Error(), task.ContextID)
	require.Len(t, refused.Holds, 1, "a finished task leaves the conversation's hold, not the task's")
	require.Equal(t, session.ReasonContext, refused.Holds[0].Reason)
}

func TestARunningTaskHoldsItsSessionAndReleasesOnlyItsOwnHold(t *testing.T) {
	base := nodeHome(t)
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(mockllm.Step{Reply: mockllm.Stream(
		mockllm.ToolCallStart("ask_1", tools.ToolNameAskUser, askArgs(t, "检查照片还是文档？", "照片")),
		mockllm.Finish("tool_calls"),
		mockllm.Done(),
	)})

	h := startRealNode(t, nodeConfig(t, mock, filepath.Join(base, "ws")))
	client := newTestClient(t, h)
	result, err := client.SendMessage(context.Background(), sendRequest("检查备份", false))
	require.NoError(t, err)
	task := result.(*a2a.Task)
	require.Equal(t, a2a.TaskStateInputRequired, task.Status.State)

	// A parked task is still an execution this node holds — the question waits on a goroutine —
	// so both reasons to keep the session are on the record.
	held := boundSession(t, base, task.ContextID)
	reasons := map[string]bool{}
	for _, hold := range holdsFor(t, held) {
		reasons[hold.Reason] = true
	}
	require.True(t, reasons[session.ReasonTask], "a parked task still needs its session")
	require.True(t, reasons[session.ReasonContext])

	// The task ends. Its own hold goes with it; the conversation's stays, because the
	// conversation can still be continued (design §9.4).
	_, err = client.CancelTask(context.Background(), &a2a.CancelTaskRequest{ID: task.ID})
	require.NoError(t, err)

	require.Eventually(t, func() bool { return len(holdsFor(t, held)) == 1 }, waitForWork, pollInterval)
	left := holdsFor(t, held)
	require.Equal(t, session.ReasonContext, left[0].Reason,
		"only the task's hold may be withdrawn when the task ends")
}

func TestACompactionMovesTheConversationAndItsProtection(t *testing.T) {
	base := nodeHome(t)
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(oneMessageScript("备份检查完成")...)

	h := startRealNode(t, nodeConfig(t, mock, filepath.Join(base, "ws"), "ReadFile"))
	client := newTestClient(t, h)
	result, err := client.SendMessage(context.Background(), sendRequest("检查备份", false))
	require.NoError(t, err)
	task := result.(*a2a.Task)
	before := boundSession(t, base, task.ContextID)
	require.Len(t, holdsFor(t, before), 1)

	// Compaction switches the conversation to a successor WHILE a turn is running, and reaches
	// the node through the manager's hook — the interface agent/compact.go's FinalizeCompact
	// calls. Driving it here is driving the node's handler with exactly what that call site
	// passes (that the call site fires is pinned in agent/compact_test.go).
	runner := nodeRunner(t, h)
	cr := runner.runs[task.ContextID]
	require.NotNil(t, cr)
	child, err := cr.sm.New("mock", cr.binding.Workspace)
	require.NoError(t, err)
	cr.sm.NotifySessionSwitch(&session.Session{ID: before}, child)

	require.Equal(t, child.ID, boundSession(t, base, task.ContextID),
		"the binding has to follow the conversation")
	require.Empty(t, holdsFor(t, before), "the predecessor's hold moves with the conversation")
	after := holdsFor(t, child.ID)
	require.Len(t, after, 1)
	require.Equal(t, session.ReasonContext, after[0].Reason)

	// The conversation is still this context's, so nothing about it changed for the caller.
	got, err := client.GetTask(context.Background(), &a2a.GetTaskRequest{ID: task.ID})
	require.NoError(t, err)
	require.Equal(t, task.ContextID, got.ContextID)
}

func TestACompactionDoesNotOrphanARunningTasksHold(t *testing.T) {
	base := nodeHome(t)
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(mockllm.Step{Reply: mockllm.Stream(
		mockllm.ToolCallStart("ask_1", tools.ToolNameAskUser, askArgs(t, "检查照片还是文档？", "照片")),
		mockllm.Finish("tool_calls"),
		mockllm.Done(),
	)})

	h := startRealNode(t, nodeConfig(t, mock, filepath.Join(base, "ws")))
	client := newTestClient(t, h)
	result, err := client.SendMessage(context.Background(), sendRequest("检查备份", false))
	require.NoError(t, err)
	task := result.(*a2a.Task)
	before := boundSession(t, base, task.ContextID)
	require.Len(t, holdsFor(t, before), 2, "a parked task holds its session twice over")

	// A turn that compacts while a task is WAITING: the child inherits both reasons, so the
	// session the answer will resume into is protected before the answer arrives.
	runner := nodeRunner(t, h)
	cr := runner.runs[task.ContextID]
	child, err := cr.sm.New("mock", cr.binding.Workspace)
	require.NoError(t, err)
	cr.sm.NotifySessionSwitch(&session.Session{ID: before}, child)

	reasons := map[string]bool{}
	for _, hold := range holdsFor(t, child.ID) {
		reasons[hold.Reason] = true
	}
	require.True(t, reasons[session.ReasonTask], "the parked task's reason has to move with the conversation")
	require.True(t, reasons[session.ReasonContext])
	require.Empty(t, holdsFor(t, before))
}

func TestAnExpiredConversationIsRefusedAndLetsItsSessionGo(t *testing.T) {
	base := nodeHome(t)
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)

	cfg := nodeConfig(t, mock, filepath.Join(base, "ws"))
	cfg.Node.ContextRetention = 200 * time.Millisecond
	h := startRealNode(t, cfg)
	client := newTestClient(t, h)

	// Two turns are scripted because the window allows the follow-up below: the point is the
	// third message, after the window, not a model that ran out of answers.
	mock.Script(oneMessageScript("第一轮完成")...)
	mock.Script(oneMessageScript("第二轮完成")...)

	result, err := client.SendMessage(context.Background(), sendRequest("第一轮", false))
	require.NoError(t, err)
	first := result.(*a2a.Task)
	held := boundSession(t, base, first.ContextID)

	// The window is counted from the last USE, so a follow-up inside it is fine — a
	// conversation in daily use never expires.
	second, err := sendInContext(t, client, "第二轮", first.ContextID)
	require.NoError(t, err)
	require.Equal(t, a2a.TaskStateCompleted, second.(*a2a.Task).Status.State)

	time.Sleep(250 * time.Millisecond)

	// Past the window the continuation FAILS EXPLICITLY: the node must never answer it with a
	// new conversation that looks like the old one (design §9.4).
	third, err := sendInContext(t, client, "第三轮", first.ContextID)
	require.NoError(t, err, "the refusal is the task's outcome, not a protocol error")
	failed, ok := third.(*a2a.Task)
	require.True(t, ok)
	require.Equal(t, a2a.TaskStateFailed, failed.Status.State)
	require.Contains(t, messageText(failed.Status.Message), "retention window")

	// And the session is an ordinary one again: the hold that kept it is gone, so any sweep may
	// reclaim it.
	require.Eventually(t, func() bool { return len(holdsFor(t, held)) == 0 }, waitForWork, pollInterval)
	require.NoError(t, otherManager(t).Delete(held),
		"an expired conversation must not keep its session forever")
}

func TestAStaleHoldIsReleasedWhenTheNodeComesBack(t *testing.T) {
	base := nodeHome(t)
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(oneMessageScript("检查完成")...)

	cfg := nodeConfig(t, mock, filepath.Join(base, "ws"), "ReadFile")
	h := startRealNode(t, cfg)
	client := newTestClient(t, h)
	result, err := client.SendMessage(context.Background(), sendRequest("检查备份", false))
	require.NoError(t, err)
	task := result.(*a2a.Task)
	held := boundSession(t, base, task.ContextID)
	require.NoError(t, h.Close())

	// What a crash leaves behind: a hold for a task that is over. It is written by hand because
	// that IS the state a killed process leaves — nobody withdrew anything.
	retention, err := session.NewRetention(sessionsDir(t), logger.New("node-test"))
	require.NoError(t, err)
	require.NoError(t, retention.Acquire(session.Hold{
		SessionID: held, Owner: nodeHoldOwner, Reason: session.ReasonTask, Ref: string(task.ID),
	}))
	require.Len(t, holdsFor(t, held), 2)

	// The next start reconciles: the task is finished, so the hold it left names nothing.
	require.NotNil(t, startRealNode(t, cfg))

	left := holdsFor(t, held)
	require.Len(t, left, 1, "a hold for a task that is not running any more must be released")
	require.Equal(t, session.ReasonContext, left[0].Reason,
		"…and the conversation's own hold must survive the reconciliation")
}

func TestAHoldForAConversationTheNodeDoesNotKnowIsReleased(t *testing.T) {
	base := nodeHome(t)
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)

	mgr := otherManager(t)
	sess, err := mgr.New("mock", filepath.Join(base, "ws"))
	require.NoError(t, err)

	retention, err := session.NewRetention(sessionsDir(t), logger.New("node-test"))
	require.NoError(t, err)
	for _, h := range []session.Hold{
		// A conversation this node has no binding for: the reason to keep the session went with
		// whatever process wrote it.
		{SessionID: sess.ID, Owner: nodeHoldOwner, Reason: session.ReasonContext, Ref: "ctx-vanished"},
		// And a hold this version does not understand: kept, and reported, rather than dropped
		// (dropping it would release a session something newer is holding).
		{SessionID: sess.ID, Owner: nodeHoldOwner, Reason: "future-reason", Ref: "unknown-1"},
	} {
		require.NoError(t, retention.Acquire(h))
	}

	startRealNode(t, nodeConfig(t, mock, filepath.Join(base, "ws")))

	var reasons []string
	for _, h := range holdsFor(t, sess.ID) {
		reasons = append(reasons, h.Reason)
	}
	require.Equal(t, []string{"future-reason"}, reasons)
}
