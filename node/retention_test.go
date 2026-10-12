package node

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/session"
)

// The node's half of §9.4. What a remote conversation's session gets is the session store's OWN
// lifecycle — the same count-based cleanup, the same pins, the same explicit delete every other
// conversation gets, run by any process over this data dir. The one thing that overrides it is an
// execution RUNNING in that session, and that is now the only reason a hold exists.

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
		filepath.Join(base, "a2a", contextsDirName, contextID+".json"), &binding))
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

// A node's session is an ORDINARY session. This is the rule that replaced the retention window,
// and it is worth its own test because the promise INVERTED: it used to survive any sweep, and
// now it lives and dies by the session store's own lifecycle — count-based cleanup, pins, an
// explicit delete — exactly like a conversation the reader had.
//
// What is still protected is the session an execution is RUNNING in (the next test): that one
// cannot be taken by anything, because a turn is writing into it.
func TestARemoteSessionsSessionIsAnOrdinarySession(t *testing.T) {
	base := nodeHome(t)
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(oneMessageScript("备份检查完成")...)

	h := startRealNode(t, nodeConfig(t, mock, filepath.Join(base, "ws")))
	client := newTestClient(t, h)
	result, err := client.SendMessage(context.Background(), sendRequest("检查备份", false))
	require.NoError(t, err)
	task := result.(*a2a.Task)

	held := boundSession(t, base, task.ContextID)
	require.Empty(t, holdsFor(t, held),
		"a finished task leaves nothing holding the session: the node no longer keeps a conversation's session for a window of its own")

	// A sweep in ANOTHER process — one that knows nothing about this node, whose `current` is not
	// the node's and which has no memory of any task — takes it. `New` sweeps as it goes (see
	// session/retention.go), and with a cap this small the oldest session is pushed out by the
	// next one: that oldest session IS the node's, which is the whole point.
	desktop := otherManager(t)
	desktop.SetMaxKeep(1)
	for i := range 3 {
		_, err := desktop.New("mock", ".")
		require.NoError(t, err, "session #%d", i)
	}
	_, err = desktop.Load(held)
	require.Error(t, err, "a remote conversation's session is not exempt from the ordinary lifecycle")

	// And a follow-up in that conversation FAILS EXPLICITLY rather than being answered with a new
	// conversation that looks like the old one (design §7.1.2, §9.4). This half of the rule did
	// not move with the window: what changed is only WHICH cleanup can take the session away, not
	// what a caller hears when it is gone. The turn never starts, so nothing reaches the model —
	// asserted, because "the answer was refused" and "the answer was refused before any work
	// happened" are different promises — and the reason names the session, so the caller can tell
	// "your conversation was collected" from "the node broke".
	third, err := sendInContext(t, client, "继续，再说两句", task.ContextID)
	require.NoError(t, err, "the refusal is the task's outcome, not a protocol error")
	refused, ok := third.(*a2a.Task)
	require.True(t, ok)
	require.Equal(t, a2a.TaskStateFailed, refused.Status.State)
	require.Contains(t, messageText(refused.Status.Message), held)
	require.Len(t, mock.Requests(), 1, "only the first submission may reach the model")
}

