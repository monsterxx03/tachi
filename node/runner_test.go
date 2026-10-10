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

// The session a remote turn lands in records WHO asked, in the one form that cannot be
// claimed: the identity the TLS handshake established out of the caller's certificate.
//
// The surface has to be off loopback for there to be an identity at all (node/options.go:
// leaving the machine is what makes identifying a caller necessary), which makes this the one
// place in the package where a REAL turn runs behind a required client certificate.
//
// Two callers rather than one, deliberately: with a single caller a field filled from the
// wrong handshake — the last one seen, a package-global — is indistinguishable from the right
// answer. Two conversations that must each record their OWN caller are not.
func TestTheReceivingSessionRecordsWhoseWorkItHolds(t *testing.T) {
	base := nodeHome(t)
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(append(oneMessageScript("第一件活儿"), oneMessageScript("第二件活儿")...)...)

	cfg := nodeConfig(t, mock, filepath.Join(base, "workspace"), "ReadFile")
	cfg.Node.Listen = "0.0.0.0:0"
	h := startRealNode(t, cfg)

	first, err := LoadOrCreateIdentity(t.TempDir())
	require.NoError(t, err)
	second, err := LoadOrCreateIdentity(t.TempDir())
	require.NoError(t, err)

	for _, caller := range []*Identity{first, second} {
		status, body, err := sendAs(t, h, caller, "谁在这儿干活？")
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, status, "body=%s", body)
	}

	sm, err := session.NewManager(logger.New("node-test"))
	require.NoError(t, err)
	sessions, err := sm.List()
	require.NoError(t, err)
	require.Len(t, sessions, 2)

	sources := make(map[string]bool, len(sessions))
	for _, s := range sessions {
		require.Equal(t, session.KindA2AExecution, s.Kind)
		sources[s.SourceNodeID] = true
	}
	require.Equal(t, map[string]bool{first.NodeID: true, second.NodeID: true}, sources,
		"each conversation carries the caller that opened it, not whichever handshake came last")
	require.NotEqual(t, h.NodeID(), first.NodeID, "a caller is never the machine it called")
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

// A remote turn must not read or write THIS machine's long-term memory (design §2.3). The two
// ways in are separate mechanisms, so both are asserted: the recall reminder injects the host's
// memory on its own, and the two memory tools are reachable through whatever the operator put in
// `node.tools`.
//
// The memory tools are named IN the allowlist here, which is the configuration that would expose
// them — and the machine really has memory configured, so the tools existed to be excluded rather
// than the assertion passing because there was nothing to exclude.
func TestARemoteTurnDoesNotReadOrWriteTheHostsMemory(t *testing.T) {
	base := nodeHome(t)
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(oneMessageScript("ok")...)

	cfg := nodeConfig(t, mock, filepath.Join(base, "ws"),
		"ReadFile", tools.ToolNameMemoryRecall, tools.ToolNameRecordMemory)
	cfg.Memory.Type = "topic"

	h := startRealNode(t, cfg)
	client := newTestClient(t, h)
	_, err := client.SendMessage(context.Background(), sendRequest("你好", false))
	require.NoError(t, err)

	requests := mock.Requests()
	require.Len(t, requests, 1)
	offered := map[string]bool{}
	for _, tool := range requests[0].Tools {
		offered[tool.Name] = true
	}
	require.True(t, offered["ReadFile"],
		"the rest of the allowlist must still apply — otherwise this proves nothing, got %v", offered)
	for name := range hostOnlyTools {
		require.False(t, offered[name],
			"%s must never reach a remote turn, even when the operator names it, got %v", name, offered)
	}
}

