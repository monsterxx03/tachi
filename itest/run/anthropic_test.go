//go:build integration

package run_test

// Anthropic protocol scenarios: the SAME script shapes as the OpenAI
// scenarios, served by mockllm.NewServer(ProtocolAnthropic) with a
// type: anthropic provider. This locks "same scenario, both providers
// behave identically" (docs §3.2) and exercises the thinking-block
// signature round-trip — the agent echoes thinking blocks (with signature)
// back into the next request's history.

import (
	"github.com/monsterxx03/tachi/itest/harness"
	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/monsterxx03/tachi/itest/run"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

// anthropicServer starts a ProtocolAnthropic mock wired into an isolated
// home with a type: anthropic provider fixture.
func anthropicServer(steps []mockllm.Step, opts ...harness.Option) (*mockllm.Server, string) {
	mock := mockllm.NewServer(mockllm.WithProtocol(mockllm.ProtocolAnthropic))
	ginkgo.DeferCleanup(mock.Close)
	mock.Script(steps...)
	home := harness.NewHome(ginkgo.GinkgoT(), mock,
		append([]harness.Option{harness.WithProtocol(mockllm.ProtocolAnthropic)}, opts...)...)
	return mock, home
}

var _ = ginkgo.Describe("-p pipe mode (Anthropic protocol)", func() {
	ginkgo.It("基础消息流: thinking(含 signature) + text + turn_complete(stop) + exit 0", func() {
		mock, home := anthropicServer([]mockllm.Step{{Reply: mockllm.Stream(
			mockllm.Thinking("让我思考一下"),
			mockllm.Text("你好,Anthropic!"),
			mockllm.Usage(100, 20),
			mockllm.Finish("stop"),
			mockllm.Done(),
		)}})
		workdir := harness.SeedWorkDir(ginkgo.GinkgoT(), nil)

		res := run.Binary(bin, workdir, "--home", home, "-p", "你好", "-o", "json-stream")

		gomega.Expect(res.Err).NotTo(gomega.HaveOccurred())
		gomega.Expect(res.ExitCode).To(gomega.Equal(0))
		events := run.ParseNDJSON(res.Stdout)

		gomega.Expect(run.TextDelta(events)).To(gomega.ContainSubstring("你好,Anthropic!"))
		gomega.Expect(events).To(gomega.ContainElement(gomega.HaveField("Type", "thinking_delta")))
		gomega.Expect(events).To(gomega.ContainElement(gomega.SatisfyAll(
			gomega.HaveField("Type", "turn_complete"),
			gomega.HaveField("ExitReason", "stop"),
		)))
		gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())
		gomega.Expect(mock.Requests()).To(gomega.HaveLen(1))
	})

	ginkgo.It("工具调用循环: thinking 带 signature 回传 + tool result 回传第二次请求", func() {
		mock, home := anthropicServer(
			[]mockllm.Step{
				{Reply: mockllm.Stream(
					mockllm.Thinking("让我查一下目录"),
					mockllm.ToolCallStart("call_1", "Bash", `{"command":"ls"}`),
					mockllm.Usage(120, 30),
					mockllm.Finish("tool_calls"),
					mockllm.Done(),
				)},
				{
					// 第二轮请求必须同时回传 thinking(含 signature)与工具结果
					Require: mockllm.HasThinking(gomega.ContainSubstring("让我查一下目录")),
					Reply: mockllm.Stream(
						mockllm.Text("目录里有 README.md。"),
						mockllm.Usage(200, 20),
						mockllm.Finish("stop"),
						mockllm.Done(),
					),
				},
			},
			harness.WithBashAllow(),
		)
		workdir := harness.SeedWorkDir(ginkgo.GinkgoT(), map[string]string{"README.md": "# Fixture\n"})

		res := run.Binary(bin, workdir, "--home", home, "-p", "看一下当前目录", "-o", "json-stream")

		gomega.Expect(res.Err).NotTo(gomega.HaveOccurred())
		gomega.Expect(res.ExitCode).To(gomega.Equal(0))
		events := run.ParseNDJSON(res.Stdout)

		gomega.Expect(events).To(gomega.ContainElement(gomega.SatisfyAll(
			gomega.HaveField("Type", "tool_result"),
			gomega.HaveField("ToolCallID", "call_1"),
			gomega.HaveField("IsError", false),
		)))
		gomega.Expect(events).To(gomega.ContainElement(gomega.SatisfyAll(
			gomega.HaveField("Type", "turn_complete"),
			gomega.HaveField("ExitReason", "stop"),
		)))

		gomega.Expect(mock.Requests()).To(gomega.HaveLen(2))
		// 工具结果回传(Anthropic tool_result 块 → 归一化 tool 消息)
		gomega.Expect(mock.Requests()[1].Messages).To(gomega.ContainElement(gomega.SatisfyAll(
			gomega.HaveField("Role", "tool"),
			gomega.HaveField("ToolCallID", "call_1"),
			gomega.HaveField("Content", gomega.ContainSubstring("README.md")),
		)))
		gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("错误路径/重试: 429 两次后成功 (Anthropic SDK 内部重试)", func() {
		mock, home := anthropicServer(
			[]mockllm.Step{
				{Reply: mockllm.StatusError(429, "rate limited")},
				{Reply: mockllm.StatusError(429, "rate limited")},
				{Reply: mockllm.Stream(
					mockllm.Text("SDK 重试后成功"),
					mockllm.Usage(50, 10),
					mockllm.Finish("stop"),
					mockllm.Done(),
				)},
			},
		)
		workdir := harness.SeedWorkDir(ginkgo.GinkgoT(), nil)

		res := run.Binary(bin, workdir, "--home", home, "-p", "hi", "-o", "json-stream")

		gomega.Expect(res.Err).NotTo(gomega.HaveOccurred())
		gomega.Expect(res.ExitCode).To(gomega.Equal(0))
		// anthropic-sdk-go 内部对 429 重试(默认 MaxRetries=2)
		gomega.Expect(mock.Requests()).To(gomega.HaveLen(3))
		gomega.Expect(run.TextDelta(run.ParseNDJSON(res.Stdout))).To(gomega.ContainSubstring("SDK 重试后成功"))
		gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())
	})
})