// The other half of the same refusal, and the one a RESTART reaches: the node comes back with no
// cached run for the context, so the conversation is loaded from the binding — and that load is
// where a session collected in the meantime shows up. Session preparation failing must end the
// task, not start a loop in a session nobody owns (design §7.1: 会话准备或保护建立失败则任务明确
// 失败，不无记录地继续).
//
// It is a separate test from the one above because the two failures happen in different places
// (`Acquire` refusing against the filesystem vs `contextRun`'s load being the one that notices),
// and only this one exercises the cold path a recovered host takes.
func TestAFollowUpIntoACollectedConversationFailsOnTheColdPathToo(t *testing.T) {
	base := nodeHome(t)
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(oneMessageScript("备份检查完成")...)

	cfg := nodeConfig(t, mock, filepath.Join(base, "ws"))
	h := startRealNode(t, cfg)
	client := newTestClient(t, h)
	result, err := client.SendMessage(context.Background(), sendRequest("检查备份", false))
	require.NoError(t, err)
	task := result.(*a2a.Task)
	held := boundSession(t, base, task.ContextID)

	// The conversation's session is collected while no node is running — nothing holds it once the
	// task is over, so any process over this data dir may take it (§9.4).
	require.NoError(t, h.Close())
	desktop := otherManager(t)
	require.NoError(t, desktop.Delete(held), "a finished conversation holds nothing")

	// A fresh host: no cached run, so the binding is loaded, and the load is what fails.
	fresh := startRealNode(t, cfg)
	again := newTestClient(t, fresh)
	refused, err := sendInContext(t, again, "继续，再说两句", task.ContextID)
	require.NoError(t, err, "the refusal is the task's outcome, not a protocol error")
	failed, ok := refused.(*a2a.Task)
	require.True(t, ok)
	require.Equal(t, a2a.TaskStateFailed, failed.Status.State, "note=%q",
		messageText(failed.Status.Message))
	require.Contains(t, messageText(failed.Status.Message), "load session",
		"the failure has to BE the load: preparing the session is what must not start a loop")
	require.Len(t, mock.Requests(), 1, "preparing the session failed, so the loop never started")
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
	// so its session is held.
	held := boundSession(t, base, task.ContextID)
	holds := holdsFor(t, held)
	require.Len(t, holds, 1, "an execution running in a session is the one reason to hold it")
	require.Equal(t, session.ReasonTask, holds[0].Reason)
	require.Equal(t, string(task.ID), holds[0].Ref)

	// The task ends, and with it the only reason the session was held: nothing is left on the
	// record, so the ordinary lifecycle owns the session again.
	cancelParkedTask(t, client, task.ID)

	require.Eventually(t, func() bool { return len(holdsFor(t, held)) == 0 }, waitForWork, pollInterval)
}

// The owner's escape hatch (requirement: a task that will never end by itself must be stoppable
// from THIS machine, so the session it holds can be deleted).
//
// The shape that "never ends by itself" is a PARKED question: nothing but an answer or a stop can
// end it, and the answer is the caller's to give — so a peer that walked away, or a state a bug
// left behind, keeps the session held until the deadline. This ends it through the local control
// interface, which is how an entry point on this machine reaches the host.
func TestTheOwnerEndsASessionsWorkThroughTheLocalInterface(t *testing.T) {
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

	held := boundSession(t, base, task.ContextID)
	require.Len(t, holdsFor(t, held), 1, "a parked task is a running execution, and that is a reason")

	// The owner ends it. The handle's client is the local interface, the same one every other
	// entry point on this machine talks to.
	ended, err := h.Client().EndSessionWork(context.Background(), held)
	require.NoError(t, err)
	require.Equal(t, 1, ended, "one execution was running there")

	// CANCELED, not FAILED: the protocol's word for "a request stopped this", because a caller
	// reading the task later has to be able to tell a stop from a failure. The reason travels
	// with it — the machine's owner is not a caller, and the caller will never guess.
	got, err := client.GetTask(context.Background(), &a2a.GetTaskRequest{ID: task.ID})
	require.NoError(t, err)
	require.Equal(t, a2a.TaskStateCanceled, got.Status.State)
	require.Contains(t, messageText(got.Status.Message), "owner")

	// The hold goes with the execution's WORK, not merely with the order to stop, and the stop does
	// not return before that: the agent loop writes its own session files while it unwinds, so a
	// delete that overlapped those writes would fail on a directory somebody is still filling.
	// Asserted directly — no waiting — because an eventually here would be exactly the mask this
	// held the bug behind: it passed on a fast machine and failed on a loaded one.
	require.Empty(t, holdsFor(t, held), "the stop must not return while the turn still holds the session")
	desktop := otherManager(t)
	require.NoError(t, desktop.Delete(held),
		"with no execution running the owner's delete must go through")

	// Asking again is not an error: there is simply nothing left to end.
	ended, err = h.Client().EndSessionWork(context.Background(), held)
	require.NoError(t, err)
	require.Equal(t, 0, ended)
}

