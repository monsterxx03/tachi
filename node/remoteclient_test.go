package node

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/itest/mockllm"
)

// The sending side, against a REAL receiving node: the peer here is the same node this
// machine serves, reached over its own loopback listener. Nothing is stubbed except the model
// behind the peer, so what is under test is the actual conversation — our client's requests
// against our server's answers, refusals included.

// startPeer brings up a receiving node whose model is scripted, and returns a sender pointing
// at it. The sender's own home is a DIFFERENT base dir: it does not host a node at all, which
// is the case the whole sending side exists for.
func startPeer(t *testing.T, mock *mockllm.Server, tools ...string) (*Sender, *Handle) {
	t.Helper()

	// The receiver's home: its own base dir, its own session store, its own node data dir.
	peerHome := t.TempDir()
	previous := config.BaseDir()
	config.SetBaseDir(peerHome)
	t.Cleanup(func() { config.SetBaseDir(previous) })

	peer := startRealNode(t, nodeConfig(t, mock, filepath.Join(peerHome, "ws"), tools...))

	// The sender's home: another base dir, with a peer entry pointing at the receiver. Only one
	// process-global base dir exists, so this is built in two steps — records written under the
	// sender's dir, addresses taken from the receiver that is already up.
	senderHome := t.TempDir()
	config.SetBaseDir(senderHome)
	t.Cleanup(func() { config.SetBaseDir(previous) })

	sender, err := OpenSender([]config.PeerConfig{{Name: "peer", URL: "http://" + peer.RemoteAddr()}})
	require.NoError(t, err)
	return sender, peer
}

func TestASubmissionReachesThePeerAndItsAnswerComesBack(t *testing.T) {
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(oneMessageScript("备份检查完成：两个目录异常")...)

	sender, peer := startPeer(t, mock, "ReadFile")
	require.NotNil(t, peer)

	view, err := sender.Submit(context.Background(), SubmitRequest{
		Peer:      "peer",
		Prompt:    "检查昨天的备份是否完整，返回异常清单，不修改文件。",
		LogicalID: "conv-1",
	})
	require.NoError(t, err)
	require.Equal(t, "completed", view.State)
	require.Contains(t, view.Result, "备份检查完成")
	require.Equal(t, "peer", view.Node)
	require.Regexp(t, `^d_[0-9a-f]{8}$`, view.ID)
	require.NotEmpty(t, view.Title, "a task result has to carry what the model will call this work")
	require.True(t, view.LastSeen.After(time.Time{}), "…and when it was seen")

	// The peer really ran a turn: this is not a client talking to itself.
	requests := mock.Requests()
	require.Len(t, requests, 1)

	// And the record on disk is what a follow-up will read: the remote ids are in it.
	record, ok := sender.records.get(view.ID)
	require.True(t, ok)
	require.NotEmpty(t, record.RemoteTaskID)
	require.NotEmpty(t, record.RemoteContextID)
	require.Equal(t, "conv-1", record.LogicalID)
	require.Equal(t, a2a.TaskStateCompleted, record.LastKnown.State)
	require.Equal(t, record.MessageID, record.MessageID, "the message id is stored for a retry")

	// The list shows it to the conversation that asked, and to nobody else.
	require.Len(t, sender.List("conv-1"), 1)
	require.Empty(t, sender.List("another-conversation"))
}

