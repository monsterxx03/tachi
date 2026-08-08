//go:build integration

package tui_test

// M2 交互覆盖 (docs §十): 流式期间输入自动排队发送、Ctrl+C 取消、AskUser
// 表单、/model 切换、/sessions 列表、会话持久化 + --resume。这些场景验证
// pendingQueue / steer / 状态机切换 / 表单等跨层交互 —— 单测覆盖不到的部分。

import (
	"time"

	"github.com/monsterxx03/tachi/itest/harness"
	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/monsterxx03/tachi/itest/tui"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

var _ = ginkgo.Describe("TUI 交互", func() {
	ginkgo.It("M2.1 流式期间输入 → ⏳ pending → 回合结束自动发送", func(_ ginkgo.SpecContext) {
		mock, home := startMock(mockllm.ProtocolOpenAI, []mockllm.Step{
			{Reply: mockllm.Stream(
				mockllm.Text("第一轮回答"),
				// 长停顿给测试留出流式期间的输入窗口
				mockllm.Pause(1500*time.Millisecond),
				mockllm.Usage(100, 20),
				mockllm.Finish("stop"),
				mockllm.Done(),
			)},
			{
				// 回合结束后 pending 消息必须作为第二条用户消息自动发出
				Require: mockllm.HasUserMessage(gomega.ContainSubstring("第二条消息")),
				Reply: mockllm.Stream(
					mockllm.Text("第二轮回答"),
					mockllm.Usage(150, 10),
					mockllm.Finish("stop"),
					mockllm.Done(),
				),
			},
		})
		s := launch(home, mock)

		s.Type("第一条")
		s.Enter()

		// 流式已开始, 此时输入不会打断当前回合
		s.Expect("第一轮回答", specTimeout)
		s.Type("第二条消息")
		s.Enter()
		// 状态栏出现 pending 计数
		s.Expect("⏳ 1 pending", specTimeout)

		// 回合结束 → pending 自动发送 → 第二轮请求带回第二条用户消息
		s.Expect("第二轮回答", specTimeout)
		s.WaitIdle(2, specTimeout)

		gomega.Expect(mock.Requests()).To(gomega.HaveLen(2))
		gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("M2.2 Ctrl+C 取消进行中的回合", func(_ ginkgo.SpecContext) {
		mock, home := startMock(mockllm.ProtocolOpenAI, []mockllm.Step{
			{Reply: mockllm.Stream(
				mockllm.Text("开始处理"),
				mockllm.Pause(30*time.Second),
				mockllm.Text("不应出现"),
				mockllm.Finish("stop"),
				mockllm.Done(),
			)},
			{Reply: mockllm.Stream(
				mockllm.Text("取消后可以继续"),
				mockllm.Usage(50, 10),
				mockllm.Finish("stop"),
				mockllm.Done(),
			)},
		})
		s := launch(home, mock)

		s.Type("hi")
		s.Enter()

		// 等回合开始流式输出后取消
		s.Expect("开始处理", specTimeout)
		s.Key("ctrl+c")

		// 取消后回到空闲, pause 后的 chunk 不应到达
		s.WaitIdle(1, specTimeout)
		gomega.Expect(s.Screen()).NotTo(gomega.ContainSubstring("不应出现"))

		// 不挂死: 取消后可以继续对话
		s.Type("再来")
		s.Enter()
		s.Expect("取消后可以继续", specTimeout)
		s.WaitIdle(2, specTimeout)

		gomega.Expect(mock.Requests()).To(gomega.HaveLen(2))
		gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("M2.3 AskUserQuestion 表单: 输入回答 → agent 继续", func(_ ginkgo.SpecContext) {
		mock, home := startMock(mockllm.ProtocolOpenAI, []mockllm.Step{
			{Reply: mockllm.Stream(
				mockllm.ToolCallStart("ask_1", "AskUserQuestion",
					`{"questions":[{"question":"你最喜欢的编程语言?","options":[]}]}`),
				mockllm.Usage(120, 30),
				mockllm.Finish("tool_calls"),
				mockllm.Done(),
			)},
			{
				// agent 把表单答案作为工具结果回传
				Require: mockllm.HasToolResult("ask_1", gomega.ContainSubstring("Go")),
				Reply: mockllm.Stream(
					mockllm.Text("好的, Go 语言很棒。"),
					mockllm.Usage(200, 20),
					mockllm.Finish("stop"),
					mockllm.Done(),
				),
			},
		})
		s := launch(home, mock)

		s.Type("问你个问题")
		s.Enter()

		// 表单渲染问题文本（无选项 → 直接进入自由文本编辑）
		s.Expect("你最喜欢的编程语言?", specTimeout)
		s.Type("Go")
		s.Enter()

		s.Expect("好的, Go 语言很棒。", specTimeout)
		s.WaitIdle(2, specTimeout)

		gomega.Expect(mock.Requests()).To(gomega.HaveLen(2))
		gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("M2.4 /model 切换 provider", func(_ ginkgo.SpecContext) {
		mock, home := startMock(mockllm.ProtocolOpenAI, []mockllm.Step{{
			Reply: mockllm.Stream(
				mockllm.Text("切换后回答"),
				mockllm.Usage(50, 10),
				mockllm.Finish("stop"),
				mockllm.Done(),
			),
		}}, harness.WithSecondProvider("mock2-model"))
		s := launch(home, mock)

		s.Command("/model")

		// 选择器列出两个 provider
		s.Expect("Select provider (↑↓ Enter Esc)", specTimeout)
		gomega.Eventually(s.Screen, 10*time.Second).Should(gomega.ContainSubstring("mock2"))
		gomega.Eventually(s.Screen, 10*time.Second).Should(gomega.ContainSubstring("mock-model"))

		// 选中第二个并确认
		s.Key("down")
		s.Enter()

		// 状态栏 provider 更新为 mock2-model
		gomega.Eventually(s.Screen, 10*time.Second).Should(gomega.ContainSubstring("openai (mock2-model)"))

		// 切换后正常对话
		s.Type("hi")
		s.Enter()
		s.Expect("切换后回答", specTimeout)
		s.WaitIdle(1, specTimeout)
		gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("M2.5 /sessions 列表并退出", func(_ ginkgo.SpecContext) {
		mock, home := startMock(mockllm.ProtocolOpenAI, []mockllm.Step{{
			Reply: mockllm.Stream(
				mockllm.Text("会话一的回答"),
				mockllm.Usage(50, 10),
				mockllm.Finish("stop"),
				mockllm.Done(),
			),
		}})
		s := launch(home, mock)

		// 先产生一个会话（标题 = 首条用户消息）
		s.Type("会话一")
		s.Enter()
		s.WaitIdle(1, specTimeout)

		s.Command("/sessions")
		s.Expect("Sessions (↑↓ Enter Esc)", specTimeout)
		gomega.Eventually(s.Screen, 10*time.Second).Should(gomega.ContainSubstring("会话一"))

		// Esc 退出选择器回到对话
		s.Key("esc")
		gomega.Eventually(s.Screen, 10*time.Second).Should(gomega.ContainSubstring("会话一的回答"))
		gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("M2.6 会话持久化 + --resume 恢复历史", func(_ ginkgo.SpecContext) {
		mock, home := startMock(mockllm.ProtocolOpenAI, []mockllm.Step{{
			Reply: mockllm.Stream(
				mockllm.Text("持久化的回答"),
				mockllm.Usage(50, 10),
				mockllm.Finish("stop"),
				mockllm.Done(),
			),
		}})

		// 第一次启动: 对话一轮后退出
		s1 := launch(home, mock)
		s1.Type("持久化测试")
		s1.Enter()
		s1.WaitIdle(1, specTimeout)
		s1.Stop()

		// 第二次启动: --resume → 会话选择器列出已存会话
		s2 := launch(home, mock, tui.WithResume())
		s2.Expect("Sessions (↑↓ Enter Esc)", specTimeout)
		gomega.Eventually(s2.Screen, 10*time.Second).Should(gomega.ContainSubstring("持久化测试"))

		// 选中会话 → 历史加载, 之前的回答渲染回聊天区
		s2.Enter()
		gomega.Eventually(s2.Screen, 10*time.Second).Should(gomega.ContainSubstring("持久化的回答"))
	})
})
