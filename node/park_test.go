package node

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/itest/mockllm"
)

// The parked question, from the parking lot up to a real caller: a remote task that stops to
// ask, the answer that resumes the SAME turn, and every way the answer can be wrong.

// ---- the answer's shape (answersFor) ----

func TestOneTextAnswerAnswersOneQuestion(t *testing.T) {
	questions := []tools.Question{{Question: "检查照片还是文档？", Header: "范围"}}

	answers, err := answersFor(questions, a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("照片")))
	require.NoError(t, err)
	require.Equal(t, map[string]string{"检查照片还是文档？": "照片"}, answers,
		"answers are keyed by the full question text — the shape the TUI and the desktop use")
}

func TestAnAnswerMustNameTheQuestionWhenSeveralAreWaiting(t *testing.T) {
	questions := []tools.Question{
		{Question: "检查哪个目录？", Header: "目标"},
		{Question: "要不要改文件？", Header: "权限"},
	}

	// One text answer cannot say which question it belongs to, and a message has no
	// positions: guessing would answer a question nobody answered.
	_, err := answersFor(questions, a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("照片")))
	require.ErrorIs(t, err, errAnswerShape)

	named := a2a.NewDataPart(map[string]any{
		"检查哪个目录？": "照片",
		"要不要改文件？": "不要",
	})
	answers, err := answersFor(questions, a2a.NewMessage(a2a.MessageRoleUser, named))
	require.NoError(t, err)
	require.Equal(t, map[string]string{"检查哪个目录？": "照片", "要不要改文件？": "不要"}, answers)
}

func TestAnAnswerToAQuestionThisTaskIsNotAskingIsRefused(t *testing.T) {
	questions := []tools.Question{{Question: "检查哪个目录？", Header: "目标"}}

	// A named answer that mentions something else is a client with the wrong idea — and
	// delivering it would answer whatever happens to be parked instead.
	_, err := answersFor(questions, a2a.NewMessage(a2a.MessageRoleUser,
		a2a.NewDataPart(map[string]any{"另一个问题？": "yes"})))
	require.ErrorIs(t, err, errAnswerShape)
}

func TestAMultiSelectAnswerJoinsItsLabels(t *testing.T) {
	questions := []tools.Question{{Question: "检查哪些？", Header: "范围", MultiSelect: true}}

	answers, err := answersFor(questions, a2a.NewMessage(a2a.MessageRoleUser,
		a2a.NewDataPart(map[string]any{"检查哪些？": []any{"照片", "视频"}})))
	require.NoError(t, err)
	require.Equal(t, "照片, 视频", answers["检查哪些？"],
		"the same joining the TUI does, so the model sees one shape whichever frontend asked")
}

// ---- the parking lot ----

func TestASlotIsConsumedByTheAnswerItAccepts(t *testing.T) {
	var expired int
	lot := newParking(time.Minute, 0, func(*parkedAsk) { expired++ })
	ask := &parkedAsk{taskID: "t1", questionID: "q1", questions: []tools.Question{{Question: "q?"}}}
	require.NoError(t, lot.park(ask))

	got, ok := lot.take("t1")
	require.True(t, ok)
	require.Equal(t, "q1", got.questionID)

	_, ok = lot.take("t1")
	require.False(t, ok, "the second answer to one question must find nothing, not the slot again")
	require.Zero(t, expired, "an answered question must not expire after the fact")
}

func TestTheLotRefusesMoreQuestionsThanItWillHold(t *testing.T) {
	lot := newParking(time.Minute, 1, nil)
	require.NoError(t, lot.park(&parkedAsk{taskID: "t1", questionID: "q1"}))
	require.ErrorIs(t, lot.park(&parkedAsk{taskID: "t2", questionID: "q2"}), errTooManyQuestions)
	require.Equal(t, 1, lot.waiting())
}

func TestAQuestionNobodyAnswersExpires(t *testing.T) {
	fired := make(chan *parkedAsk, 1)
	lot := newParking(20*time.Millisecond, 0, func(a *parkedAsk) { fired <- a })

	ask := &parkedAsk{taskID: "t1", questionID: "q1"}
	require.NoError(t, lot.park(ask))

	select {
	case got := <-fired:
		require.Equal(t, ask, got)
	case <-time.After(waitForWork):
		t.Fatal("an unanswered question must run out of time")
	}
	_, ok := lot.slot("t1")
	require.False(t, ok, "an expired question is no longer waiting")
}