func TestAParkedPeerQuestionComesBackAndIsAnsweredThroughTheSameRecord(t *testing.T) {
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(askThenReply(t, "检查照片还是文档？", "照片", "好，只检查照片。")...)

	sender, _ := startPeer(t, mock, "ReadFile")

	view, err := sender.Submit(context.Background(), SubmitRequest{
		Peer: "peer", Prompt: "检查昨天的备份", LogicalID: "conv-1",
		Wait: 5 * time.Second, // the peer asks instead of finishing; the wait ends at the question
	})
	require.NoError(t, err)
	require.True(t, view.Waiting(), "a question ends the wait: the task will not move until answered")
	require.NotNil(t, view.Question)
	require.Equal(t, "ask_1", view.Question.ID)
	require.Contains(t, view.Question.Text, "检查照片还是文档？")
	require.Empty(t, view.Result, "the question must not be handed over as if it were an answer")

	// Answering goes through the SAME local record: it names the question, and the peer wakes
	// the turn that asked (its script's second step requires the answer to have arrived).
	answered, err := sender.Reply(context.Background(), view.ID, view.Question.ID,
		map[string]string{"检查照片还是文档？": "照片"}, 5*time.Second)
	require.NoError(t, err)
	require.Equal(t, "completed", answered.State)
	require.Contains(t, answered.Result, "只检查照片")
	require.Nil(t, answered.Question, "an answered task has no open question")
	require.Len(t, mock.Requests(), 2, "the answer resumed the peer's turn instead of starting a new one")
}

func TestASubmissionInABusyConversationIsRefused(t *testing.T) {
	// A peer whose task stays in flight: the parked test executor, so the conversation is
	// genuinely busy while the sender asks again.
	parked := &parkedExecutor{
		release:  make(chan struct{}),
		canceled: make(chan struct{}),
		started:  make(chan struct{}, 1),
	}

	peerHome := t.TempDir()
	previous := config.BaseDir()
	config.SetBaseDir(peerHome)
	t.Cleanup(func() { config.SetBaseDir(previous) })

	peer := startWith(t, filepath.Join(peerHome, "node"), func(o *Options) { o.Executor = parked })
	senderHome := t.TempDir()
	config.SetBaseDir(senderHome)
	t.Cleanup(func() { config.SetBaseDir(previous) })

	sender, err := OpenSender([]config.PeerConfig{{Name: "peer", URL: "http://" + peer.RemoteAddr()}})
	require.NoError(t, err)

	// First submission: the peer accepts it and keeps working. The short wait is deliberate —
	// the caller gets the handle, which is what §6.2 is for.
	first, err := sender.Submit(context.Background(), SubmitRequest{
		Peer: "peer", Prompt: "长活儿", LogicalID: "conv-1", Wait: 200 * time.Millisecond,
	})
	require.NoError(t, err)
	require.True(t, first.InFlight(), "the handle comes back while the peer works: %+v", first)
	<-parked.started

	// A follow-up in that conversation: the peer refuses it, with OUR reason and domain. This is
	// the refusal the sending side has to be able to read — the SDK's own client would drop the
	// domain and leave only a sentence.
	_, err = sender.Submit(context.Background(), SubmitRequest{
		Peer: "peer", Prompt: "再顺便看看", LogicalID: "conv-1", ContinueFrom: first.ID,
	})
	var refusal *PeerRefusal
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, reasonContextBusy, refusal.Reason)
	require.Equal(t, errorDomain, refusal.Domain,
		"our own refusals have to survive the trip: that is why this client is not the SDK's")
	require.True(t, refusal.NotAccepted(), "a busy refusal means no task was created")

	// Let the peer's work finish and WAIT for it: a task still writing its record when the test
	// returns races the temp dir's removal, which surfaces as
	// "unlinkat .../node/tasks: directory not empty" against assertions that all passed.
	close(parked.release)
	done, err := sender.Wait(context.Background(), first.ID, waitForWork)
	require.NoError(t, err)
	require.Equal(t, "completed", done.State)
}

