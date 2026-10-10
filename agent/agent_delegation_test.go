package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/agent/systemreminder"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/llm"
)

// Delegation through a real run: the reminders are registered by the same path every agent
// builds its collector with, they describe only what the RUN was given, and what they say
// reaches the model.

// delegationProvider is a stand-in for this machine's sending side. The two methods these tests
// exercise are real; the rest exist because the run option takes the WHOLE contract — a run
// either talks to a sending side or to nothing, and a double that implemented only half of it
// would be testing a shape no caller has.
type delegationProvider struct {
	tasks []tools.RemoteTaskInfo
	peers []tools.PeerSummary
}

func (p *delegationProvider) RemoteTasks(string) []tools.RemoteTaskInfo { return p.tasks }
func (p *delegationProvider) Peers(context.Context) []tools.PeerSummary { return p.peers }

func (p *delegationProvider) Delegate(context.Context, tools.DelegationRequest) (tools.RemoteTaskInfo, error) {
	return tools.RemoteTaskInfo{}, nil
}

func (p *delegationProvider) Get(context.Context, string) (tools.RemoteTaskInfo, error) {
	return tools.RemoteTaskInfo{}, nil
}

func (p *delegationProvider) Wait(context.Context, string, time.Duration) (tools.RemoteTaskInfo, error) {
	return tools.RemoteTaskInfo{}, nil
}

func (p *delegationProvider) Reply(context.Context, string, string, map[string]string, time.Duration) (tools.RemoteTaskInfo, error) {
	return tools.RemoteTaskInfo{}, nil
}

func (p *delegationProvider) Cancel(context.Context, string) (tools.RemoteTaskInfo, error) {
	return tools.RemoteTaskInfo{}, nil
}

// recordingProvider answers one scripted turn and keeps what the model was sent.
type recordingProvider struct {
	sequences [][]llm.StreamEvent
	calls     int
	sent      []string
}

func (p *recordingProvider) Name() string         { return "recording" }
func (p *recordingProvider) ProviderName() string { return "recording" }
func (p *recordingProvider) Model() string        { return "mock-model" }

// CreateChat is the title generator's path (a brand-new session gets one). It answers with a
// fixed title rather than nil: a nil response is a panic there, and this test is not about titles.
func (p *recordingProvider) CreateChat(context.Context, []llm.Message, []llm.Tool, llm.ChatOptions) (*llm.Response, error) {
	return &llm.Response{Content: "标题"}, nil
}

func (p *recordingProvider) CreateChatStream(_ context.Context, messages []llm.Message, _ []llm.Tool, _ llm.ChatOptions) (<-chan llm.StreamEvent, error) {
	var sent strings.Builder
	for _, m := range messages {
		sent.WriteString(m.Content)
		sent.WriteString("\n")
	}
	p.sent = append(p.sent, sent.String())

	ch := make(chan llm.StreamEvent, len(p.sequences[0])+1)
	if p.calls < len(p.sequences) {
		for _, ev := range p.sequences[p.calls] {
			ch <- ev
		}
	}
	p.calls++
	close(ch)
	return ch, nil
}

func (p *recordingProvider) lastSent() string {
	if len(p.sent) == 0 {
		return ""
	}
	return p.sent[len(p.sent)-1]
}

func TestTheDelegationRemindersAreRegisteredAndFollowTheRun(t *testing.T) {
	// The collector is built by the same call every agent uses, so this is the registration the
	// real thing has — not a hand-assembled list that could drift from it.
	a := newTestAgent(t, &recordingProvider{})
	a.buildReminderCollectorFrom(SystemReminderConfig{})

	provider := &delegationProvider{
		peers: []tools.PeerSummary{{Name: "nas", Description: "家里的 NAS", Reachable: true}},
		tasks: []tools.RemoteTaskInfo{{ID: "d_1", Node: "nas", Title: "检查备份", State: "working"}},
	}

	// A run that was not given a sending side is told NOTHING about delegation: that is the gate
	// (§10.2.1), and it is the run's delegator that decides it, not the collector.
	bare := a.Config.ReminderCollector.CollectPieces(t.Context(), systemreminder.Context{
		LogicalConversation: "conv-1",
	})
	for _, p := range bare {
		if strings.Contains(strings.Join(p.Lines, "\n"), "可以委派工作的节点") ||
			strings.Contains(strings.Join(p.Lines, "\n"), "远程任务") {
			t.Errorf("a run without a delegator must not be told about delegation: %v", p.Lines)
		}
	}

	// Given one, both fire: which nodes exist, and what this conversation has out.
	withDelegation := a.Config.ReminderCollector.CollectPieces(t.Context(), systemreminder.Context{
		Delegator: provider, LogicalConversation: "conv-1",
	})
	joined := ""
	for _, p := range withDelegation {
		joined += strings.Join(p.Lines, "\n") + "\n"
	}
	if !strings.Contains(joined, "可以委派工作的节点") || !strings.Contains(joined, "- nas：家里的 NAS") {
		t.Errorf("the peers reminder must reach the collector:\n%s", joined)
	}
	if !strings.Contains(joined, "d_1 | nas | 检查备份") {
		t.Errorf("the tasks reminder must reach the collector:\n%s", joined)
	}
}

func TestARunGivenDelegationCarriesTheReminderToTheModel(t *testing.T) {
	provider := &delegationProvider{
		peers: []tools.PeerSummary{{Name: "nas", Description: "家里的 NAS", Reachable: true}},
	}
	runner := &recordingProvider{sequences: [][]llm.StreamEvent{textSeq("好")}}
	a := newTestAgent(t, runner, withFakeSession())
	a.buildReminderCollectorFrom(SystemReminderConfig{})

	events := a.RunConversationStream(t.Context(), nil, "让 nas 查一下备份", "", llm.ChatOptions{},
		WithDelegation(provider))
	drainAgentEvents(events)

	sent := runner.lastSent()
	if !strings.Contains(sent, "可以委派工作的节点") {
		t.Errorf("a run that may delegate must be told which nodes exist:\n%s", sent)
	}

	// The same turn WITHOUT the option: the model is not told, and — because the tools travel as
	// that option's extras — it also has no way to act on the information. One knob, one gate.
	runner2 := &recordingProvider{sequences: [][]llm.StreamEvent{textSeq("好")}}
	b := newTestAgent(t, runner2, withFakeSession())
	b.buildReminderCollectorFrom(SystemReminderConfig{})

	events = b.RunConversationStream(t.Context(), nil, "你好", "", llm.ChatOptions{})
	drainAgentEvents(events)
	if got := runner2.lastSent(); strings.Contains(got, "可以委派工作的节点") {
		t.Errorf("a run without delegation must not be told about nodes:\n%s", got)
	}
}