func TestForgettingAQuestionDisarmsItsDeadline(t *testing.T) {
	fired := make(chan *parkedAsk, 1)
	lot := newParking(30*time.Millisecond, 0, func(a *parkedAsk) { fired <- a })

	ask := &parkedAsk{taskID: "t1", questionID: "q1"}
	require.NoError(t, lot.park(ask))
	lot.forget("t1")

	select {
	case got := <-fired:
		t.Fatalf("a question that was answered (or cancelled) must not expire afterwards: %v", got)
	case <-time.After(150 * time.Millisecond):
	}
}

// ---- the question and the answer over the wire ----

// contains is a mockllm.Matcher: the mock's core deliberately keeps gomega out, and this
// file only needs one substring check.
type contains string

func (c contains) Match(actual any) (bool, error) {
	s, ok := actual.(string)
	return ok && strings.Contains(s, string(c)), nil
}

// askArgs renders an AskUserQuestion tool call's arguments.
func askArgs(t *testing.T, question string, options ...string) string {
	t.Helper()
	opts := make([]map[string]string, 0, len(options))
	for _, o := range options {
		opts = append(opts, map[string]string{"label": o, "description": o + " 这一类的"})
	}
	raw, err := json.Marshal(map[string]any{
		"questions": []map[string]any{{
			"question": question, "header": "范围", "options": opts, "multiSelect": false,
		}},
	})
	require.NoError(t, err)
	return string(raw)
}

// askThenReply is a model that asks one question, and — only if the answer really reached
// the tool call that asked it — replies with the turn's answer.
func askThenReply(t *testing.T, question, wantInAnswer, reply string) []mockllm.Step {
	t.Helper()
	return []mockllm.Step{
		{Reply: mockllm.Stream(
			mockllm.ToolCallStart("ask_1", tools.ToolNameAskUser, askArgs(t, question, "照片", "文档")),
			mockllm.Finish("tool_calls"),
			mockllm.Done(),
		)},
		{
			Require: mockllm.HasToolResult("ask_1", contains(wantInAnswer)),
			Reply: mockllm.Stream(
				mockllm.Text(reply),
				mockllm.Finish("stop"),
				mockllm.Done(),
			),
		},
	}
}

func TestARemoteTaskPausesOnAQuestionAndTheAnswerResumesTheSameTurn(t *testing.T) {
	base := nodeHome(t)
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(askThenReply(t, "检查照片还是文档？", "照片", "好，只检查照片。")...)

	h := startRealNode(t, nodeConfig(t, mock, filepath.Join(base, "ws")))
	client := newTestClient(t, h)

	first, err := client.SendMessage(context.Background(), sendRequest("检查昨天的备份", false))
	require.NoError(t, err)
	task, ok := first.(*a2a.Task)
	require.True(t, ok)

	// A blocking send returns when the task reaches an interrupted state (§6.2), and what it
	// returns is the QUESTION: the task is parked, not finished, and has no product yet.
	require.Equal(t, a2a.TaskStateInputRequired, task.Status.State)
	require.Empty(t, task.Artifacts)
	asked := messageText(task.Status.Message)
	require.Contains(t, asked, "检查照片还是文档？")
	require.Contains(t, asked, "文档", "the options travel with the question, or the caller cannot pick one")

	questionID := QuestionID(task.Status.Message)
	require.Equal(t, "ask_1", questionID,
		"the question's id is the tool call that asked it: what makes a late answer unambiguous")

	// The answer names the task AND the question it answers (§8.5).
	answer := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("照片"))
	answer.TaskID = task.ID
	answer.ContextID = task.ContextID
	answer.Metadata = map[string]any{QuestionIDMetadataKey: questionID}

	second, err := client.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: answer})
	require.NoError(t, err)
	final, ok := second.(*a2a.Task)
	require.True(t, ok)
	require.Equal(t, a2a.TaskStateCompleted, final.Status.State, "status message: %s", messageText(final.Status.Message))
	require.Equal(t, "好，只检查照片。", firstPartText(t, final.Artifacts[0]))

	// Two model calls, one turn: the second one carried the answer as the FIRST call's tool
	// result (its reply was scripted behind that requirement). A resumed turn that had
	// started a new loop would have asked the question again instead.
	require.Len(t, mock.Requests(), 2)
}