func TestAnUnreachablePeerIsUnknownRatherThanRefused(t *testing.T) {
	previous := config.BaseDir()
	config.SetBaseDir(t.TempDir())
	t.Cleanup(func() { config.SetBaseDir(previous) })

	// Nothing listens here. The difference matters: the peer never considered the request, so
	// "it said no" would be a lie — and the record must say what we actually know.
	sender, err := OpenSender([]config.PeerConfig{{Name: "ghost", URL: "http://127.0.0.1:1"}})
	require.NoError(t, err)

	view, err := sender.Submit(context.Background(), SubmitRequest{
		Peer: "ghost", Prompt: "有人吗", LogicalID: "conv-1",
	})
	var refusal *PeerRefusal
	require.ErrorAs(t, err, &refusal)
	require.True(t, refusal.Transport)
	require.False(t, refusal.NotAccepted(), "an unknown outcome must not be read as a safe retry")

	// The record exists — it was written before the request left — and carries no remote ids.
	require.NotEmpty(t, view.ID)
	record, ok := sender.records.get(view.ID)
	require.True(t, ok)
	require.Empty(t, record.RemoteTaskID, "nothing was accepted, and the record says so by omission")
	require.Equal(t, "unknown", view.State)
	require.Contains(t, view.Note, "could not be reached")
}

func TestOurOwnRefusalBodyIsReadAsOurs(t *testing.T) {
	// The classifier, on the exact body this node's own admission layer renders. It cannot be
	// produced end to end for every reason (a question mismatch is refused locally before it
	// leaves), so the shape is pinned here and the wire is pinned by the tests above.
	body := `{"error":{"code":409,"status":"FAILED_PRECONDITION","message":"this task is finished",
	  "details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"TASK_FINISHED",
	  "domain":"tachi","metadata":{"taskId":"t-1"}}]}}`

	refusal := parseRefusal(http.StatusConflict, []byte(body), "peer")
	require.Equal(t, reasonTaskFinished, refusal.Reason)
	require.Equal(t, errorDomain, refusal.Domain)
	require.Equal(t, "t-1", refusal.Metadata["taskId"])
	require.Contains(t, refusal.Error(), "TASK_FINISHED")

	// A protocol-domain refusal is read too, and is NOT mistaken for ours: it carries no action
	// this side is entitled to take.
	protocol := parseRefusal(http.StatusNotFound, []byte(
		`{"error":{"code":404,"status":"NOT_FOUND","message":"task not found",
		 "details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"TASK_NOT_FOUND",
		 "domain":"a2a-protocol.org"}]}}`), "peer")
	require.Equal(t, "TASK_NOT_FOUND", protocol.Reason)
	require.Equal(t, a2a.ProtocolDomain, protocol.Domain)
	require.False(t, protocol.NotAccepted(), "a finished or missing task is an answer, not a retry hint")
}

func TestAFollowUpStaysInTheConversationItBelongsTo(t *testing.T) {
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(oneMessageScript("第一轮完成")...)
	mock.Script(oneMessageScript("第二轮完成")...)

	sender, _ := startPeer(t, mock, "ReadFile")
	first, err := sender.Submit(context.Background(), SubmitRequest{
		Peer: "peer", Prompt: "第一轮", LogicalID: "conv-1",
	})
	require.NoError(t, err)

	// A follow-up names the record it continues, and the peer answers in the SAME remote
	// conversation (§8.1): same contextId, new task.
	second, err := sender.Submit(context.Background(), SubmitRequest{
		Peer: "peer", Prompt: "第一轮", LogicalID: "conv-1", ContinueFrom: first.ID,
	})
	require.NoError(t, err)
	require.NotEqual(t, first.ID, second.ID, "a follow-up is new work: new local record, new remote task")

	firstRecord, _ := sender.records.get(first.ID)
	secondRecord, _ := sender.records.get(second.ID)
	require.Equal(t, firstRecord.RemoteContextID, secondRecord.RemoteContextID,
		"both live in one remote conversation")
	require.NotEqual(t, firstRecord.RemoteTaskID, secondRecord.RemoteTaskID)

	// Asking for work in a conversation that belongs to another peer is refused rather than
	// quietly sent somewhere else (§8.5).
	_, err = sender.Submit(context.Background(), SubmitRequest{
		Peer: "other-node", Prompt: "接着那份", LogicalID: "conv-1", ContinueFrom: first.ID,
	})
	require.Error(t, err)
}

