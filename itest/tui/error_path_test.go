//go:build integration

package tui_test

// M1.3 错误路径 (docs §十): 429 重试（OpenAI RetryProvider / Anthropic SDK
// 内部重试 / Responses 线各自的 retry 语义）与流中断 —— 断言重试成功后渲染
// 正常，以及流损坏时 TUI 展示错误且不挂死（错误后仍可继续对话）。

import (
	"time"

	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

var _ = ginkgo.Describe("TUI 错误路径", func() {
	ginkgo.DescribeTable("429 两次后重试成功",
		func(_ ginkgo.SpecContext, p mockllm.Protocol) {
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
			s := launch(home, mock)

			s.Type("hi")
			s.Enter()

			s.Expect("重试后终于成功了", specTimeout)
			// 初始 + 2 次重试 = 3 个请求（三条协议各自的 retry 语义一致）
			s.WaitIdle(3, specTimeout)

			gomega.Expect(mock.Requests()).To(gomega.HaveLen(3))
			gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())
		},
		ginkgo.Entry("OpenAI", mockllm.ProtocolOpenAI, ginkgo.SpecTimeout(90*time.Second)),
		ginkgo.Entry("Anthropic", mockllm.ProtocolAnthropic, ginkgo.SpecTimeout(90*time.Second)),
		ginkgo.Entry("OpenAI Responses", mockllm.ProtocolOpenAIResponses, ginkgo.SpecTimeout(90*time.Second)),
	)

	ginkgo.DescribeTable("流中断: 错误展示 + UI 不挂死可继续对话",
		func(_ ginkgo.SpecContext, p mockllm.Protocol) {
			mock, home := startMock(p, []mockllm.Step{
				{Reply: mockllm.MalformedSSE()},
				{Reply: mockllm.Stream(
					mockllm.Text("第二次正常回答"),
					mockllm.Usage(50, 10),
					mockllm.Finish("stop"),
					mockllm.Done(),
				)},
			})
			s := launch(home, mock)

			s.Type("hi")
			s.Enter()

			// 流损坏 → TUI 以 "Error:" 角色消息展示并回到空闲
			s.Expect("Error:", specTimeout)
			s.WaitIdle(1, specTimeout)

			// 不挂死: 错误后仍可输入, 第二个脚本步骤正常完成
			s.Type("再来")
			s.Enter()
			s.Expect("第二次正常回答", specTimeout)
			s.WaitIdle(2, specTimeout)

			gomega.Expect(mock.Requests()).To(gomega.HaveLen(2))
			gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())
		},
		ginkgo.Entry("OpenAI", mockllm.ProtocolOpenAI, ginkgo.SpecTimeout(90*time.Second)),
		ginkgo.Entry("Anthropic", mockllm.ProtocolAnthropic, ginkgo.SpecTimeout(90*time.Second)),
		ginkgo.Entry("OpenAI Responses", mockllm.ProtocolOpenAIResponses, ginkgo.SpecTimeout(90*time.Second)),
	)
})