// The question is on disk before anyone can see it, which is what makes a caller that walks
// away and comes back (or a person reading the task tomorrow) able to see what was asked —
// and what makes the answer checkable against a record rather than against a live process.
func TestTheQuestionIsInTheTaskRecordWhileItWaits(t *testing.T) {
	base := nodeHome(t)
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(mockllm.Step{Reply: mockllm.Stream(
		mockllm.ToolCallStart("ask_1", tools.ToolNameAskUser, askArgs(t, "检查照片还是文档？", "照片", "文档")),
		mockllm.Finish("tool_calls"),
		mockllm.Done(),
	)})

	h := startRealNode(t, nodeConfig(t, mock, filepath.Join(base, "ws")))
	client := newTestClient(t, h)

	first, err := client.SendMessage(context.Background(), sendRequest("检查昨天的备份", false))
	require.NoError(t, err)
	task := first.(*a2a.Task)

	got, err := client.GetTask(context.Background(), &a2a.GetTaskRequest{ID: task.ID})
	require.NoError(t, err)
	require.Equal(t, a2a.TaskStateInputRequired, got.Status.State)
	require.Contains(t, messageText(got.Status.Message), "检查照片还是文档？")
	require.Equal(t, "ask_1", QuestionID(got.Status.Message),
		"the question's id is part of the record, not only of the process that asked")
	require.Contains(t, messageText(got.Status.Message), "文档",
		"the options are in the record too — an answer is checked against what was asked")
}

// ---- the ways an answer can be wrong (the guard's own refusals) ----

func TestAnAnswerWithoutTheQuestionIDIsRefused(t *testing.T) {
	base := nodeHome(t)
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(mockllm.Step{Reply: mockllm.Stream(
		mockllm.ToolCallStart("ask_1", tools.ToolNameAskUser, askArgs(t, "检查照片还是文档？", "照片")),
		mockllm.Finish("tool_calls"), mockllm.Done(),
	)})

	h := startRealNode(t, nodeConfig(t, mock, filepath.Join(base, "ws")))
	client := newTestClient(t, h)
	first, err := client.SendMessage(context.Background(), sendRequest("检查备份", false))
	require.NoError(t, err)
	task := first.(*a2a.Task)

	// Without the question's id, "which question is this an answer to?" has no answer beyond
	// "whatever is parked" — and a late answer would silently become the answer to the
	// CURRENT question. So the refusal hands back the id that IS open.
	resp, raw := post(t, "http://"+h.RemoteAddr()+restSendMessagePath, "application/a2a+json",
		taskMessageBody("msg-answer-1", task.ID, task.ContextID, nil,
			map[string]any{"text": "照片"}), nil)
	require.Equal(t, http.StatusConflict, resp.StatusCode, "body=%s", raw)
	require.Contains(t, raw, reasonQuestionMismatch)
	require.Contains(t, raw, errorDomain)
	require.Contains(t, raw, "ask_1", "the refusal must name the question that is open")
	require.NotContains(t, raw, a2a.ProtocolDomain)

	// And the task is untouched by the refused answer: still waiting, still the same question.
	got, err := client.GetTask(context.Background(), &a2a.GetTaskRequest{ID: task.ID})
	require.NoError(t, err)
	require.Equal(t, a2a.TaskStateInputRequired, got.Status.State)
	require.Len(t, mock.Requests(), 1, "a refused answer must not run anything")
}