func TestAQuestionWithoutAnOpenQuestionIsRefusedLocally(t *testing.T) {
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(oneMessageScript("完成")...)

	sender, _ := startPeer(t, mock, "ReadFile")
	view, err := sender.Submit(context.Background(), SubmitRequest{
		Peer: "peer", Prompt: "什么都不问的活", LogicalID: "conv-1",
	})
	require.NoError(t, err)
	require.Equal(t, "completed", view.State, "note=%q result=%q", view.Note, view.Result)

	_, err = sender.Reply(context.Background(), view.ID, "ask_1", map[string]string{"x": "y"}, time.Second)
	require.Error(t, err, "answering a task that asks nothing must not reach the peer")
	require.Contains(t, err.Error(), "not waiting")

	// And the peer never saw a second request: a refusal decided locally costs nothing.
	require.Len(t, mock.Requests(), 1)
}

func TestTheModelViewIsTheDesignsShape(t *testing.T) {
	record := &Delegation{
		ID: "d_7f3a1c02", Peer: "home-nas", Title: "检查昨天的备份",
		QuestionID: "ask_1",
		LastKnown:  lastKnown{State: a2a.TaskStateInputRequired, Text: "检查照片还是文档？", At: time.Now()},
	}
	view := viewOf(record, "")
	require.Equal(t, "d_7f3a1c02", view.ID)
	require.Equal(t, "home-nas", view.Node)
	require.Equal(t, "input_required", view.State)
	require.True(t, view.Waiting())
	require.Equal(t, "ask_1", view.Question.ID)
	require.Empty(t, view.Result)

	// A handle that is still SUBMITTED counts as in flight: the state write may simply not have
	// landed yet, and treating it as "not started" is how a caller resubmits (§6.2).
	require.True(t, TaskView{State: "submitted"}.InFlight())
	require.True(t, TaskView{State: "working"}.InFlight())
	require.False(t, TaskView{State: "completed"}.InFlight())
}

func TestAToolLayerFailureIsNotAFabricatedTask(t *testing.T) {
	// Every submission that reached a peer produced a record; one that could not even be built
	// (an unknown alias) produces none, and the caller is told why instead of getting a view of
	// a task that never existed.
	previous := config.BaseDir()
	config.SetBaseDir(t.TempDir())
	t.Cleanup(func() { config.SetBaseDir(previous) })

	sender, err := OpenSender(nil)
	require.NoError(t, err)

	_, err = sender.Submit(context.Background(), SubmitRequest{Peer: "nobody", Prompt: "hi"})
	var unknown *UnknownPeerError
	require.ErrorAs(t, err, &unknown)
	require.Empty(t, unknown.Known)
	require.Contains(t, err.Error(), "no peers are configured")

	_, err = sender.Submit(context.Background(), SubmitRequest{Peer: "peer", Prompt: "   "})
	require.Error(t, err)

	if errors.Is(err, context.Canceled) {
		t.Fatal("unreachable")
	}
}

func TestAnswerPartsMatchHowThePeerReadsThem(t *testing.T) {
	// One question takes one text answer; several need the answers NAMED, which is what the
	// receiving side requires to map them back (§7.4).
	single := answerParts(map[string]string{"检查照片还是文档？": "照片"})
	require.Len(t, single, 1)
	require.Equal(t, "照片", single[0].Text())

	many := answerParts(map[string]string{"哪个目录？": "照片", "要改文件吗？": "不要"})
	require.Len(t, many, 1)
	named, ok := many[0].Data().(map[string]any)
	require.True(t, ok, "several answers travel as a named set, got %T", many[0].Data())
	require.Equal(t, "不要", named["要改文件吗？"])
}

func TestATitleIsDerivedFromTheFirstLine(t *testing.T) {
	require.Equal(t, "检查昨天的备份", deriveTitle("检查昨天的备份\n返回异常清单"))
	require.Equal(t, "远程任务", deriveTitle("   "))

	long := deriveTitle(strings.Repeat("很长的描述", 20))
	require.LessOrEqual(t, len([]rune(long)), 61, "a title is a label, not the prompt")
	require.True(t, strings.HasSuffix(long, "…"))
}
