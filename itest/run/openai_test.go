//go:build integration

package run_test

import (
	"github.com/monsterxx03/tachi/itest/harness"
	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/monsterxx03/tachi/itest/run"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

// M0 scenarios (docs §十): the shortest path from agent loop to mock LLM,
// running the REAL binary with isolated --home and seeded work dirs.

var _ = ginkgo.Describe("-p pipe mode", func() {
	ginkgo.It("基础消息流: text_delta 累积 + turn_complete(stop) + exit 0", func() {
		mock := mockllm.NewServer()
		ginkgo.DeferCleanup(mock.Close)
		mock.Script(mockllm.Step{Reply: mockllm.Stream(
			mockllm.Thinking("让我思考一下"),
			mockllm.Text("你好,世界!"),
			mockllm.Usage(100, 20),
			mockllm.Finish("stop"),
			mockllm.Done(),
		)})
		home := harness.NewHome(ginkgo.GinkgoT(), mock)
		workdir := harness.SeedWorkDir(ginkgo.GinkgoT(), nil)

		res := run.Binary(bin, workdir, "--home", home, "-p", "你好", "-o", "json-stream")

		gomega.Expect(res.Err).NotTo(gomega.HaveOccurred())
		gomega.Expect(res.ExitCode).To(gomega.Equal(0))
		events := run.ParseNDJSON(res.Stdout)

		gomega.Expect(run.TextDelta(events)).To(gomega.ContainSubstring("你好,世界!"))
		gomega.Expect(events).To(gomega.ContainElement(gomega.SatisfyAll(
			gomega.HaveField("Type", "turn_complete"),
			gomega.HaveField("ExitReason", "stop"),
			gomega.HaveField("Iterations", 1),
		)))
		// thinking 事件也在流里(尽管 -p 模式不渲染)
		gomega.Expect(events).To(gomega.ContainElement(gomega.HaveField("Type", "thinking_delta")))
		// 脚本被完整消费,没有多余请求
		gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())
		gomega.Expect(mock.Requests()).To(gomega.HaveLen(1))
	})

	ginkgo.It("工具调用循环: tool_call → tool_result → 第二轮 text, 结果回传第二次请求", func() {
		mock := mockllm.NewServer()
		ginkgo.DeferCleanup(mock.Close)
		mock.Script(
			mockllm.Step{Reply: mockllm.Stream(
				mockllm.ToolCallStart("call_1", "Bash", `{"command":"ls"}`),
				mockllm.Usage(120, 30),
				mockllm.Finish("tool_calls"),
				mockllm.Done(),
			)},
			mockllm.Step{
				Require: mockllm.HasToolResult("call_1", gomega.ContainSubstring("README.md")),
				Reply: mockllm.Stream(
					mockllm.Text("目录里有 README.md。"),
					mockllm.Usage(200, 20),
					mockllm.Finish("stop"),
					mockllm.Done(),
				),
			},
		)
		home := harness.NewHome(ginkgo.GinkgoT(), mock, harness.WithBashAllow())
		workdir := harness.SeedWorkDir(ginkgo.GinkgoT(), map[string]string{"README.md": "# Fixture\n"})

		res := run.Binary(bin, workdir, "--home", home, "-p", "看一下当前目录", "-o", "json-stream")

		gomega.Expect(res.Err).NotTo(gomega.HaveOccurred())
		gomega.Expect(res.ExitCode).To(gomega.Equal(0))
		events := run.ParseNDJSON(res.Stdout)

		gomega.Expect(events).To(gomega.ContainElement(gomega.SatisfyAll(
			gomega.HaveField("Type", "tool_call"),
			gomega.HaveField("ToolName", "Bash"),
		)))
		gomega.Expect(events).To(gomega.ContainElement(gomega.SatisfyAll(
			gomega.HaveField("Type", "tool_result"),
			gomega.HaveField("ToolCallID", "call_1"),
			gomega.HaveField("IsError", false),
		)))
		gomega.Expect(events).To(gomega.ContainElement(gomega.SatisfyAll(
			gomega.HaveField("Type", "turn_complete"),
			gomega.HaveField("ExitReason", "stop"),
		)))

		// agent loop 确实把工具结果回传了第二次请求
		gomega.Expect(mock.Requests()).To(gomega.HaveLen(2))
		gomega.Expect(mock.Requests()[1].Messages).To(gomega.ContainElement(gomega.SatisfyAll(
			gomega.HaveField("Role", "tool"),
			gomega.HaveField("ToolCallID", "call_1"),
			gomega.HaveField("Content", gomega.ContainSubstring("README.md")),
		)))
		gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("错误路径/重试: 429 两次后成功 (OpenAI RetryProvider)", func() {
		mock := mockllm.NewServer()
		ginkgo.DeferCleanup(mock.Close)
		mock.Script(
			mockllm.Step{Reply: mockllm.StatusError(429, "rate limited")},
			mockllm.Step{Reply: mockllm.StatusError(429, "rate limited")},
			mockllm.Step{Reply: mockllm.Stream(
				mockllm.Text("重试后终于成功了"),
				mockllm.Usage(50, 10),
				mockllm.Finish("stop"),
				mockllm.Done(),
			)},
		)
		home := harness.NewHome(ginkgo.GinkgoT(), mock)
		workdir := harness.SeedWorkDir(ginkgo.GinkgoT(), nil)

		res := run.Binary(bin, workdir, "--home", home, "-p", "hi", "-o", "json-stream")

		gomega.Expect(res.Err).NotTo(gomega.HaveOccurred())
		gomega.Expect(res.ExitCode).To(gomega.Equal(0))
		// RetryProvider 对 429 重试(最大 2 次),最终第三次成功
		gomega.Expect(mock.Requests()).To(gomega.HaveLen(3))
		gomega.Expect(run.TextDelta(run.ParseNDJSON(res.Stdout))).To(gomega.ContainSubstring("重试后终于成功了"))
		gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("--allowed-tools 生效: 白名单外的工具调用不执行并报错回传", func() {
		mock := mockllm.NewServer()
		ginkgo.DeferCleanup(mock.Close)
		mock.Script(
			// 第一轮:请求必须只广告白名单内的工具; mock 故意返回被过滤的
			// WebSearch 调用 —— agent 不执行它, 而是把错误结果回传
			mockllm.Step{
				Require: mockllm.HasNoTool("WebSearch"),
				Reply: mockllm.Stream(
					mockllm.ToolCallStart("call_1", "WebSearch", `{"query":"tachi"}`),
					mockllm.Usage(120, 30),
					mockllm.Finish("tool_calls"),
					mockllm.Done(),
				),
			},
			mockllm.Step{
				Require: mockllm.HasToolError("call_1"),
				Reply: mockllm.Stream(
					mockllm.Text("抱歉, 这个工具不可用。"),
					mockllm.Usage(200, 20),
					mockllm.Finish("stop"),
					mockllm.Done(),
				),
			},
		)
		home := harness.NewHome(ginkgo.GinkgoT(), mock)
		workdir := harness.SeedWorkDir(ginkgo.GinkgoT(), nil)

		res := run.Binary(bin, workdir, "--home", home,
			"-p", "搜索一下", "-o", "json-stream", "--allowed-tools", "Bash")

		gomega.Expect(res.Err).NotTo(gomega.HaveOccurred())
		gomega.Expect(res.ExitCode).To(gomega.Equal(0))
		events := run.ParseNDJSON(res.Stdout)

		// 工具错误执行事件: IsError=true 的 tool_result
		gomega.Expect(events).To(gomega.ContainElement(gomega.SatisfyAll(
			gomega.HaveField("Type", "tool_result"),
			gomega.HaveField("ToolCallID", "call_1"),
			gomega.HaveField("IsError", true),
		)))
		gomega.Expect(events).To(gomega.ContainElement(gomega.SatisfyAll(
			gomega.HaveField("Type", "turn_complete"),
			gomega.HaveField("ExitReason", "stop"),
		)))
		gomega.Expect(mock.Requests()).To(gomega.HaveLen(2))
		gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())
	})
})