func TestAnAnswerOfTheWrongShapeIsRefused(t *testing.T) {
	base := nodeHome(t)
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(mockllm.Step{Reply: mockllm.Stream(
		mockllm.ToolCallStart("ask_1", tools.ToolNameAskUser, askArgs(t, "检查照片还是文档？", "照片")),
		mockllm.Finish("tool_calls"), mockllm.Done(),
	)})

	h := startRealNode(t, nodeConfig(t, mock, filepath.Join(base, "ws")))
	client := newTestClient(t, h)
	first, err := client.SendMessage(context.Background(), sendRequest("检查备份", false))
	require.NoError(t, err)
	task := first.(*a2a.Task)

	// Right question, no content: a message that answers nothing cannot be delivered as an
	// answer (an empty answers map would tell the model the caller chose nothing).
	resp, raw := post(t, "http://"+h.RemoteAddr()+restSendMessagePath, "application/a2a+json",
		taskMessageBody("msg-answer-2", task.ID, task.ContextID,
			map[string]any{QuestionIDMetadataKey: "ask_1"}, map[string]any{"text": "   "}), nil)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "body=%s", raw)
	require.Contains(t, raw, reasonAnswerShape)
	require.Contains(t, raw, errorDomain)
}

func TestNewWorkInAParkedContextIsRefused(t *testing.T) {
	base := nodeHome(t)
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(mockllm.Step{Reply: mockllm.Stream(
		mockllm.ToolCallStart("ask_1", tools.ToolNameAskUser, askArgs(t, "检查照片还是文档？", "照片")),
		mockllm.Finish("tool_calls"), mockllm.Done(),
	)})

	h := startRealNode(t, nodeConfig(t, mock, filepath.Join(base, "ws")))
	client := newTestClient(t, h)
	first, err := client.SendMessage(context.Background(), sendRequest("检查备份", false))
	require.NoError(t, err)
	task := first.(*a2a.Task)

	// A parked context is NOT busy — its next message is the answer it waits for. But that
	// message has to NAME THE TASK: without one it would create a SECOND task in the same
	// context, and two turns in one conversation is what §7.1.1 refuses everywhere else.
	resp, raw := post(t, "http://"+h.RemoteAddr()+restSendMessagePath, "application/a2a+json",
		taskBody("msg-new-work", "another job", task.ContextID), nil)
	require.Equal(t, http.StatusConflict, resp.StatusCode, "body=%s", raw)
	require.Contains(t, raw, reasonContextParked)
	require.Contains(t, raw, errorDomain)
	require.Contains(t, raw, string(task.ID), "the refusal must name the task that is waiting")
	require.Contains(t, raw, "ask_1", "…and the question, so the caller can answer it")

	require.Len(t, listTasks(t, h), 1, "the refusal must not have created a task")
}

func TestAMessageNamingARunningTaskIsRefused(t *testing.T) {
	// The echo placeholder's parked executor keeps the task in flight for as long as the test
	// needs it; what is under test is the guard, not the work.
	parked := newParkedExecutor()
	h := startWith(t, t.TempDir(), func(o *Options) { o.Executor = parked })
	client := newTestClient(t, h)

	var runningID a2a.TaskID

	// Every task this test leaves parked is released and waited for before the test returns:
	// a background execution still writing into the data dir when the test ends would race the
	// temp-dir cleanup (and, at -race, turn a teardown into a failure).
	t.Cleanup(func() {
		close(parked.release)
		require.Eventually(t, func() bool {
			got, err := client.GetTask(context.Background(), &a2a.GetTaskRequest{ID: runningID})
			return err == nil && got.Status.State == a2a.TaskStateCompleted
		}, waitForWork, pollInterval)
	})

	result, err := client.SendMessage(context.Background(), sendRequest("long job", true))
	require.NoError(t, err)
	task := result.(*a2a.Task)
	runningID = task.ID
	<-parked.started

	resp, raw := post(t, "http://"+h.RemoteAddr()+restSendMessagePath, "application/a2a+json",
		taskMessageBody("msg-to-running", task.ID, task.ContextID, nil,
			map[string]any{"text": "are you done?"}), nil)
	require.Equal(t, http.StatusConflict, resp.StatusCode, "body=%s", raw)
	require.Contains(t, raw, reasonTaskBusy)
	require.Contains(t, raw, errorDomain)
}

func TestAMessageNamingAnUnknownTaskIsRefused(t *testing.T) {
	h := start(t, t.TempDir())

	resp, raw := post(t, "http://"+h.RemoteAddr()+restSendMessagePath, "application/a2a+json",
		taskMessageBody("msg-unknown", a2a.NewTaskID(), "", nil, map[string]any{"text": "hello"}), nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode, "body=%s", raw)
	require.Contains(t, raw, reasonUnknownTask)
	require.Contains(t, raw, errorDomain)
}

