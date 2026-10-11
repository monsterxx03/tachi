package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/agent/tools"
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

	peer := startRealNode(t, nodeConfig(t, mock, filepath.Join(peerHome, "ws")))

	// The sender's home: another base dir, with a peer entry pointing at the receiver. Only one
	// process-global base dir exists, so this is built in two steps — records written under the
	// sender's dir, addresses taken from the receiver that is already up.
	return openSenderFor(t, "http://"+peer.RemoteAddr()), peer
}

// openSenderFor points a sender at a peer URL, in its OWN home — the sender hosts no node, which
// is the case this whole half of the package exists for. Split out from startPeer so a test can
// aim it at something other than the receiver itself (a proxy in front of one, say).
func openSenderFor(t *testing.T, peerURL string) *Sender {
	t.Helper()
	senderHome := t.TempDir()
	previous := config.BaseDir()
	config.SetBaseDir(senderHome)
	t.Cleanup(func() { config.SetBaseDir(previous) })

	sender, err := OpenSender([]config.PeerConfig{{Name: "peer", URL: peerURL}}, "", nil)
	require.NoError(t, err)
	return sender
}

func TestASubmissionReachesThePeerAndItsAnswerComesBack(t *testing.T) {
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(oneMessageScript("备份检查完成：两个目录异常")...)

	sender, peer := startPeer(t, mock, "ReadFile")
	require.NotNil(t, peer)

	view, err := sender.Delegate(context.Background(), tools.DelegationRequest{
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
	require.Len(t, sender.RemoteTasks("conv-1"), 1)
	require.Empty(t, sender.RemoteTasks("another-conversation"))
}

func TestAParkedPeerQuestionComesBackAndIsAnsweredThroughTheSameRecord(t *testing.T) {
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(askThenReply(t, "检查照片还是文档？", "照片", "好，只检查照片。")...)

	sender, _ := startPeer(t, mock, "ReadFile")

	view, err := sender.Delegate(context.Background(), tools.DelegationRequest{
		Peer: "peer", Prompt: "检查昨天的备份", LogicalID: "conv-1",
		Wait: 5 * time.Second, // the peer asks instead of finishing; the wait ends at the question
	})
	require.NoError(t, err)
	require.True(t, view.Waiting(), "a question ends the wait: the task will not move until answered")
	require.Equal(t, "ask_1", view.QuestionID)
	require.Contains(t, view.QuestionText, "检查照片还是文档？")
	require.Empty(t, view.Result, "the question must not be handed over as if it were an answer")

	// Answering goes through the SAME local record: it names the question, and the peer wakes
	// the turn that asked (its script's second step requires the answer to have arrived).
	answered, err := sender.Reply(context.Background(), view.ID, view.QuestionID,
		map[string]string{"检查照片还是文档？": "照片"}, 5*time.Second)
	require.NoError(t, err)
	require.Equal(t, "completed", answered.State)
	require.Contains(t, answered.Result, "只检查照片")
	require.Empty(t, answered.QuestionID, "an answered task has no open question")
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

	peer := startWith(t, filepath.Join(peerHome, "a2a"), func(o *Options) { o.Executor = parked })
	senderHome := t.TempDir()
	config.SetBaseDir(senderHome)
	t.Cleanup(func() { config.SetBaseDir(previous) })

	sender, err := OpenSender([]config.PeerConfig{{Name: "peer", URL: "http://" + peer.RemoteAddr()}}, "", nil)
	require.NoError(t, err)

	// First submission: the peer accepts it and keeps working. The short wait is deliberate —
	// the caller gets the handle, which is what §6.2 is for.
	first, err := sender.Delegate(context.Background(), tools.DelegationRequest{
		Peer: "peer", Prompt: "长活儿", LogicalID: "conv-1", Wait: 200 * time.Millisecond,
	})
	require.NoError(t, err)
	require.True(t, first.InFlight(), "the handle comes back while the peer works: %+v", first)
	<-parked.started

	// A follow-up in that conversation: the peer refuses it, with OUR reason and domain. This is
	// the refusal the sending side has to be able to read — the SDK's own client would drop the
	// domain and leave only a sentence.
	_, err = sender.Delegate(context.Background(), tools.DelegationRequest{
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
	sender, err := OpenSender([]config.PeerConfig{{Name: "ghost", URL: "http://127.0.0.1:1"}}, "", nil)
	require.NoError(t, err)

	view, err := sender.Delegate(context.Background(), tools.DelegationRequest{
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
	first, err := sender.Delegate(context.Background(), tools.DelegationRequest{
		Peer: "peer", Prompt: "第一轮", LogicalID: "conv-1",
	})
	require.NoError(t, err)

	// A follow-up names the record it continues, and the peer answers in the SAME remote
	// conversation (§8.1): same contextId, new task.
	second, err := sender.Delegate(context.Background(), tools.DelegationRequest{
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
	_, err = sender.Delegate(context.Background(), tools.DelegationRequest{
		Peer: "other-node", Prompt: "接着那份", LogicalID: "conv-1", ContinueFrom: first.ID,
	})
	require.Error(t, err)
}

// A follow-up does NOT have to name the machine, and that is the shape the tool produces: its
// contract is "exactly one of node / continue_from" (tools.DelegateTask), and §8.1's own example
// of a continuation is `{"continue_from": "d_7f3a", "prompt": …}` — no node in it.
//
// This used to be impossible. Submit looked the alias up first, so an absent one was refused as an
// unknown peer ("node: \"\" is not a configured peer") before the record was ever consulted, and
// the tool refuses to be given both — so there was no way to continue anything at all. The record
// is the authority on which machine it belongs to; that is what §8.1 means by 服务端由它解析.
func TestAFollowUpWithoutANodeTakesTheMachineFromTheRecord(t *testing.T) {
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(oneMessageScript("第一轮完成")...)
	mock.Script(oneMessageScript("第二轮完成")...)

	sender, _ := startPeer(t, mock, "ReadFile")
	first, err := sender.Delegate(context.Background(), tools.DelegationRequest{
		Peer: "peer", Prompt: "第一轮", LogicalID: "conv-1",
	})
	require.NoError(t, err)

	second, err := sender.Delegate(context.Background(), tools.DelegationRequest{
		Prompt: "接着那份", LogicalID: "conv-1", ContinueFrom: first.ID,
	})
	require.NoError(t, err, "the record names the machine, so the caller does not have to")
	require.Equal(t, first.Node, second.Node)

	firstRecord, _ := sender.records.get(first.ID)
	secondRecord, _ := sender.records.get(second.ID)
	require.Equal(t, firstRecord.RemoteContextID, secondRecord.RemoteContextID,
		"and it lands in the SAME remote conversation — which is the whole point of continuing")
	require.NotEqual(t, firstRecord.RemoteTaskID, secondRecord.RemoteTaskID)
}

// The record's machine is not configured any more. Said as its own fact rather than as an unknown
// peer: nothing was named, so "which alias did you mean" would be an answer to a question the
// caller did not ask.
func TestAFollowUpWhoseMachineIsGoneSaysSo(t *testing.T) {
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(oneMessageScript("第一轮完成")...)

	sender, _ := startPeer(t, mock, "ReadFile")
	first, err := sender.Delegate(context.Background(), tools.DelegationRequest{
		Peer: "peer", Prompt: "第一轮", LogicalID: "conv-1",
	})
	require.NoError(t, err)

	// The operator removed that peer, and this is a sender built on the SAME home: the records are
	// read back from disk, which is what makes this "the config changed", not "the cache is stale".
	// Another peer IS configured — the point is which alias is missing, not that the list is empty.
	stripped, err := OpenSender([]config.PeerConfig{
		{Name: "someone-else", URL: "https://127.0.0.1:1"},
	}, "tachi@mac", nil)
	require.NoError(t, err)
	_, err = stripped.Delegate(context.Background(), tools.DelegationRequest{
		Prompt: "接着那份", LogicalID: "conv-1", ContinueFrom: first.ID,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a configured peer any more")
	require.Contains(t, err.Error(), first.ID, "the refusal names the record the caller must act on")
}

// A long answer is cut by CHARACTERS, never in the middle of one — and the WHOLE thing stays on this
// machine. The first characters of an answer are usually the answer (a reminder shows the same text,
// and must stay a summary), while the rest used to be unreachable at any price: the peer keeps its
// own copy, and every route back to us shortens it the same way.
func TestALongAnswerIsKeptWholeOnDiskAndCutSafelyInTheText(t *testing.T) {
	// A spill dir of its own: the text is written where a model can ReadFile it, and a test must not
	// write into the real ~/.tachi/tool_results.
	c := &remoteClient{spillDir: t.TempDir()}
	const id = "d_0000abcd"

	// 3 bytes each, so a byte budget and a character budget cannot agree by accident.
	long := strings.Repeat("中", answerExcerptLen+500)
	got := c.boundAnswer(id, long)

	require.NotContains(t, got, "\uFFFD", "a half-cut character is what this test exists for")
	require.True(t, strings.HasPrefix(got, strings.Repeat("中", answerExcerptLen)))
	require.Contains(t, got, "已截断")

	// The whole answer is on disk, byte for byte, at the path the text names — and the path is
	// derived from the RECORD rather than a timestamp, because this text travels into the reminder:
	// a rendering that changes for no reason costs the prompt prefix on every turn.
	path := filepath.Join(c.spillDir, "remote_"+id+".txt")
	require.Contains(t, got, path)
	stored, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, long, string(stored), "the whole answer, not the excerpt")

	// Seeing the same answer again (a `get` after the task already finished) points at the same file
	// and does not leave a second copy behind.
	require.Contains(t, c.boundAnswer(id, long), path)
	entries, err := os.ReadDir(c.spillDir)
	require.NoError(t, err)
	require.Len(t, entries, 1)

	// An answer that fits is handed over untouched, and writes nothing. The boundary is included:
	// exactly the budget is not "too long".
	require.Equal(t, "全部拿到了", c.boundAnswer(id, "全部拿到了"))
	require.Equal(t, strings.Repeat("中", answerExcerptLen), c.boundAnswer(id, strings.Repeat("中", answerExcerptLen)))
	entries, err = os.ReadDir(c.spillDir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

// A spill that cannot happen must not cost the answer its first characters as well. No directory is
// a real possibility (a config that names none, a disk that is full, permissions), and a tool call
// that fails is worse than one that answers with less.
func TestAnAnswerThatCannotBeSpilledStillComesBackShortened(t *testing.T) {
	c := &remoteClient{} // no spill dir: every write has to fail
	got := c.boundAnswer("d_0000abcd", strings.Repeat("中", answerExcerptLen+10))

	require.Contains(t, got, "已截断")
	require.Contains(t, got, "对端", "the honest note says where the rest really is")
	require.NotContains(t, got, "本机", "no local path may be named when nothing was written")
	require.Equal(t, answerExcerptLen, len([]rune(strings.TrimSuffix(got, excerptNote))))
}

// The whole trip, with an answer long enough to matter: the peer sends it, the record keeps the
// head, and the REST is on this machine for the model to read.
//
// This is the scenario the shortening used to lose. The full text was decoded here for one instant
// and then dropped — nothing could get it back, because the peer's copy is behind the same bound
// (a `get` shortens it again), which is why the only advice left was "ask for less next time".
func TestALongRemoteAnswerSurvivesOnThisMachinesDisk(t *testing.T) {
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	long := "## 1. `df -h`\n" + strings.Repeat("Filesystem 3.7T 1.3T 2.4T 35% /vol1\n", 300)
	mock.Script(oneMessageScript(long)...)

	sender, _ := startPeer(t, mock, "ReadFile")
	view, err := sender.Delegate(context.Background(), tools.DelegationRequest{
		Peer: "peer", Prompt: "查磁盘", LogicalID: "conv-1",
	})
	require.NoError(t, err)

	// The spill dir is this machine's own (under the sender's --home, never the real ~/.tachi).
	path := filepath.Join(config.ToolResultsDir(), "remote_"+view.ID+".txt")
	require.Contains(t, view.Result, path, "the shortened answer says where the rest is")
	require.Contains(t, view.Result, "已截断")
	require.Less(t, len([]rune(view.Result)), len([]rune(long)),
		"what reaches the model is the head, not the whole thing")

	stored, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, strings.TrimSpace(long), strings.TrimSpace(string(stored)),
		"and that file is everything the peer sent")
}

// A follow-up names the PREVIOUS remote task as its reference — not its own id, and not this
// machine's local `d_…` id (§8.1). The far side is what reads it: `referenceTaskIds` is how the
// peer is told which earlier piece of work this continues, so an id it has never seen (a local
// one) or a task it has not been told about yet (its own successor) tells it nothing.
//
// The outgoing body is not observable through the sender's API, so the receiver's listener is put
// behind a recording proxy: the card resolution, the submissions and the answers are the real
// ones, and what is captured on the way through is exactly what would have gone on the wire.
func TestAFollowUpReferencesThePreviousRemoteTask(t *testing.T) {
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(oneMessageScript("第一轮完成")...)
	mock.Script(oneMessageScript("第二轮完成")...)

	peerHome := t.TempDir()
	previous := config.BaseDir()
	config.SetBaseDir(peerHome)
	t.Cleanup(func() { config.SetBaseDir(previous) })
	peer := startRealNode(t, nodeConfig(t, mock, filepath.Join(peerHome, "ws")))

	upstream, err := url.Parse("http://" + peer.RemoteAddr())
	require.NoError(t, err)
	var mu sync.Mutex
	var sent []map[string]any
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if strings.HasSuffix(r.URL.Path, "message:send") {
			var parsed map[string]any
			if json.Unmarshal(body, &parsed) == nil {
				mu.Lock()
				sent = append(sent, parsed)
				mu.Unlock()
			}
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, upstream.String()+r.URL.RequestURI(), bytes.NewReader(body))
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		req.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(proxy.Close)

	sender := openSenderFor(t, proxy.URL)
	first, err := sender.Delegate(context.Background(), tools.DelegationRequest{
		Peer: "peer", Prompt: "第一轮", LogicalID: "conv-1",
	})
	require.NoError(t, err)
	second, err := sender.Delegate(context.Background(), tools.DelegationRequest{
		Peer: "peer", Prompt: "第二轮", LogicalID: "conv-1", ContinueFrom: first.ID,
	})
	require.NoError(t, err)

	firstRecord, ok := sender.records.get(first.ID)
	require.True(t, ok)
	secondRecord, ok := sender.records.get(second.ID)
	require.True(t, ok)
	require.NotEqual(t, firstRecord.RemoteTaskID, secondRecord.RemoteTaskID)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, sent, 2, "two submissions went out")
	firstMsg, ok := sent[0]["message"].(map[string]any)
	require.True(t, ok)
	require.NotContains(t, firstMsg, "referenceTaskIds", "the first submission continues nothing")

	secondMsg, ok := sent[1]["message"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, []any{firstRecord.RemoteTaskID}, secondMsg["referenceTaskIds"],
		"the follow-up references the PREVIOUS remote task")
	require.NotContains(t, secondMsg["referenceTaskIds"].([]any), secondRecord.RemoteTaskID,
		"…never its own")
}

func TestAQuestionWithoutAnOpenQuestionIsRefusedLocally(t *testing.T) {
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(oneMessageScript("完成")...)

	sender, _ := startPeer(t, mock, "ReadFile")
	view, err := sender.Delegate(context.Background(), tools.DelegationRequest{
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
	// No sender: a bare record with no identity renders under the alias it was created with.
	view := (&Sender{}).viewOf(record, "")
	require.Equal(t, "d_7f3a1c02", view.ID)
	require.Equal(t, "home-nas", view.Node)
	require.Equal(t, "input_required", view.State)
	require.True(t, view.Waiting())
	require.Equal(t, "ask_1", view.QuestionID)
	require.Empty(t, view.Result)

	// A handle that is still SUBMITTED counts as in flight: the state write may simply not have
	// landed yet, and treating it as "not started" is how a caller resubmits (§6.2).
	require.True(t, tools.RemoteTaskInfo{State: "submitted"}.InFlight())
	require.True(t, tools.RemoteTaskInfo{State: "working"}.InFlight())
	require.False(t, tools.RemoteTaskInfo{State: "completed"}.InFlight())
}

func TestAToolLayerFailureIsNotAFabricatedTask(t *testing.T) {
	// Every submission that reached a peer produced a record; one that could not even be built
	// (an unknown alias) produces none, and the caller is told why instead of getting a view of
	// a task that never existed.
	previous := config.BaseDir()
	config.SetBaseDir(t.TempDir())
	t.Cleanup(func() { config.SetBaseDir(previous) })

	sender, err := OpenSender(nil, "", nil)
	require.NoError(t, err)

	_, err = sender.Delegate(context.Background(), tools.DelegationRequest{Peer: "nobody", Prompt: "hi"})
	var unknown *UnknownPeerError
	require.ErrorAs(t, err, &unknown)
	require.Empty(t, unknown.Known)
	require.Contains(t, err.Error(), "no peers are configured")

	_, err = sender.Delegate(context.Background(), tools.DelegationRequest{Peer: "peer", Prompt: "   "})
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

// A peer may answer with a direct Message instead of creating a task, and that is an ANSWER, not a
// failure: the spec's SendMessageResponse is a oneOf and its words for this shape are "a direct
// Message response for simple interactions that don't require task tracking".
//
// This node used to refuse it — "answered with a message rather than a task" — which threw away
// the text the peer had just produced and left the record saying the outcome was UNKNOWN. Three
// different stories about one successful exchange, and the model got the worst of them.
//
// The stub IS the peer: one route, and no card (an unreachable card is not a failure either, §8.6).
// A real Tachi node always answers with a Task, so only a stub can produce this shape at all.
func messageAnswerPeer(t *testing.T, contextID string) (*Sender, func() int, func() []map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "message:send") {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()

		answer := map[string]any{
			"messageId": "m-answer",
			"role":      "agent",
			"parts":     []any{map[string]any{"text": "直接答复：两个目录都完整"}},
		}
		if contextID != "" {
			answer["contextId"] = contextID
		}
		raw, _ = json.Marshal(map[string]any{"message": answer})
		w.Header().Set("Content-Type", a2aMediaType)
		_, _ = w.Write(raw)
	}))
	t.Cleanup(srv.Close)
	sender := openSenderFor(t, srv.URL)
	return sender,
		func() int { mu.Lock(); defer mu.Unlock(); return len(bodies) },
		func() []map[string]any { mu.Lock(); defer mu.Unlock(); return bodies }
}

func TestAPeerThatAnswersWithAMessageIsRecordedAsAnAnswer(t *testing.T) {
	sender, requests, _ := messageAnswerPeer(t, "ctx-direct")

	view, err := sender.Delegate(context.Background(), tools.DelegationRequest{
		Peer: "peer", Prompt: "检查备份", LogicalID: "conv-1",
	})
	require.NoError(t, err, "a direct message is an answer, not a failure")
	require.Equal(t, "completed", view.State,
		"the interaction is over and the answer is in hand — not \"unknown\"")
	require.Contains(t, view.Result, "两个目录都完整")
	require.Contains(t, view.Note, "direct message",
		"the model has to be told there is no handle, or it will try to wait on one")
	require.False(t, view.InFlight(), "there is nothing in flight to wait for")
	require.Empty(t, view.QuestionID, "a direct message cannot be a question for us to answer")

	// Asking again is answered from the record, without a round trip: there is no task to ask about.
	before := requests()
	again, err := sender.Get(context.Background(), view.ID)
	require.NoError(t, err)
	require.Equal(t, view.Result, again.Result)
	require.Equal(t, before, requests(), "a handle-less record must not cost a request")

	// Cancelling says WHICH reason there is nothing to cancel, not the wrong one.
	_, err = sender.Cancel(context.Background(), view.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "answered directly")
	require.NotContains(t, err.Error(), "never accepted",
		"it WAS accepted — the peer answered it")

	// And it shows up in the conversation's own list, like any other delegated work.
	list := sender.RemoteTasks("conv-1")
	require.Len(t, list, 1)
	require.Equal(t, view.ID, list[0].ID)
	require.Empty(t, sender.RemoteTasks("conv-other"), "another conversation sees none of it")
}

// The conversation is still continuable, because the answer named one: the follow-up goes out on
// the SAME contextId (that is what lets a peer pick up its own history), and it references no task,
// because this exchange never produced one.
func TestAFollowUpContinuesTheConversationADirectAnswerNamed(t *testing.T) {
	sender, _, bodies := messageAnswerPeer(t, "ctx-direct")

	first, err := sender.Delegate(context.Background(), tools.DelegationRequest{
		Peer: "peer", Prompt: "检查备份", LogicalID: "conv-1",
	})
	require.NoError(t, err)

	_, err = sender.Delegate(context.Background(), tools.DelegationRequest{
		Peer: "peer", Prompt: "缺失的那两个目录呢", LogicalID: "conv-1", ContinueFrom: first.ID,
	})
	require.NoError(t, err)

	sent := bodies()
	require.Len(t, sent, 2)
	follow, ok := sent[1]["message"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "ctx-direct", follow["contextId"],
		"the follow-up continues the conversation the answer named")
	require.NotContains(t, follow, "referenceTaskIds",
		"there is no task to reference — a direct answer never made one")
}

// Without a contextId there is nothing to continue, and the refusal says WHICH of the two reasons
// it is: this node answered, it just never named a conversation.
func TestADirectAnswerWithoutAContextCannotBeContinued(t *testing.T) {
	sender, _, _ := messageAnswerPeer(t, "")

	first, err := sender.Delegate(context.Background(), tools.DelegationRequest{
		Peer: "peer", Prompt: "检查备份", LogicalID: "conv-1",
	})
	require.NoError(t, err)

	_, err = sender.Delegate(context.Background(), tools.DelegationRequest{
		Peer: "peer", Prompt: "接着说", LogicalID: "conv-1", ContinueFrom: first.ID,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "answered directly")
	require.NotContains(t, err.Error(), "never accepted")
}
