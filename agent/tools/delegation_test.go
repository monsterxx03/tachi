package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// The two delegation tools, against a stand-in for this machine's sending side. What is under
// test here is the CONTRACT with the model: which arguments are refused rather than guessed at,
// what a result looks like, and that the conversation a tool was built with — never a model
// argument — is what its work is filed under.

type fakeDelegator struct {
	peers []PeerSummary

	delegations []DelegationRequest
	gets        []string
	waits       []string
	replies     []fakeReply
	cancels     []string
	lists       []string

	// view is what every call answers with, and err what it fails with. A call may carry BOTH: a
	// refusal that still names the task is exactly the shape a caller has to handle.
	view RemoteTaskInfo
	err  error
}

type fakeReply struct {
	ID         string
	QuestionID string
	Answer     map[string]string
}

func (f *fakeDelegator) Peers(context.Context) []PeerSummary { return f.peers }

func (f *fakeDelegator) Delegate(_ context.Context, req DelegationRequest) (RemoteTaskInfo, error) {
	f.delegations = append(f.delegations, req)
	return f.view, f.err
}

func (f *fakeDelegator) Get(_ context.Context, id string) (RemoteTaskInfo, error) {
	f.gets = append(f.gets, id)
	return f.view, f.err
}

func (f *fakeDelegator) Wait(_ context.Context, id string, _ time.Duration) (RemoteTaskInfo, error) {
	f.waits = append(f.waits, id)
	return f.view, f.err
}

func (f *fakeDelegator) Reply(_ context.Context, id, questionID string, answer map[string]string, _ time.Duration) (RemoteTaskInfo, error) {
	f.replies = append(f.replies, fakeReply{ID: id, QuestionID: questionID, Answer: answer})
	return f.view, f.err
}

func (f *fakeDelegator) Cancel(_ context.Context, id string) (RemoteTaskInfo, error) {
	f.cancels = append(f.cancels, id)
	return f.view, f.err
}

func (f *fakeDelegator) List(logicalID string) []RemoteTaskInfo {
	f.lists = append(f.lists, logicalID)
	return []RemoteTaskInfo{f.view}
}

func decodeResult(t *testing.T, raw string) RemoteTaskInfo {
	t.Helper()
	var info RemoteTaskInfo
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		t.Fatalf("a tool result must be a task object, got %q: %v", raw, err)
	}
	return info
}

func TestDelegateTaskFilesTheWorkUnderTheToolsConversation(t *testing.T) {
	fake := &fakeDelegator{view: RemoteTaskInfo{ID: "d_7f3a1c02", Node: "home-nas", Title: "检查备份", State: "working"}}
	tool := NewDelegateTaskTool(fake)
	ctx := WithLogicalConversation(t.Context(), "conv-1")

	out, err := tool.ExecuteContext(ctx,
		`{"node":"home-nas","prompt":"检查昨天的备份是否完整，返回异常清单，不修改文件。"}`)
	if err != nil {
		t.Fatalf("ExecuteContext: %v", err)
	}

	if len(fake.delegations) != 1 {
		t.Fatalf("expected one delegation, got %d", len(fake.delegations))
	}
	req := fake.delegations[0]
	if req.Peer != "home-nas" || !strings.Contains(req.Prompt, "不修改文件") {
		t.Errorf("the request must carry what the model wrote: %+v", req)
	}
	// The conversation comes from the RUN, never from the model: a model that could name it
	// would be able to file work under another conversation (§8.5).
	if req.LogicalID != "conv-1" {
		t.Errorf("logical id = %q, want the tool's", req.LogicalID)
	}

	info := decodeResult(t, out)
	if info.ID != "d_7f3a1c02" || info.State != "working" {
		t.Errorf("unexpected result: %+v", info)
	}
}

func TestDelegateTaskRefusesAmbiguousTargets(t *testing.T) {
	tool := NewDelegateTaskTool(&fakeDelegator{})
	ctx := WithLogicalConversation(t.Context(), "conv-1")

	for _, tc := range []struct {
		name string
		args string
		want string
	}{
		{"both targets", `{"node":"nas","continue_from":"d_1","prompt":"x"}`, "not both"},
		{"no target", `{"prompt":"x"}`, `"node"`},
		{"empty prompt", `{"node":"nas","prompt":"   "}`, "needs a prompt"},
		{"bad json", `{`, "invalid arguments"},
	} {
		if _, err := tool.ExecuteContext(ctx, tc.args); err == nil {
			t.Errorf("%s: expected a refusal", tc.name)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: refusal %q does not mention %q", tc.name, err.Error(), tc.want)
		}
	}
}

func TestAFailureThatStillNamesTheTaskIsReportedAsData(t *testing.T) {
	// A refusal that carries the task id (a busy peer, an unknown outcome) must reach the model
	// as a result it can act on: a bare error would make it delegate a second time.
	fake := &fakeDelegator{
		view: RemoteTaskInfo{ID: "d_9e11", Node: "nas", State: "unknown"},
		err:  errors.New("node: nas could not be reached"),
	}
	tool := NewDelegateTaskTool(fake)
	ctx := WithLogicalConversation(t.Context(), "conv-1")

	out, err := tool.ExecuteContext(ctx, `{"node":"nas","prompt":"检查备份"}`)
	if err != nil {
		t.Fatalf("a named task must come back as data, got error %v", err)
	}
	info := decodeResult(t, out)
	if info.ID != "d_9e11" {
		t.Errorf("the id is what the model needs next: %+v", info)
	}
	if !strings.Contains(info.Note, "could not be reached") {
		t.Errorf("the reason has to travel with the record: %+v", info)
	}
}

