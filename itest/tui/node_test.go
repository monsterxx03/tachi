//go:build integration

package tui_test

// 接收侧：一个远端任务真的在这台机器上跑完一轮，以及停在问题上、被回答之后接着跑同一轮。
//
// 这条 spec 覆盖包内测试够不着的那一层 —— **真实的 config.yaml**：`node.enabled` /
// `workspace` / `tools` / `question_timeout` / `max_parked` 从 YAML 经 config.Load 走到
// NodeConfig，工作区、工具白名单、会话存储全部由文件驱动。node 包的测试用代码构造配置，所以
// "只有走文件路径才会踩"的东西（defaults 是否生效、context_window 有没有、workspace 里那个
// `~` 展开没展开）它测不到。
//
// 不在这条 spec 里的：受管会话的准入（普通入口不能驱动它）。那条在 node/agent 包内有更精确
// 的断言（自建 agent、同一会话、逐条比对历史未被写），而在这里驱动真实 TUI 去选中那个会话需要
// 一串按键脚本，换不来额外的信心。

import (
	"context"
	"path/filepath"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/monsterxx03/tachi/itest/harness"
	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/monsterxx03/tachi/node"
	"github.com/monsterxx03/tachi/session"
)

var _ = ginkgo.Describe("接收侧：远端任务", func() {
	ginkgo.It("在节点配置的工作区里跑完一轮，并留下一条受管的执行记录", func() {
		// The workspace is a temp dir of its own, so the spec never touches the real one
		// (the config default, ~/workspace, is not a test's business).
		workspace := filepath.Join(ginkgo.GinkgoT().TempDir(), "workspace")

		mock, home := startMock(mockllm.ProtocolOpenAI,
			[]mockllm.Step{{Reply: mockllm.Stream(
				mockllm.Text("备份检查完成：两个目录异常"),
				mockllm.Finish("stop"),
				mockllm.Done(),
			)}},
			harness.WithNodeWorkspace(workspace),
			harness.WithNodeService(),
		)

		ctx, cancel := context.WithTimeout(context.Background(), specTimeout)
		ginkgo.DeferCleanup(cancel)

		// Exactly what an entry point does: load the home's config, then start-or-attach.
		cfg := harness.Config(ginkgo.GinkgoT(), home)
		gomega.Expect(cfg.Node.Enabled).To(gomega.BeTrue(),
			"the fixture must have switched the node on, or this spec tests nothing")
		gomega.Expect(cfg.Node.Workspace).To(gomega.Equal(workspace),
			"the fixture's workspace has to arrive through the YAML, defaults and all — what a remote turn may DO is code now, not configuration")

		handle, err := node.StartOrAttachNode(ctx, cfg)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(handle).NotTo(gomega.BeNil())
		gomega.Expect(handle.Hosting()).To(gomega.BeTrue())
		ginkgo.DeferCleanup(func() { _ = handle.Close() })

		// A peer's view: resolve the Card, then send a message.
		card, err := agentcard.DefaultResolver.Resolve(ctx, "http://"+handle.RemoteAddr())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		client, err := a2aclient.NewFromCard(ctx, card)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ginkgo.DeferCleanup(func() { _ = client.Destroy() })

		result, err := client.SendMessage(ctx, &a2a.SendMessageRequest{
			Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("检查昨天的备份")),
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		task, ok := result.(*a2a.Task)
		gomega.Expect(ok).To(gomega.BeTrue())
		gomega.Expect(task.Status.State).To(gomega.Equal(a2a.TaskStateCompleted))
		gomega.Expect(task.Artifacts).To(gomega.HaveLen(1))
		gomega.Expect(task.Artifacts[0].Parts[0].Text()).To(gomega.Equal("备份检查完成：两个目录异常"))

		// The script was consumed EXACTLY as written: a missing or extra model call (an
		// auto-compaction, a title generation) would show up here rather than as a mystery
		// later.
		gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())

		// The turn ran in a session on the node's workspace, in the home's own session store,
		// marked as a remote execution the memory consolidation must skip.
		sm, err := session.NewManager(nil)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		sessions, err := sm.List()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(sessions).To(gomega.HaveLen(1), "one remote conversation, one session")
		gomega.Expect(sessions[0].WorkingDir).To(gomega.Equal(workspace),
			"the remote turn must run in the CONFIGURED workspace")
		gomega.Expect(sessions[0].Kind).To(gomega.Equal(session.KindA2AExecution))
		gomega.Expect(sessions[0].SkipDream).To(gomega.BeTrue())
	})

	ginkgo.It("问一个问题，回答之后接着跑同一轮", func() {
		workspace := filepath.Join(ginkgo.GinkgoT().TempDir(), "workspace")

		mock, home := startMock(mockllm.ProtocolOpenAI,
			[]mockllm.Step{
				{Reply: mockllm.Stream(
					mockllm.ToolCallStart("ask_1", "AskUserQuestion",
						`{"questions":[{"question":"检查照片还是文档？","header":"范围","multiSelect":false}]}`),
					mockllm.Finish("tool_calls"),
					mockllm.Done(),
				)},
				{Reply: mockllm.Stream(
					mockllm.Text("好，只检查照片。"),
					mockllm.Finish("stop"),
					mockllm.Done(),
				)},
			},
			harness.WithNodeWorkspace(workspace),
			harness.WithNodeService(),
		)

		ctx, cancel := context.WithTimeout(context.Background(), specTimeout)
		ginkgo.DeferCleanup(cancel)

		cfg := harness.Config(ginkgo.GinkgoT(), home)
		// The two settings a parked question needs arrive through the YAML like everything
		// else, and this is the only place the FILE path is exercised (the package's own tests
		// build the config in code). What must hold is that a question cannot wait forever and
		// cannot pile up without a limit — the values themselves are defaults.
		gomega.Expect(cfg.Node.QuestionTimeout).To(gomega.BeNumerically(">", 0),
			"a question with no deadline is a caller holding a turn open forever")
		gomega.Expect(cfg.Node.MaxParked).To(gomega.BeNumerically(">", 0))

		handle, err := node.StartOrAttachNode(ctx, cfg)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ginkgo.DeferCleanup(func() { _ = handle.Close() })

		card, err := agentcard.DefaultResolver.Resolve(ctx, "http://"+handle.RemoteAddr())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		client, err := a2aclient.NewFromCard(ctx, card)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ginkgo.DeferCleanup(func() { _ = client.Destroy() })

		// A blocking send returns at the interrupted state, carrying the question (§6.2).
		result, err := client.SendMessage(ctx, &a2a.SendMessageRequest{
			Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("检查昨天的备份")),
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		asked, ok := result.(*a2a.Task)
		gomega.Expect(ok).To(gomega.BeTrue())
		gomega.Expect(asked.Status.State).To(gomega.Equal(a2a.TaskStateInputRequired))
		gomega.Expect(asked.Status.Message.Parts[0].Text()).To(gomega.ContainSubstring("检查照片还是文档？"))

		questionID, ok := asked.Status.Message.Metadata["questionId"].(string)
		gomega.Expect(ok).To(gomega.BeTrue(), "the question's id has to reach the caller")

		answer := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("照片"))
		answer.TaskID = asked.ID
		answer.ContextID = asked.ContextID
		answer.Metadata = map[string]any{"questionId": questionID}

		// The answer resumes the SAME turn: the script's second step is what a fresh loop
		// would have needed a question of its own to reach.
		done, err := client.SendMessage(ctx, &a2a.SendMessageRequest{Message: answer})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		finished, ok := done.(*a2a.Task)
		gomega.Expect(ok).To(gomega.BeTrue())
		gomega.Expect(finished.Status.State).To(gomega.Equal(a2a.TaskStateCompleted))
		gomega.Expect(finished.Artifacts[0].Parts[0].Text()).To(gomega.Equal("好，只检查照片。"))
		gomega.Expect(mock.Requests()).To(gomega.HaveLen(2))
		gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())
	})
})
