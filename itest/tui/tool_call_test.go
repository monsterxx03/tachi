//go:build integration

package tui_test

// M1.2 工具调用循环 (docs §十): tool_call → 工具真实执行 → tool result 回传
// → 下一轮 text。断言面: 工具行从 `~ Bash(ls)`（运行中）变为 `v Bash(ls)`
// （完成）+ 结果摘要，最终回答渲染，mock 第二轮请求回传 tool result。
// M2.7 多工具并行调用: 一个响应两个 tool_call（三条协议线均支持 —— OpenAI
// 流式按 tool_calls[] index 区分、Anthropic 按 content block index 区分、
// Responses 按 item 区分，契约测试已锁定）。

import (
	"time"

	"github.com/monsterxx03/tachi/itest/harness"
	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/monsterxx03/tachi/itest/tui"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

var _ = ginkgo.Describe("TUI 工具调用循环", func() {
	ginkgo.DescribeTable("~ Bash → v Bash → 结果回传 + 最终回答",
		func(_ ginkgo.SpecContext, p mockllm.Protocol) {
			mock, home := startMock(p, []mockllm.Step{
				{Reply: mockllm.Stream(
					mockllm.Thinking("让我查一下目录"),
					mockllm.ToolCallStart("call_1", "Bash", `{"command":"ls"}`),
					// 停顿让 `~ Bash(ls)` 运行态可稳定观察（渲染轮询 20ms）
					mockllm.Pause(200*time.Millisecond),
					mockllm.Usage(120, 30),
					mockllm.Finish("tool_calls"),
					mockllm.Done(),
				)},
				{
					// 第二轮请求必须回传工具结果（含 ls 输出）
					Require: mockllm.HasToolResult("call_1", gomega.ContainSubstring("README.md")),
					Reply: mockllm.Stream(
						mockllm.Text("目录里有 README.md。"),
						mockllm.Usage(200, 20),
						mockllm.Finish("stop"),
						mockllm.Done(),
					),
				},
			}, harness.WithBashAllow())
			s := launch(home, mock, tui.WithSeedFiles(map[string]string{"README.md": "# Fixture\n"}))

			s.Type("看一下当前目录")
			s.Enter()

			// 工具运行中（~ 行，preview 在流结束才应用——Responses 线下
			// "~ Bash(ls)" 只有 ~10ms 窗口，用 "~ Bash(" 稳定断言运行态）
			s.Expect("~ Bash(", specTimeout)
			// 完成态 v 行带完整 preview（稳定）
			s.Expect("v Bash(ls)", specTimeout)
			s.Expect("README.md", specTimeout)
			s.WaitIdle(2, specTimeout)

			// agent loop 确实把工具结果回传了第二次请求
			gomega.Expect(mock.Requests()).To(gomega.HaveLen(2))
			gomega.Expect(mock.Requests()[1].Messages).To(gomega.ContainElement(gomega.SatisfyAll(
				gomega.HaveField("Role", "tool"),
				gomega.HaveField("ToolCallID", "call_1"),
				gomega.HaveField("Content", gomega.ContainSubstring("README.md")),
			)))
			gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())
		},
		ginkgo.Entry("OpenAI", mockllm.ProtocolOpenAI, ginkgo.SpecTimeout(90*time.Second)),
		ginkgo.Entry("Anthropic", mockllm.ProtocolAnthropic, ginkgo.SpecTimeout(90*time.Second)),
		ginkgo.Entry("OpenAI Responses", mockllm.ProtocolOpenAIResponses, ginkgo.SpecTimeout(90*time.Second)),
	)

	ginkgo.DescribeTable("多工具并行调用: 一个响应两个 tool_call 同时显示并回传",
		func(_ ginkgo.SpecContext, p mockllm.Protocol) {
			mock, home := startMock(p, []mockllm.Step{
				{Reply: mockllm.Stream(
					mockllm.ToolCallStart("call_1", "Bash", `{"command":"echo one"}`),
					mockllm.ToolCallStart("call_2", "Bash", `{"command":"echo two"}`),
					// 停顿让两个 `~ Bash(...)` 运行态可稳定观察（exec 只要 ~7ms，
					// 轮询 20ms 抓不到瞬态）
					mockllm.Pause(300*time.Millisecond),
					mockllm.Usage(120, 30),
					mockllm.Finish("tool_calls"),
					mockllm.Done(),
				)},
				{
					Require: mockllm.HasToolResult("call_1", gomega.ContainSubstring("one")),
					Reply: mockllm.Stream(
						mockllm.Text("两个命令都执行完了。"),
						mockllm.Usage(200, 20),
						mockllm.Finish("stop"),
						mockllm.Done(),
					),
				},
			}, harness.WithBashAllow())
			s := launch(home, mock)

			s.Type("并行执行")
			s.Enter()

			// 两个工具同时进入运行态（preview 流末才应用，见 M1.2 注释）
			s.Expect("~ Bash(", specTimeout)
			// 完成态两个 v 行各带 preview —— 证明两个 tool_call 都真实执行
			s.Expect("v Bash(echo one)", specTimeout)
			s.Expect("v Bash(echo two)", specTimeout)
			s.Expect("两个命令都执行完了。", specTimeout)
			s.WaitIdle(2, specTimeout)

			// 第二轮同时回传两个 tool result
			gomega.Expect(mock.Requests()).To(gomega.HaveLen(2))
			msgs := mock.Requests()[1].Messages
			gomega.Expect(msgs).To(gomega.ContainElement(gomega.SatisfyAll(
				gomega.HaveField("Role", "tool"), gomega.HaveField("ToolCallID", "call_1"))))
			gomega.Expect(msgs).To(gomega.ContainElement(gomega.SatisfyAll(
				gomega.HaveField("Role", "tool"), gomega.HaveField("ToolCallID", "call_2"))))
			gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())
		},
		ginkgo.Entry("OpenAI", mockllm.ProtocolOpenAI, ginkgo.SpecTimeout(90*time.Second)),
		ginkgo.Entry("Anthropic", mockllm.ProtocolAnthropic, ginkgo.SpecTimeout(90*time.Second)),
		ginkgo.Entry("OpenAI Responses", mockllm.ProtocolOpenAIResponses, ginkgo.SpecTimeout(90*time.Second)),
	)
})