// ---- the deadline, and what a restart does with a parked task ----

func TestAQuestionNobodyAnswersEndsTheTask(t *testing.T) {
	base := nodeHome(t)
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(mockllm.Step{Reply: mockllm.Stream(
		mockllm.ToolCallStart("ask_1", tools.ToolNameAskUser, askArgs(t, "检查照片还是文档？", "照片")),
		mockllm.Finish("tool_calls"), mockllm.Done(),
	)})

	cfg := nodeConfig(t, mock, filepath.Join(base, "ws"))
	cfg.Node.QuestionTimeout = 100 * time.Millisecond
	h := startRealNode(t, cfg)
	client := newTestClient(t, h)

	first, err := client.SendMessage(context.Background(), sendRequest("检查备份", false))
	require.NoError(t, err)
	task := first.(*a2a.Task)
	require.Equal(t, a2a.TaskStateInputRequired, task.Status.State)

	// INPUT_REQUIRED means "a turn in this process is waiting for the answer". When the
	// deadline passes, nobody is — so the task must not keep claiming it is waiting: a caller
	// that polls it (or comes back tomorrow) is told it is over.
	require.Eventually(t, func() bool {
		got, err := client.GetTask(context.Background(), &a2a.GetTaskRequest{ID: task.ID})
		return err == nil && got.Status.State == a2a.TaskStateFailed
	}, waitForWork, pollInterval)

	got, err := client.GetTask(context.Background(), &a2a.GetTaskRequest{ID: task.ID})
	require.NoError(t, err)
	require.Contains(t, messageText(got.Status.Message), "检查照片还是文档？",
		"the record still says what was asked — 'nobody answered' is only useful with the question")
	require.NotEmpty(t, got.Status.Message.Parts)
}

func TestAParkedTaskIsResolvedAsInterruptedWhenTheProcessGoesAway(t *testing.T) {
	base := nodeHome(t)
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(mockllm.Step{Reply: mockllm.Stream(
		mockllm.ToolCallStart("ask_1", tools.ToolNameAskUser, askArgs(t, "检查照片还是文档？", "照片")),
		mockllm.Finish("tool_calls"), mockllm.Done(),
	)})

	cfg := nodeConfig(t, mock, filepath.Join(base, "ws"))
	h := startRealNode(t, cfg)
	client := newTestClient(t, h)
	first, err := client.SendMessage(context.Background(), sendRequest("检查备份", false))
	require.NoError(t, err)
	task := first.(*a2a.Task)
	require.Equal(t, a2a.TaskStateInputRequired, task.Status.State)

	// The process goes away with the question open. The goroutine that was waiting for the
	// answer did not survive it, so the next start must not pretend the answer can still
	// arrive (§7.4) — and it keeps the question (§9.3).
	require.NoError(t, h.Close())

	store, err := openTaskStore(filepath.Join(config.A2ADir(), tasksDirName))
	require.NoError(t, err)
	rec, ok := store.record(task.ID)
	require.True(t, ok)
	require.Equal(t, a2a.TaskStateFailed, rec.Task.Status.State)
	require.Contains(t, messageText(rec.Task.Status.Message), interruptReason)
	require.Contains(t, messageText(rec.Task.Status.Message), "检查照片还是文档？",
		"the question has to survive the restart with the task")
}