// What the hold covers, exactly: the turn's WRITES, which outlive "the turn ended".
//
// A turn ends as an EVENT (the executor's publish calls forgetTurn for a finish, a cancel, an
// expired question) while the agent loop still has its own session files to write — the tail of
// the stream, which is where the checkpoint bookkeeping and the final messages land. So a release
// at that moment would let a delete walk a directory somebody is still filling: RemoveAll fails on
// a non-empty directory, and the owner's delete reports that as a refusal for a session that is, in
// fact, over. That is the shape a loaded runner used to hit by luck; this test takes the luck out.
func TestEndingATurnDoesNotDropItsHoldBeforeTheTurnHasUnwound(t *testing.T) {
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
	held := boundSession(t, base, task.ContextID)
	require.Len(t, holdsFor(t, held), 1, "a parked task is a running execution, and that is a reason")

	runner := nodeRunner(t, h)
	turn, ok := runner.turn(task.ID)
	require.True(t, ok, "the parked task is a live turn")

	// The turn is forgotten — that is the event-side end every path goes through — and the session
	// is STILL held: the loop has not written its last files yet, and it is about to.
	runner.forgetTurn(task.ID)
	require.Len(t, holdsFor(t, held), 1,
		"the hold describes a turn writing into the session, and the loop is still writing")

	// Read the stream to its end — what the drain does — and the hold goes, no later than that.
	turn.stop()
	<-turn.unwound
	require.Empty(t, holdsFor(t, held),
		"the hold goes with the turn's work: dropped by then, and not before")
}

// A compaction with nothing running: the conversation MOVES (the binding follows it), and nothing
// else does. It is worth its own test because the successor is where the temptation to protect
// would reappear — and the rule the design landed on has to hold across compaction too: a remote
// conversation's session is an ordinary session, the predecessor's and the successor's alike.
func TestACompactionMovesTheConversationAndLeavesNoHoldBehind(t *testing.T) {
	base := nodeHome(t)
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(oneMessageScript("备份检查完成")...)

	h := startRealNode(t, nodeConfig(t, mock, filepath.Join(base, "ws")))
	client := newTestClient(t, h)
	result, err := client.SendMessage(context.Background(), sendRequest("检查备份", false))
	require.NoError(t, err)
	task := result.(*a2a.Task)
	before := boundSession(t, base, task.ContextID)
	require.Empty(t, holdsFor(t, before), "a finished conversation holds nothing to move")

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
	require.Empty(t, holdsFor(t, before))
	require.Empty(t, holdsFor(t, child.ID),
		"the successor is an ordinary session too: having this conversation in it protects nothing")

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
	require.Len(t, holdsFor(t, before), 1, "a parked task is a running execution, and that is a reason")

	// A turn that compacts while a task is WAITING: the hold moves to the child, so the session
	// the answer will resume into is protected before the answer arrives.
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
	require.Len(t, reasons, 1, "and it is the only reason there is")
	require.Empty(t, holdsFor(t, before))
}

func TestAStaleHoldIsReleasedWhenTheNodeComesBack(t *testing.T) {
	base := nodeHome(t)
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(oneMessageScript("检查完成")...)

	cfg := nodeConfig(t, mock, filepath.Join(base, "ws"))
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
	require.Len(t, holdsFor(t, held), 1)

	// The next start reconciles: the task is finished, so the hold it left names nothing —
	// and with no deadline on a hold, this owner is the only thing that could withdraw it.
	require.NotNil(t, startRealNode(t, cfg))

	require.Empty(t, holdsFor(t, held),
		"a hold for a task that is not running any more must be released")
}

// A hold whose ref this node cannot account for is released; one whose REASON it does not know
// is kept and reported. The second half matters more now: with a single reason in the code, a
// record carrying another one is a newer version's (or a bug's), and dropping it would release a
// session something else is holding.
//
// The retired `context` reason sits between those two, which is why it is named here separately:
// this version knows exactly what it meant (a conversation kept for its own retention window) and
// knows no such promise exists any more, so the record is brought forward rather than reported as
// a stranger — while a reason nobody has ever written stays put.
func TestAStaleHoldIsReleasedAndAnUnknownReasonIsKept(t *testing.T) {
	base := nodeHome(t)
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)

	mgr := otherManager(t)
	sess, err := mgr.New("mock", filepath.Join(base, "ws"))
	require.NoError(t, err)

	retention, err := session.NewRetention(sessionsDir(t), logger.New("node-test"))
	require.NoError(t, err)
	for _, h := range []session.Hold{
		// A task this node does not have: it finished, or it was resolved as interrupted before
		// this start, so nothing here is waiting for it.
		{SessionID: sess.ID, Owner: nodeHoldOwner, Reason: session.ReasonTask, Ref: "task-vanished"},
		// A reason an older version wrote and this one retired: released, because this node keeps
		// nothing for a conversation.
		{SessionID: sess.ID, Owner: nodeHoldOwner, Reason: session.ReasonContextRetired, Ref: "context-1"},
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
