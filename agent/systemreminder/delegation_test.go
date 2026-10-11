package systemreminder

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/agent/tools"
)

// The delegation reminders: what a conversation already asked elsewhere, and which nodes it may
// ask. Both are silent unless the RUN has a sending side — that gate is what keeps a cron or
// ambient turn from being told about delegation at all (§10.2.1).

type fakeProvider struct {
	tasks   []tools.RemoteTaskInfo
	peers   []tools.PeerSummary
	asked   []string
	peerHit int
}

func (f *fakeProvider) RemoteTasks(conversation string) []tools.RemoteTaskInfo {
	f.asked = append(f.asked, conversation)
	return f.tasks
}

func (f *fakeProvider) Peers(context.Context) []tools.PeerSummary {
	f.peerHit++
	return f.peers
}

func task(id, node, title, state string) tools.RemoteTaskInfo {
	return tools.RemoteTaskInfo{ID: id, Node: node, Title: title, State: state, LastSeen: time.Now()}
}

func TestTheTaskReminderIsSilentWithoutARunThatCanDelegate(t *testing.T) {
	provider := &fakeProvider{tasks: []tools.RemoteTaskInfo{task("d_1", "nas", "检查备份", "working")}}
	reminder := RemoteTasksReminder{}

	if got := reminder.Generate(t.Context(), Context{LogicalConversation: "conv-1"}); got != nil {
		t.Errorf("no delegator means no reminder, got %v", got)
	}
	// A run with a delegator but NO conversation is in the sessionless scope, and that scope is
	// described like any other: it is what its own RemoteTask(list) shows.
	if got := reminder.Generate(t.Context(), Context{Delegator: provider}); len(got) == 0 {
		t.Error("the sessionless scope has its own work, and must be told about it")
	}
	if len(provider.asked) != 1 || provider.asked[0] != "" {
		t.Errorf("the scope is asked for as it is (empty included): %v", provider.asked)
	}
}

func TestTheTaskReminderPutsTheQuestionFirst(t *testing.T) {
	provider := &fakeProvider{tasks: []tools.RemoteTaskInfo{
		task("d_3", "nas", "更新的活儿", "working"),
		{ID: "d_2", Node: "nas", Title: "要不要改文件", State: "input_required",
			QuestionID: "ask_7", QuestionText: "要不要改文件？"},
		task("d_1", "vps", "旧活儿", "completed"),
	}}
	lines := RemoteTasksReminder{}.Generate(t.Context(), Context{
		Delegator: provider, LogicalConversation: "conv-1",
	})
	if len(lines) == 0 {
		t.Fatal("a conversation with delegated work must be told about it")
	}
	body := strings.Join(lines, "\n")

	// A waiting question is the only item here that needs the model to ACT, so it leads.
	if idxTask, idxWorking := strings.Index(body, "d_2"), strings.Index(body, "d_3"); idxTask > idxWorking {
		t.Errorf("a waiting task must be listed first:\n%s", body)
	}
	if !strings.Contains(body, "question_id=ask_7") {
		t.Errorf("the question id is what an answer must name:\n%s", body)
	}
	if !strings.Contains(body, "等待回答") || !strings.Contains(body, "已完成") {
		t.Errorf("states must read as states:\n%s", body)
	}
	if !strings.Contains(body, "未列出的任务不等于已结束") {
		t.Errorf("the reminder must not let absence read as deletion:\n%s", body)
	}
	if provider.asked[0] != "conv-1" {
		t.Errorf("the reminder must ask for THIS conversation, got %q", provider.asked[0])
	}
}