func TestAFailureWithNoTaskIsAnError(t *testing.T) {
	tool := NewDelegateTaskTool(&fakeDelegator{err: errors.New("node: peer \"nobody\" is not configured")})
	ctx := WithLogicalConversation(t.Context(), "conv-1")
	if _, err := tool.ExecuteContext(ctx, `{"node":"nobody","prompt":"x"}`); err == nil {
		t.Fatal("an unknown peer produces no task, so it must be an error")
	}
}

func TestRemoteTaskRequiresATaskIDForEverythingButList(t *testing.T) {
	fake := &fakeDelegator{view: RemoteTaskInfo{ID: "d_1", State: "working"}}
	tool := NewRemoteTaskTool(fake, time.Second)
	ctx := WithLogicalConversation(t.Context(), "conv-1")

	for _, action := range []string{"get", "wait", "cancel", "reply"} {
		if _, err := tool.ExecuteContext(ctx, `{"action":"`+action+`"}`); err == nil {
			t.Errorf("%s without a task_id must be refused", action)
		}
	}
	if _, err := tool.ExecuteContext(ctx, `{"action":"frobnicate","task_id":"d_1"}`); err == nil {
		t.Error("an unknown action must be refused, not guessed at")
	}
	// list needs no id and is filed under the tool's conversation.
	if _, err := tool.ExecuteContext(ctx, `{"action":"list"}`); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(fake.lists) != 1 || fake.lists[0] != "conv-1" {
		t.Errorf("list must be scoped to this conversation, got %v", fake.lists)
	}
}

func TestRemoteTaskReplyNamesTheQuestionAndTheAnswer(t *testing.T) {
	fake := &fakeDelegator{view: RemoteTaskInfo{ID: "d_1", State: "working"}}
	tool := NewRemoteTaskTool(fake, time.Second)
	ctx := WithLogicalConversation(t.Context(), "conv-1")

	// One question: a plain answer.
	if _, err := tool.ExecuteContext(ctx,
		`{"action":"reply","task_id":"d_1","question_id":"ask_1","answer":"照片"}`); err != nil {
		t.Fatalf("reply: %v", err)
	}
	// Several: the answers are NAMED, which is what the receiving side needs to map them back.
	// (The wiring of a single answer onto one question is the sending side's job — see node's
	// answerParts, which is where the two spellings must agree.)
	if _, err := tool.ExecuteContext(ctx,
		`{"action":"reply","task_id":"d_1","question_id":"ask_2","answers":{"哪个目录？":"照片"}}`); err != nil {
		t.Fatalf("reply with named answers: %v", err)
	}

	if len(fake.replies) != 2 {
		t.Fatalf("expected two replies, got %d", len(fake.replies))
	}
	if fake.replies[0].QuestionID != "ask_1" || fake.replies[0].Answer["answer"] != "照片" {
		t.Errorf("the question has to travel with the answer: %+v", fake.replies[0])
	}
	if fake.replies[1].Answer["哪个目录？"] != "照片" {
		t.Errorf("named answers must pass through unchanged: %+v", fake.replies[1])
	}

	// Both spellings at once is ambiguous, and neither answers anything.
	for _, args := range []string{
		`{"action":"reply","task_id":"d_1","answer":"a","answers":{"q":"a"}}`,
		`{"action":"reply","task_id":"d_1"}`,
	} {
		if _, err := tool.ExecuteContext(ctx, args); err == nil {
			t.Errorf("expected a refusal for %s", args)
		}
	}
}

func TestRemoteTaskListCanBeNarrowedToOneNode(t *testing.T) {
	fake := &fakeDelegator{view: RemoteTaskInfo{ID: "d_1", Node: "nas", State: "completed"}}
	tool := NewRemoteTaskTool(fake, time.Second)
	ctx := WithLogicalConversation(t.Context(), "conv-1")

	out, err := tool.ExecuteContext(ctx, `{"action":"list","node":"nas"}`)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var page struct {
		Tasks []RemoteTaskInfo `json:"tasks"`
		Count int              `json:"count"`
	}
	if err := json.Unmarshal([]byte(out), &page); err != nil {
		t.Fatalf("list result: %v", err)
	}
	if page.Count != 1 || len(page.Tasks) != 1 {
		t.Errorf("unexpected page: %+v", page)
	}

	out, err = tool.ExecuteContext(ctx, `{"action":"list","node":"elsewhere"}`)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if err := json.Unmarshal([]byte(out), &page); err != nil {
		t.Fatal(err)
	}
	if page.Count != 0 {
		t.Errorf("a node filter must actually filter: %+v", page)
	}
}

func TestTheDelegationToolsAreNotInTheRegistry(t *testing.T) {
	// They are attached per run (see agent.WithExtraTools) rather than registered, and that is
	// what keeps them out of turns that must not delegate at all (§10.2.1): a cron turn gets no
	// delegator to build them with, and no amount of reloading can resurrect one.
	reg := NewRegistry()
	if reg.GetTool(ToolNameDelegateTask) != nil || reg.GetTool(ToolNameRemoteTask) != nil {
		t.Fatal("delegation tools must never be part of the default registry")
	}
}
