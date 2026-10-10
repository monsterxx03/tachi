package node

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/creasty/defaults"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/session"
)

// A remote task, end to end and for real: a message arrives over HTTP, a Tachi turn runs
// against a scripted model, and what comes back is the turn's answer. Nothing here is a
// placeholder — the executor is the one the node actually uses, and the session it writes
// is in the same store the local frontends read.
//
// The provider is mocked (a real model has no place in a unit test); everything else is
// the node's own code path.

// nodeConfig is a node's configuration for these tests: one provider pointed at the
// scripted model, plus the node's own switch, workspace and tool allowlist.
func nodeConfig(t *testing.T, mock *mockllm.Server, workspace string, tools ...string) *config.Config {
	t.Helper()
	cfg := &config.Config{Language: "zh"}
	cfg.Providers = []config.ProviderConfig{{
		Name:    "mock",
		Type:    mock.Protocol().String(),
		Model:   "mock-model",
		BaseURL: mock.BaseURL(),
		APIKey:  "test-key",
		// Without a context window the agent reads the budget as zero and auto-compacts
		// before the first turn is even sent — an extra model call the script does not have
		// (which is how this was found: the "hang" was a retry loop against a 500).
		Spec: config.ModelSpec{ContextWindow: ptr(int64(128000))},
	}}
	// Defaults are not decoration: `defaults.Set` is what config.Load() does for every
	// real process, and a hand-built Config without them is a config nobody has. (Left
	// out, Compact.Threshold stays 0 and the agent auto-compacts before EVERY turn — which
	// looks exactly like a hang.)
	if err := defaults.Set(cfg); err != nil {
		t.Fatalf("defaults.Set: %v", err)
	}
	cfg.Node.Enabled = true
	cfg.Node.Workspace = workspace
	cfg.Node.Tools = tools
	return cfg
}

// nodeHome puts the node's data dir (and the session store) inside a temp dir, so a test
// never touches the real ~/.tachi.
func nodeHome(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	config.SetBaseDir(base)
	t.Cleanup(func() { config.SetBaseDir("") })
	return base
}

func startRealNode(t *testing.T, cfg *config.Config) *Handle {
	t.Helper()
	h, err := StartOrAttachNode(context.Background(), cfg)
	require.NoError(t, err)
	require.NotNil(t, h, "an enabled node must produce a handle")
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	return h
}

func oneMessageScript(text string) []mockllm.Step {
	return []mockllm.Step{{Reply: mockllm.Stream(
		mockllm.Text(text),
		mockllm.Finish("stop"),
		mockllm.Done(),
	)}}
}

func TestRemoteTaskRunsARealTurnInTheNodesWorkspace(t *testing.T) {
	base := nodeHome(t)
	workspace := filepath.Join(base, "workspace")

	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(oneMessageScript("备份检查完成：两个目录异常")...)

	h := startRealNode(t, nodeConfig(t, mock, workspace, "ReadFile", "Grep", "Glob"))
	client := newTestClient(t, h)

	result, err := client.SendMessage(context.Background(), sendRequest("检查昨天的备份", false))
	require.NoError(t, err)
	task, ok := result.(*a2a.Task)
	require.True(t, ok)
	require.Equal(t, a2a.TaskStateCompleted, task.Status.State)
	require.Len(t, task.Artifacts, 1)
	require.Equal(t, "备份检查完成：两个目录异常", firstPartText(t, task.Artifacts[0]),
		"the artifact is the turn's answer, not the streaming deltas")

	// The workspace is created on first use — a node that never gets a task touches nothing.
	require.DirExists(t, workspace)

	// The turn ran in a session on THIS node's workspace, in the store the local frontends
	// read, and it is marked as a remote conversation the memory consolidation skips.
	sm, err := session.NewManager(logger.New("node-test"))
	require.NoError(t, err)
	sessions, err := sm.List()
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Equal(t, workspace, sessions[0].WorkingDir,
		"a remote turn must not run in whatever directory the hosting process started in")
	require.True(t, sessions[0].SkipDream)

	// And the context binding points at it: that is what makes a follow-up possible
	// without any in-memory state.
	var binding contextBinding
	require.NoError(t, fileutil.ReadJSON(
		filepath.Join(base, "node", contextsDirName, task.ContextID+".json"), &binding))
	require.Equal(t, sessions[0].ID, binding.SessionID)
	require.Equal(t, workspace, binding.Workspace)
}