func TestTheTaskReminderIsBoundedAndSaysHowToSeeTheRest(t *testing.T) {
	provider := &fakeProvider{}
	for i := range 8 {
		provider.tasks = append(provider.tasks, task(string(rune('a'+i)), "nas", "活儿", "working"))
	}
	lines := RemoteTasksReminder{Max: 3}.Generate(t.Context(), Context{
		Delegator: provider, LogicalConversation: "conv-1",
	})
	if len(lines) != 6 { // header + 3 tasks + the count + the how-to
		t.Fatalf("expected a bounded list, got %d lines:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	if !strings.Contains(strings.Join(lines, "\n"), "还有 5 项未列出") {
		t.Errorf("truncation has to be counted, not silent:\n%s", strings.Join(lines, "\n"))
	}
}

// The reminder GROUPS (waiting first) but never re-sorts within a group: the order it renders is
// the order its provider gave it, which is "newest first" and — since node.Sender's store breaks
// ties by id — a total order. So the block a turn sends is the same block the next turn would,
// and a reminder that has not changed is not re-appended as if it had (see the collector's dedup).
func TestTheTaskReminderKeepsTheProvidersOrderInsideAGroup(t *testing.T) {
	// Provider order, as a list is defined to give it: newest first.
	provider := &fakeProvider{tasks: []tools.RemoteTaskInfo{
		task("d_new", "nas", "最新的一件", "working"),
		task("d_mid", "nas", "中间那件", "completed"),
		task("d_old", "vps", "最早的一件", "failed"),
	}}
	lines := RemoteTasksReminder{}.Generate(t.Context(), Context{
		Delegator: provider, LogicalConversation: "conv-1",
	})
	body := strings.Join(lines, "\n")
	newIdx, midIdx, oldIdx := strings.Index(body, "d_new"), strings.Index(body, "d_mid"), strings.Index(body, "d_old")
	if newIdx < 0 || midIdx < 0 || oldIdx < 0 {
		t.Fatalf("every task has to be listed:\n%s", body)
	}
	if newIdx >= midIdx || midIdx >= oldIdx {
		t.Errorf("within a group the provider's order is the order shown:\n%s", body)
	}

	// And the same input renders the same block twice — nothing here is keyed by a map.
	again := strings.Join(RemoteTasksReminder{}.Generate(t.Context(), Context{
		Delegator: provider, LogicalConversation: "conv-1",
	}), "\n")
	if again != body {
		t.Errorf("the same list must render the same block:\nfirst:\n%s\nsecond:\n%s", body, again)
	}
}

func TestThePeersReminderSpeaksOncePerConversation(t *testing.T) {
	provider := &fakeProvider{peers: []tools.PeerSummary{
		{Name: "nas", Description: "家里的 NAS", Reachable: true},
		{Name: "vps", Note: "香港的小机器", Reachable: false},
	}}
	reminder := &PeersReminder{}
	rctx := Context{Delegator: provider, LogicalConversation: "conv-1"}

	first := reminder.Generate(t.Context(), rctx)
	if len(first) == 0 {
		t.Fatal("a run that may delegate must be told which nodes exist")
	}
	body := strings.Join(first, "\n")
	if !strings.Contains(body, "nas：家里的 NAS") {
		t.Errorf("a reachable node is described by its own card:\n%s", body)
	}
	if !strings.Contains(body, "vps：香港的小机器（上次联系不上") {
		t.Errorf("an unreachable node says so, and falls back to the configured note:\n%s", body)
	}

	// Unchanged: telling it again would only spend tokens on what the model already read.
	if again := reminder.Generate(t.Context(), rctx); again != nil {
		t.Errorf("an unchanged peer list must stay quiet, got %v", again)
	}

	// A node added is a change the model has to hear.
	provider.peers = append(provider.peers, tools.PeerSummary{Name: "buildbox", Description: "编译机", Reachable: true})
	if changed := reminder.Generate(t.Context(), rctx); len(changed) == 0 {
		t.Error("a changed peer list must be told again")
	}

	// Another conversation is told for the first time, even with the same list: the memory is
	// per conversation.
	if other := reminder.Generate(t.Context(), Context{Delegator: provider, LogicalConversation: "conv-2"}); len(other) == 0 {
		t.Error("another conversation must be told once too")
	}
}

func TestThePeersReminderIsSilentWithNoPeersOrNoDelegator(t *testing.T) {
	reminder := &PeersReminder{}
	if got := reminder.Generate(t.Context(), Context{LogicalConversation: "conv-1"}); got != nil {
		t.Errorf("no delegator means no peers reminder, got %v", got)
	}
	// A run with no conversation is still told which nodes exist: the list is a fact about the
	// MACHINE, and a pipe run has the tool but no other way to learn a node's name.
	withPeers := &fakeProvider{peers: []tools.PeerSummary{{Name: "nas", Reachable: true}}}
	if got := reminder.Generate(t.Context(), Context{Delegator: withPeers}); len(got) == 0 {
		t.Error("a sessionless run must be told which nodes it may delegate to")
	}
	empty := &fakeProvider{}
	if got := reminder.Generate(t.Context(), Context{Delegator: empty, LogicalConversation: "conv-1"}); got != nil {
		t.Errorf("no configured peers means nothing to say, got %v", got)
	}
	if empty.peerHit != 1 {
		t.Errorf("the provider is asked whether there are peers: hits=%d", empty.peerHit)
	}
}

// The reminder shows the identity beside the name, and the node's own skills after it.
//
// The identity is there because it is the one fact about a peer nobody else can claim: it makes a
// name the reader is unsure about checkable, and it is a second way to name that node in a tool
// argument. The skills are the node's OWN claim about what it can do — which is how they are worded
// ("它自称会") — and they are capped, because a reminder that grows a paragraph per node is paid for
// on every turn of every conversation that can delegate.
func TestThePeersReminderShowsIdentityAndAdvertisedSkills(t *testing.T) {
	provider := &fakeProvider{peers: []tools.PeerSummary{{
		Name:        "nas",
		NodeID:      "cd726b446c34386ece70d88fdd5fbf2a",
		Description: "跑在家里的机器",
		Skills:      []string{"a", "b", "c", "d", "e", "f", "g"},
		Reachable:   true,
	}}}
	reminder := &PeersReminder{}

	lines := reminder.Generate(t.Context(), Context{Delegator: provider, LogicalConversation: "conv-1"})
	body := strings.Join(lines, "\n")

	if !strings.Contains(body, "nas (cd726b44)") {
		t.Errorf("the identity has to sit beside the name, short enough to read:\n%s", body)
	}
	if !strings.Contains(body, "它自称会：a、b、c、d、e、f 等 7 项") {
		t.Errorf("the node's advertised skills are its claim, and the list is capped:\n%s", body)
	}

	// A node this machine has no identity for says nothing about one, and a node with no skills does
	// not grow an empty label.
	provider.peers = []tools.PeerSummary{{Name: "vps", Reachable: false}}
	lines = reminder.Generate(t.Context(), Context{Delegator: provider, LogicalConversation: "conv-2"})
	body = strings.Join(lines, "\n")
	if strings.Contains(body, "(") || strings.Contains(body, "自称会") {
		t.Errorf("nothing to say about identity or skills means saying nothing:\n%s", body)
	}
}