// The other half of the same rule, asserted on the agent the runner actually builds: recall is
// switched off, so no reminder injects this machine's memory into a remote prompt. It has to be
// asserted here rather than through a turn — a remote conversation with nothing worth recalling
// produces the same (empty) prompt either way.
func TestARemoteAgentHasRecallSwitchedOff(t *testing.T) {
	base := nodeHome(t)
	cfg := nodeConfig(t, mockllm.NewServer(), filepath.Join(base, "ws"))
	cfg.Memory.Type = "topic"

	r, err := newRunner(cfg, filepath.Join(base, "ws"),
		filepath.Join(base, "node"), filepath.Join(base, "sessions"), logger.New("node-test"))
	require.NoError(t, err)
	cr, err := r.contextRun(context.Background(), "", "ctx-1")
	require.NoError(t, err)
	require.NotNil(t, cr.agent.Config.Memory, "memory must be configured for this to mean anything")
	require.NotNil(t, cr.agent.Config.Memory.Backend,
		"a configured machine has a backend — which is what registers the two tools")
	require.True(t, cr.agent.Config.Memory.SkipRecall,
		"a remote turn must not have the host's memory recalled into its prompt")
}

// A remote turn's context carries the usage source, so every model call it makes is attributable
// to the task that caused it (design §9.5).
//
// Asserted on the context rather than on the ledger, because the ledger's recorder is a
// process-wide singleton whose directory is fixed at its first use in the process — a test that
// read it would be reading whichever directory another test happened to set up. What THIS side
// owns is the context, and llm/usage_recorder_test.go covers the row it produces.
func TestATurnRunsWithItsUsageSourceAttached(t *testing.T) {
	base := nodeHome(t)
	cfg := nodeConfig(t, mockllm.NewServer(), filepath.Join(base, "ws"))
	r, err := newRunner(cfg, filepath.Join(base, "ws"),
		filepath.Join(base, "node"), filepath.Join(base, "sessions"), logger.New("node-test"))
	require.NoError(t, err)
	cr, err := r.contextRun(context.Background(), "", "ctx-1")
	require.NoError(t, err)

	ctx, cancel := r.turnContext(context.Background(), cr, a2a.TaskID("t-1"))
	defer cancel()

	src, ok := llm.UsageSourceFromCtx(ctx)
	require.True(t, ok, "a remote turn must file its calls under a source")
	require.Equal(t, llm.UsageSourceA2A, src.Origin)
	require.Equal(t, "t-1", src.TaskID)
	require.Equal(t, "ctx-1", src.ContextID)
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
// sessionCount is how many conversations this machine's store holds, read the way a local
// frontend reads it — so a session a remote task opened is counted, which is the point: it is a
// conversation on this machine whether or not it is one the user may drive.
func sessionCount(t *testing.T) int {
	t.Helper()
	sm, err := session.NewManager(logger.New("node-test"))
	require.NoError(t, err)
	list, err := sm.List()
	require.NoError(t, err)
	return len(list)
}

// Only a NEW work request opens a conversation (design §7.1.1). The other three things that reach
// this surface — a replay of an accepted message, a query, and a cancel — travel the same way and
// must not: a `get` that quietly opened one would leave an empty conversation behind every poll,
// and a replay would leave one behind every retry.
func TestOnlyANewWorkRequestOpensAConversation(t *testing.T) {
	base := nodeHome(t)
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	mock.Script(oneMessageScript("备份检查完成")...)

	h := startRealNode(t, nodeConfig(t, mock, filepath.Join(base, "ws"), "ReadFile"))
	client := newTestClient(t, h)

	// ONE request object, sent twice: the same message id is what makes the second a replay.
	// A fresh one would be a new work request with new ids — which is a different test.
	req := sendRequest("检查备份", false)
	first, err := client.SendMessage(context.Background(), req)
	require.NoError(t, err)
	task := first.(*a2a.Task)

	replay, err := client.SendMessage(context.Background(), req)
	require.NoError(t, err)
	replayed, ok := replay.(*a2a.Task)
	require.True(t, ok)
	require.Equal(t, task.ID, replayed.ID, "a replay answers with the task already accepted")

	if _, err = client.GetTask(context.Background(), &a2a.GetTaskRequest{ID: task.ID}); err != nil {
		require.NoError(t, err)
	}
	// A finished task cannot be cancelled, and that refusal is not the point here — what is being
	// asserted is that the call opens nothing on its way to it.
	_, _ = client.CancelTask(context.Background(), &a2a.CancelTaskRequest{ID: task.ID})

	require.Len(t, mock.Requests(), 1, "only the first submission may reach the model")
	require.Equal(t, 1, sessionCount(t),
		"a replay, a query and a cancel must not each leave a conversation behind")
}

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