func TestRemoteTurnOffersOnlyTheNodesAllowedTools(t *testing.T) {
	base := nodeHome(t)
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(oneMessageScript("ok")...)

	// The tools the model is OFFERED come from the node's config and nothing else. The
	// other half of the same promise — refusing a tool at execution — is the run's tool
	// view, built from this very list.
	h := startRealNode(t, nodeConfig(t, mock, filepath.Join(base, "ws"), "ReadFile", "Grep"))
	client := newTestClient(t, h)
	_, err := client.SendMessage(context.Background(), sendRequest("你好", false))
	require.NoError(t, err)

	requests := mock.Requests()
	require.Len(t, requests, 1)
	offered := map[string]bool{}
	for _, tool := range requests[0].Tools {
		offered[tool.Name] = true
	}
	require.True(t, offered["ReadFile"], "an allowlisted tool must be offered, got %v", offered)
	require.False(t, offered["Bash"], "a tool the operator did not allow must not be offered")
	require.False(t, offered["WriteFile"])
}

func TestAnEmptyToolListOffersNoToolsThatTouchThisMachine(t *testing.T) {
	base := nodeHome(t)
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(oneMessageScript("ok")...)

	// The empty list is a POLICY, not an accident: a node whose operator named no tools hands
	// the model nothing that acts on this machine. AskUserQuestion is not one of those — it
	// asks the CALLER, and it is the only interaction a remote task has — so it is the one
	// tool left in the view, and the list still means what it says about everything else.
	h := startRealNode(t, nodeConfig(t, mock, filepath.Join(base, "ws")))
	client := newTestClient(t, h)
	_, err := client.SendMessage(context.Background(), sendRequest("你好", false))
	require.NoError(t, err)

	requests := mock.Requests()
	require.Len(t, requests, 1)
	offered := make([]string, 0, len(requests[0].Tools))
	for _, tool := range requests[0].Tools {
		offered = append(offered, tool.Name)
	}
	require.Equal(t, []string{tools.ToolNameAskUser}, offered,
		"an empty allowlist must leave exactly the tool that only talks to the caller")
}

func TestAFailedTurnIsReportedAsFailed(t *testing.T) {
	base := nodeHome(t)
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	// The provider fails: a turn that dies mid-flight must not be dressed up as a result.
	mock.Script(mockllm.Step{Reply: func(_ context.Context, w http.ResponseWriter, _ mockllm.Protocol) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"model exploded"}}`))
	}})

	h := startRealNode(t, nodeConfig(t, mock, filepath.Join(base, "ws"), "ReadFile"))
	client := newTestClient(t, h)

	result, err := client.SendMessage(context.Background(), sendRequest("检查备份", false))
	require.NoError(t, err, "the TASK failed; the protocol call itself succeeded")
	task := result.(*a2a.Task)
	require.Equal(t, a2a.TaskStateFailed, task.Status.State)
	require.NotNil(t, task.Status.Message, "a failure must carry its reason")
	require.Empty(t, task.Artifacts, "a failed task has no product")
}

// The property the node depends on, asserted at the node's own boundary: the session a
// remote task ran in is NOT a conversation a local frontend can pick up and continue.
//
// This is the version of agent/managed_test.go that matters, because the session here was
// created by the real runner and the "local" agent below is built exactly the way a
// frontend builds one (its own manager, the session loaded as current, no grant).
func TestASessionHeldByAContextCannotBeDrivenLocally(t *testing.T) {
	base := nodeHome(t)
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(oneMessageScript("remote work done")...)

	cfg := nodeConfig(t, mock, filepath.Join(base, "workspace"), "ReadFile")
	h := startRealNode(t, cfg)
	client := newTestClient(t, h)
	_, err := client.SendMessage(context.Background(), sendRequest("remote job", false))
	require.NoError(t, err)

	sm, err := session.NewManager(logger.New("node-test"))
	require.NoError(t, err)
	sessions, err := sm.List()
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Equal(t, session.KindA2AExecution, sessions[0].Kind)

	before, err := sm.LoadSessionMessages(sessions[0].ID)
	require.NoError(t, err)

	// A frontend resuming that session: its own agent, its own manager, the session current.
	localMgr, err := session.NewManager(logger.New("node-test"))
	require.NoError(t, err)
	_, err = localMgr.Load(sessions[0].ID)
	require.NoError(t, err)
	local, _, err := agent.NewAIAgentWithConfig(context.Background(), agent.AgentConfig{
		Logger:         logger.New("node-test"),
		FullConfig:     cfg,
		SystemConfig:   agent.SystemConfigFromConfig(cfg),
		SessionManager: localMgr,
	})
	require.NoError(t, err)
	t.Cleanup(local.Close)

	events := local.RunConversationStream(context.Background(), nil, "let me continue here", "", llm.ChatOptions{})
	var refusal error
	for ev := range events {
		if ev.Type == agent.AgentEventError && ev.Result != nil {
			refusal = ev.Result.Error
		}
	}
	require.ErrorIs(t, refusal, agent.ErrManagedSession)

	after, err := sm.LoadSessionMessages(sessions[0].ID)
	require.NoError(t, err)
	require.Len(t, after, len(before),
		"a refused turn must not have written into the remote conversation")
}