func TestCancelingAParkedTaskEndsItAndItsQuestion(t *testing.T) {
	base := nodeHome(t)
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(mockllm.Step{Reply: mockllm.Stream(
		mockllm.ToolCallStart("ask_1", tools.ToolNameAskUser, askArgs(t, "检查照片还是文档？", "照片")),
		mockllm.Finish("tool_calls"), mockllm.Done(),
	)})

	h := startRealNode(t, nodeConfig(t, mock, filepath.Join(base, "ws")))
	client := newTestClient(t, h)
	first, err := client.SendMessage(context.Background(), sendRequest("检查备份", false))
	require.NoError(t, err)
	task := first.(*a2a.Task)
	require.Equal(t, a2a.TaskStateInputRequired, task.Status.State)

	canceled, err := client.CancelTask(context.Background(), &a2a.CancelTaskRequest{ID: task.ID})
	require.NoError(t, err)
	require.Equal(t, a2a.TaskStateCanceled, canceled.Status.State,
		"a parked turn is the one a cancel can only end from here: the SDK's execution for it is over")

	// The answer arrives after the cancel. The task is finished, so nothing can revive it —
	// and the question it was carrying is gone with it rather than being delivered to a loop
	// that no longer exists.
	resp, raw := post(t, "http://"+h.RemoteAddr()+restSendMessagePath, "application/a2a+json",
		taskMessageBody("msg-late-answer", task.ID, task.ContextID,
			map[string]any{QuestionIDMetadataKey: "ask_1"}, map[string]any{"text": "照片"}), nil)
	require.Equal(t, http.StatusConflict, resp.StatusCode, "body=%s", raw)
	require.Contains(t, raw, reasonTaskFinished)
	require.Contains(t, raw, errorDomain)
}

// ---- the line between a question and a tool authorization ----

func TestAnAnsweredQuestionNeverApprovesATool(t *testing.T) {
	base := nodeHome(t)
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(
		mockllm.Step{Reply: mockllm.Stream(
			mockllm.ToolCallStart("call_bash", tools.ToolNameBash, `{"command":"echo ran-without-approval"}`),
			mockllm.Finish("tool_calls"),
			mockllm.Done(),
		)},
		mockllm.Step{
			// The next call is served only if the model was told the command was REFUSED —
			// if it had run, the tool result would be the command's output instead and this
			// requirement would fail the request.
			Require: mockllm.HasToolResult("call_bash", contains("permission denied")),
			Reply:   mockllm.Stream(mockllm.Text("没有权限，我不执行。"), mockllm.Finish("stop"), mockllm.Done()),
		},
	)

	cfg := nodeConfig(t, mock, filepath.Join(base, "ws"))
	// A command that REQUIRES approval. There is no human attached to a remote task, so the
	// ask is a refusal — and the caller's answer to a question (§7.4) is never what turns it
	// into an approval, because a tool authorization never becomes a question at all.
	cfg.Permissions.Bash.Ask = []string{"echo *"}
	h := startRealNode(t, cfg)

	result, err := newTestClient(t, h).SendMessage(context.Background(), sendRequest("跑一个命令", false))
	require.NoError(t, err)
	task := result.(*a2a.Task)
	require.Equal(t, a2a.TaskStateCompleted, task.Status.State,
		"the turn finishes with the refusal; a tool authorization must not become a question "+
			"the caller can answer, which would park it in INPUT_REQUIRED")
	require.Equal(t, "没有权限，我不执行。", firstPartText(t, task.Artifacts[0]))
	require.NoError(t, mock.Error(), "the scripted requirement tells whether the command was run")
}

// ---- helpers for the bodies these tests send by hand ----

// taskMessageBody is a message/send body for a message that NAMES a task — the shape the
// answer path and every refusal around it are about.
func taskMessageBody(messageID string, taskID a2a.TaskID, contextID string, metadata map[string]any, parts ...map[string]any) string {
	msg := map[string]any{
		"messageId": messageID,
		"taskId":    string(taskID),
		"role":      "ROLE_USER",
		"parts":     parts,
	}
	if contextID != "" {
		msg["contextId"] = contextID
	}
	if metadata != nil {
		msg["metadata"] = metadata
	}
	return marshalBody(msg)
}

// taskBody is the same for a message that names only a context.
func taskBody(messageID, text, contextID string) string {
	return marshalBody(map[string]any{
		"messageId": messageID,
		"contextId": contextID,
		"role":      "ROLE_USER",
		"parts":     []map[string]any{{"text": text}},
	})
}

func marshalBody(msg map[string]any) string {
	raw, err := json.Marshal(map[string]any{
		"message":       msg,
		"configuration": map[string]any{"returnImmediately": true},
	})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// listTasks is what this node is holding: the store's own snapshot, because the SDK's list
// refuses a caller it cannot identify (and these bodies go out without an identity).
func listTasks(t *testing.T, h *Handle) []*taskRecord {
	t.Helper()
	return h.store.snapshot()
}
