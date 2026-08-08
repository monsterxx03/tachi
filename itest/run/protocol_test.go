//go:build integration

package run_test

// Triple-protocol -p pipe mode scenarios: the SAME script shapes served over
// all wire formats (docs §3.2 — "same scenario, all providers behave
// identically"). Each scenario is written once as a DescribeTable
// parameterized by protocol; adding a new provider protocol is a one-line
// Entry.
//
// Thinking round-trips: OpenAI (reasoning_content) and Anthropic (thinking
// blocks with signature) ECHO previous-turn reasoning back; the Responses
// protocol FORBIDS resending it (llm.OpenAIResponsesProvider drops thinking
// blocks), so the tool-loop scenario asserts the negative on that wire
// (HasNoThinking) — see thinkingRoundTripRequire.

import (
	"time"

	"github.com/monsterxx03/tachi/itest/harness"
	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/monsterxx03/tachi/itest/run"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

// thinkingRoundTripRequire returns the second-round thinking assertion for a
// protocol: HasThinking on wires that echo reasoning back, HasNoThinking on
// Responses (which by design must not resend previous-turn reasoning). m is a
// gomega matcher (ContainSubstring, ...) — the mockllm.Matcher interface is
// satisfied by every gomega matcher.
func thinkingRoundTripRequire(p mockllm.Protocol, m mockllm.Matcher) mockllm.RequireFunc {
	if p == mockllm.ProtocolOpenAIResponses {
		return mockllm.HasNoThinking()
	}
	return mockllm.HasThinking(m)
}

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
		ginkgo.Entry("OpenAI Responses", mockllm.ProtocolOpenAIResponses),
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
					// Responses 线不回传 thinking → 负向断言(HasNoThinking)。
					// usage 带 cache 计数 —— 第二轮历史命中缓存是真实场景
					Require: thinkingRoundTripRequire(p, gomega.ContainSubstring("让我查一下目录")),
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
			if p == mockllm.ProtocolAnthropic || p == mockllm.ProtocolOpenAIResponses {
				// cache creation 只在 Anthropic（cache_creation_input_tokens）
				// 与 Responses（input_tokens_details.cache_write_tokens）线格式上表达
				gomega.Expect(tcUsage.CacheCreationInputTokens).To(gomega.Equal(int64(40)))
			} else {
				gomega.Expect(tcUsage.CacheCreationInputTokens).To(gomega.Equal(int64(0)))
			}
		},
		ginkgo.Entry("OpenAI", mockllm.ProtocolOpenAI),
		ginkgo.Entry("Anthropic", mockllm.ProtocolAnthropic),
		ginkgo.Entry("OpenAI Responses", mockllm.ProtocolOpenAIResponses),
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
		ginkgo.Entry("OpenAI Responses", mockllm.ProtocolOpenAIResponses),
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
		ginkgo.Entry("OpenAI Responses", mockllm.ProtocolOpenAIResponses),
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
		ginkgo.Entry("OpenAI Responses", mockllm.ProtocolOpenAIResponses),
	)

	ginkgo.DescribeTable("spec.timeout 生效: 慢响应快速失败而非挂起",
		func(_ ginkgo.SpecContext, p mockllm.Protocol) {
			// Hold 保持响应打开直到客户端中止 —— 模拟"永远慢"的上游。
			// 超时发生在流建立之后(headers 已返回、body 挂起), 属于
			// mid-stream 失败: RetryProvider 按设计不重试流中途错误,
			// anthropic/openai-res SDK 超时也直接返回 → 都是 1 次请求。
			// 放 3 个 step 仅作守卫: 若未来某条路径开始重试超时, 脚本
			// 未耗尽(不触发 mock.Error), 请求数断言会先行失败。
			mock, home := startMock(p, []mockllm.Step{
				{Reply: mockllm.Hold()},
				{Reply: mockllm.Hold()},
				{Reply: mockllm.Hold()},
			}, harness.WithSpecTimeout(200*time.Millisecond))
			workdir := harness.SeedWorkDir(ginkgo.GinkgoT(), nil)

			start := time.Now()
			res := run.Binary(bin, workdir, "--home", home, "-p", "hi", "-o", "json-stream")
			elapsed := time.Since(start)

			gomega.Expect(res.Err).NotTo(gomega.HaveOccurred())
			// ExitReasonError → exit 1 (exitCodeForReason)
			gomega.Expect(res.ExitCode).To(gomega.Equal(1))
			events := run.ParseNDJSON(res.Stdout)

			// 超时 → 没有 turn_complete, 只有 error 事件(消息含超时关键字)
			gomega.Expect(events).NotTo(gomega.ContainElement(gomega.HaveField("Type", "turn_complete")))
			gomega.Expect(events).To(gomega.ContainElement(gomega.SatisfyAll(
				gomega.HaveField("Type", "error"),
				gomega.HaveField("Error", gomega.SatisfyAny(
					gomega.ContainSubstring("deadline exceeded"),
					gomega.ContainSubstring("Client.Timeout"),
				)),
			)))

			// 恰好 1 个请求; Hold 被客户端中止, mock 不产生错误
			gomega.Expect(mock.Requests()).To(gomega.HaveLen(1))
			gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())

			// 超时生效的核心证据: 请求在 ~200ms 内被断开而不是永久
			// 挂起(无 timeout 配置时 Hold 永不返回, 进程不会退出)。
			gomega.Expect(elapsed).To(gomega.BeNumerically("<", 5*time.Second))
		},
		ginkgo.Entry("OpenAI", mockllm.ProtocolOpenAI, ginkgo.SpecTimeout(15*time.Second)),
		ginkgo.Entry("Anthropic", mockllm.ProtocolAnthropic, ginkgo.SpecTimeout(15*time.Second)),
		ginkgo.Entry("OpenAI Responses", mockllm.ProtocolOpenAIResponses, ginkgo.SpecTimeout(15*time.Second)),
	)
})
