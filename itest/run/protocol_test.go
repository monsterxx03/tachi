//go:build integration

package run_test

// Dual-protocol -p pipe mode scenarios: the SAME script shapes served over
// both wire formats (docs §3.2 — "same scenario, both providers behave
// identically"). Each scenario is written once as a DescribeTable
// parameterized by protocol; adding a new provider protocol is a one-line
// Entry. Thinking round-trips are asserted in both protocols (OpenAI echoes
// reasoning_content, Anthropic thinking blocks with signature).

import (
	"github.com/monsterxx03/tachi/itest/harness"
	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/monsterxx03/tachi/itest/run"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

// startMock starts a protocol-specific mock server wired into an isolated
// --home whose config fixture follows the same protocol (provider type +
// base_url). Registered for cleanup; returned mock/home belong to one spec.
func startMock(p mockllm.Protocol, steps []mockllm.Step, opts ...harness.Option) (*mockllm.Server, string) {
	mock := mockllm.NewServer(mockllm.WithProtocol(p))
	ginkgo.DeferCleanup(mock.Close)
	mock.Script(steps...)
	home := harness.NewHome(ginkgo.GinkgoT(), mock,
		append([]harness.Option{harness.WithProtocol(p)}, opts...)...)
	return mock, home
}

var _ = ginkgo.Describe("-p pipe mode (dual protocol)", func() {
	ginkgo.DescribeTable("基础消息流: thinking + text + turn_complete(stop) + exit 0",
		func(p mockllm.Protocol) {
			mock, home := startMock(p, []mockllm.Step{{
				// -p 模式 Thinking:&false(main_agent.go)→ 请求体必须体现
				// thinking 禁用(OpenAI 无 reasoning_effort / Anthropic
				// thinking.type=disabled),锁死这个行为
				Require: mockllm.HasThinkingDisabled(),
				Reply: mockllm.Stream(
					mockllm.Thinking("让我思考一下"),
					mockllm.Text("你好,世界!"),
					mockllm.Usage(100, 20),
					mockllm.Finish("stop"),
					mockllm.Done(),
				),
			}})
			workdir := harness.SeedWorkDir(ginkgo.GinkgoT(), nil)

			res := run.Binary(bin, workdir, "--home", home, "-p", "你好", "-o", "json-stream")

			gomega.Expect(res.Err).NotTo(gomega.HaveOccurred())
			gomega.Expect(res.ExitCode).To(gomega.Equal(0))
			events := run.ParseNDJSON(res.Stdout)

			gomega.Expect(run.TextDelta(events)).To(gomega.ContainSubstring("你好,世界!"))
			gomega.Expect(events).To(gomega.ContainElement(gomega.HaveField("Type", "thinking_delta")))
			gomega.Expect(events).To(gomega.ContainElement(gomega.SatisfyAll(
				gomega.HaveField("Type", "turn_complete"),
				gomega.HaveField("ExitReason", "stop"),
				gomega.HaveField("Iterations", 1),
			)))
			// 脚本被完整消费,没有多余请求
			gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())
			gomega.Expect(mock.Requests()).To(gomega.HaveLen(1))
		},
		ginkgo.Entry("OpenAI", mockllm.ProtocolOpenAI),
		ginkgo.Entry("Anthropic", mockllm.ProtocolAnthropic),
	)

	ginkgo.DescribeTable("工具调用循环: thinking 回传 + tool result 回传第二次请求",
		func(p mockllm.Protocol) {
			mock, home := startMock(p, []mockllm.Step{
				{Reply: mockllm.Stream(
					mockllm.Thinking("让我查一下目录"),
					mockllm.ToolCallStart("call_1", "Bash", `{"command":"ls"}`),
					mockllm.Usage(120, 30),
					mockllm.Finish("tool_calls"),
					mockllm.Done(),
				)},
				{
					// 第二轮请求必须回传 thinking(OpenAI reasoning_content /
					// Anthropic thinking block with signature)与工具结果;
					// usage 带 cache 计数 —— 第二轮历史命中缓存是真实场景
					Require: mockllm.HasThinking(gomega.ContainSubstring("让我查一下目录")),
					Reply: mockllm.Stream(
						mockllm.Text("目录里有 README.md。"),
						mockllm.UsageWithCache(200, 20, 120, 40),
						mockllm.Finish("stop"),
						mockllm.Done(),
					),
				},
			}, harness.WithBashAllow())
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

			// 第二轮的 cache usage 穿透到 turn_complete 事件
			var tcUsage *run.Usage
			for _, ev := range events {
				if ev.Type == "turn_complete" {
					tcUsage = ev.Usage
				}
			}
			gomega.Expect(tcUsage).NotTo(gomega.BeNil())
			gomega.Expect(tcUsage.InputTokens).To(gomega.Equal(int64(200)))
			gomega.Expect(tcUsage.CacheReadInputTokens).To(gomega.Equal(int64(120)))
			if p == mockllm.ProtocolAnthropic {
				// cache creation 只在 Anthropic 线格式上表达
				gomega.Expect(tcUsage.CacheCreationInputTokens).To(gomega.Equal(int64(40)))
			} else {
				gomega.Expect(tcUsage.CacheCreationInputTokens).To(gomega.Equal(int64(0)))
			}
		},
		ginkgo.Entry("OpenAI", mockllm.ProtocolOpenAI),
		ginkgo.Entry("Anthropic", mockllm.ProtocolAnthropic),
	)

	ginkgo.DescribeTable("429 两次后重试成功",
		func(p mockllm.Protocol) {
			// OpenAI 走 RetryProvider;Anthropic 走 SDK 内部重试 ——
			// 最终都重试到第三次成功,行为一致
			mock, home := startMock(p, []mockllm.Step{
				{Reply: mockllm.StatusError(429, "rate limited")},
				{Reply: mockllm.StatusError(429, "rate limited")},
				{Reply: mockllm.Stream(
					mockllm.Text("重试后终于成功了"),
					mockllm.Usage(50, 10),
					mockllm.Finish("stop"),
					mockllm.Done(),
				)},
			})
			workdir := harness.SeedWorkDir(ginkgo.GinkgoT(), nil)

			res := run.Binary(bin, workdir, "--home", home, "-p", "hi", "-o", "json-stream")

			gomega.Expect(res.Err).NotTo(gomega.HaveOccurred())
			gomega.Expect(res.ExitCode).To(gomega.Equal(0))
			gomega.Expect(mock.Requests()).To(gomega.HaveLen(3))
			gomega.Expect(run.TextDelta(run.ParseNDJSON(res.Stdout))).To(gomega.ContainSubstring("重试后终于成功了"))
			gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())
		},
		ginkgo.Entry("OpenAI", mockllm.ProtocolOpenAI),
		ginkgo.Entry("Anthropic", mockllm.ProtocolAnthropic),
	)

	ginkgo.DescribeTable("持续 429: 重试耗尽后 run 模式报错退出 (exit 1 + error 事件)",
		func(p mockllm.Protocol) {
			// 全部 step 都返回 429 —— 重试(最大 2 次)后仍失败。多放一个
			// step 是为了证明重试耗尽后不再发起请求(脚本不会耗尽)。
			mock, home := startMock(p, []mockllm.Step{
				{Reply: mockllm.StatusError(429, "rate limited")},
				{Reply: mockllm.StatusError(429, "rate limited")},
				{Reply: mockllm.StatusError(429, "rate limited")},
				{Reply: mockllm.StatusError(429, "rate limited")},
			})
			workdir := harness.SeedWorkDir(ginkgo.GinkgoT(), nil)

			res := run.Binary(bin, workdir, "--home", home, "-p", "hi", "-o", "json-stream")

			gomega.Expect(res.Err).NotTo(gomega.HaveOccurred())
			// ExitReasonError → exit 1 (exitCodeForReason)
			gomega.Expect(res.ExitCode).To(gomega.Equal(1))
			events := run.ParseNDJSON(res.Stdout)
			// 没有 turn_complete,只有 error 事件(消息含 429)
			gomega.Expect(events).NotTo(gomega.ContainElement(gomega.HaveField("Type", "turn_complete")))
			gomega.Expect(events).To(gomega.ContainElement(gomega.SatisfyAll(
				gomega.HaveField("Type", "error"),
				gomega.HaveField("Error", gomega.ContainSubstring("429")),
			)))
			// 初始 + 2 次重试 = 3 个请求;重试耗尽后不再发起请求
			gomega.Expect(mock.Requests()).To(gomega.HaveLen(3))
			// 第 4 个 step 未被消费 → 脚本未耗尽,mock 无错误
			gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())
		},
		ginkgo.Entry("OpenAI", mockllm.ProtocolOpenAI),
		ginkgo.Entry("Anthropic", mockllm.ProtocolAnthropic),
	)

	ginkgo.DescribeTable("--allowed-tools 生效: 白名单外工具不执行并报错回传",
		func(p mockllm.Protocol) {
			mock, home := startMock(p, []mockllm.Step{
				// 第一轮:请求必须只广告白名单内的工具; mock 故意返回被过滤的
				// WebSearch 调用 —— agent 不执行它, 而是把错误结果回传
				{
					Require: mockllm.HasNoTool("WebSearch"),
					Reply: mockllm.Stream(
						mockllm.ToolCallStart("call_1", "WebSearch", `{"query":"tachi"}`),
						mockllm.Usage(120, 30),
						mockllm.Finish("tool_calls"),
						mockllm.Done(),
					),
				},
				{
					Require: mockllm.HasToolError("call_1"),
					Reply: mockllm.Stream(
						mockllm.Text("抱歉, 这个工具不可用。"),
						mockllm.Usage(200, 20),
						mockllm.Finish("stop"),
						mockllm.Done(),
					),
				},
			})
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
		},
		ginkgo.Entry("OpenAI", mockllm.ProtocolOpenAI),
		ginkgo.Entry("Anthropic", mockllm.ProtocolAnthropic),
	)
})
