//go:build integration

package tui_test

// M1.1 基础消息流 (docs §十): thinking → text → stop 全链路 — 进程内 TUI
// 驱动真实 Model + 真实 AIAgent，同一脚本在三协议下行为一致。断言面：
//   1. Screen() 渲染思考与回答（虚拟终端重建，与真实终端一致）
//   2. WaitIdle —— 主锚点是 mock 脚本被完整消费（收到 1 个请求）+ 状态栏回空闲
//   3. 会话文件落盘（home/session/<id>/messages.jsonl 含用户消息）
//   4. 状态栏显示 provider 信息（openai (mock-model)）

import (
	"os"
	"time"

	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

var _ = ginkgo.Describe("TUI 基础消息流", func() {
	ginkgo.DescribeTable("thinking → text → stop: 渲染 + 会话落盘 + 状态栏回空闲",
		func(_ ginkgo.SpecContext, p mockllm.Protocol) {
			mock, home := startMock(p, []mockllm.Step{{
				Reply: mockllm.Stream(
					mockllm.Thinking("让我想想"),
					mockllm.Text("你好,世界!"),
					mockllm.Usage(100, 20),
					mockllm.Finish("stop"),
					mockllm.Done(),
				),
			}})
			s := launch(home, mock)

			s.Type("你好")
			s.Enter()

			// 思考与回答都渲染到了屏幕（单 chunk 流式，虚拟终端可稳定断言）
			s.Expect("让我想想", specTimeout)
			s.Expect("你好,世界!", specTimeout)
			s.WaitIdle(1, specTimeout)

			// 状态栏显示 provider 信息（name (model)），会话创建后标题回显
			gomega.Expect(s.Screen()).To(gomega.ContainSubstring(providerInfoFor(p)))

			// agent loop 只打了一次 mock，脚本完整消费
			gomega.Expect(mock.Requests()).To(gomega.HaveLen(1))
			gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())

			// 会话文件落盘：home/session/<id>/messages.jsonl 含用户消息
			files := sessionMessageFiles(home)
			gomega.Expect(files).NotTo(gomega.BeEmpty())
			data, err := os.ReadFile(files[0])
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(string(data)).To(gomega.ContainSubstring("你好"))
		},
		ginkgo.Entry("OpenAI", mockllm.ProtocolOpenAI, ginkgo.SpecTimeout(90*time.Second)),
		ginkgo.Entry("Anthropic", mockllm.ProtocolAnthropic, ginkgo.SpecTimeout(90*time.Second)),
		ginkgo.Entry("OpenAI Responses", mockllm.ProtocolOpenAIResponses, ginkgo.SpecTimeout(90*time.Second)),
	)

	ginkgo.DescribeTable("thinking 多轮回传: 第二轮请求带回 assistant 思考块",
		func(_ ginkgo.SpecContext, p mockllm.Protocol) {
			mock, home := startMock(p, []mockllm.Step{
				{Reply: mockllm.Stream(
					mockllm.Thinking("我在思考第一轮"),
					mockllm.Text("第一步"),
					mockllm.Usage(100, 20),
					mockllm.Finish("stop"),
					mockllm.Done(),
				)},
				{
					// 第二轮：上一轮的 thinking 必须原样回传（Responses 协议
					// 设计上不回传 reasoning，见 mockllm.HasThinking 注释）
					Require: thinkingRoundTrip(p, "我在思考第一轮"),
					Reply: mockllm.Stream(
						mockllm.Text("第二轮回答"),
						mockllm.Usage(150, 10),
						mockllm.Finish("stop"),
						mockllm.Done(),
					),
				},
			})
			s := launch(home, mock)

			s.Type("第一问")
			s.Enter()
			s.WaitIdle(1, specTimeout)

			// 回合结束后自动进入第二轮
			s.Type("第二问")
			s.Enter()
			s.WaitIdle(2, specTimeout)
			s.Expect("第二轮回答", specTimeout)

			gomega.Expect(mock.Requests()).To(gomega.HaveLen(2))
			gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())
		},
		ginkgo.Entry("OpenAI", mockllm.ProtocolOpenAI, ginkgo.SpecTimeout(90*time.Second)),
		ginkgo.Entry("Anthropic", mockllm.ProtocolAnthropic, ginkgo.SpecTimeout(90*time.Second)),
		ginkgo.Entry("OpenAI Responses", mockllm.ProtocolOpenAIResponses, ginkgo.SpecTimeout(90*time.Second)),
	)
})

// thinkingRoundTrip selects the protocol-appropriate thinking round-trip
// assertion: positive (HasThinking) on OpenAI/Anthropic wires, negative
// (HasNoThinking) on the Responses wire which drops reasoning by design.
func thinkingRoundTrip(p mockllm.Protocol, text string) mockllm.RequireFunc {
	if p == mockllm.ProtocolOpenAIResponses {
		return mockllm.HasNoThinking()
	}
	return mockllm.HasThinking(gomega.ContainSubstring(text))
}

var _ = ginkgo.Describe("TUI 启动态", func() {
	ginkgo.It("启动即显示状态栏 provider 与输入占位提示", func() {
		mock, home := startMock(mockllm.ProtocolOpenAI, nil)
		s := launch(home, mock)

		// 初始帧:状态栏(● 空闲) + provider 信息 + 输入框占位
		gomega.Eventually(s.Screen, 10*time.Second).Should(gomega.ContainSubstring("openai (mock-model)"))
		gomega.Eventually(s.Screen, 10*time.Second).Should(gomega.ContainSubstring("Send a message"))
		// 空闲点 ● 在重建屏幕上反映当前状态
		gomega.Eventually(s.Screen, 10*time.Second).Should(gomega.ContainSubstring("● tachi |"))

		// 未发消息 → mock 零请求
		gomega.Expect(mock.RequestCount()).To(gomega.Equal(0))
	})
})
