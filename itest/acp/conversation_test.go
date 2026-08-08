//go:build integration

package acp_test

// L1 — the conversation core over the real ACP wire, table-driven across
// all three mockllm protocols (OpenAI / Anthropic / OpenAI Responses): a
// prompt turn streams AgentThoughtChunk + AgentMessageChunk updates and
// finishes with the mapped stop reason; tool calls go through the editor
// permission flow and execute Bash for real; transient 429s retry; a Cancel
// notification aborts the turn mid-stream.

import (
	"context"
	"strings"
	"time"

	acpapi "github.com/coder/acp-go-sdk"
	"github.com/monsterxx03/tachi/itest/acp"
	"github.com/monsterxx03/tachi/itest/harness"
	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

// startACPTurn starts a protocol-specific mock + client, opens a session in
// a seeded workdir (Bash side effects land there), and returns the trio.
// hOpts are extra harness options (e.g. WithBashAsk to force the permission
// flow); opts are client options (permission policy).
func startACPTurn(p mockllm.Protocol, steps []mockllm.Step, seeds map[string]string, hOpts []harness.Option, opts ...acp.Option) (*acp.Client, *mockllm.Server, acpapi.SessionId, string) {
	mock := mockllm.NewServer(mockllm.WithProtocol(p))
	ginkgo.DeferCleanup(mock.Close)
	mock.Script(steps...)
	home := harness.NewHome(ginkgo.GinkgoT(), mock,
		append([]harness.Option{harness.WithProtocol(p)}, hOpts...)...)
	workdir := harness.SeedWorkDir(ginkgo.GinkgoT(), seeds)

	client, err := acp.Start(bin, home, opts...)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	ginkgo.DeferCleanup(client.Close)

	sid, err := newSession(client, workdir)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return client, mock, sid, workdir
}

// prompt sends one turn and returns the response (asserting no transport
// error — a JSON-RPC failure surfaces as a test failure here).
func prompt(client *acp.Client, sid acpapi.SessionId, text string) acpapi.PromptResponse {
	resp, err := client.Conn().Prompt(context.Background(), acpapi.PromptRequest{
		SessionId: sid,
		Prompt:    []acpapi.ContentBlock{acpapi.TextBlock(text)},
	})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return resp
}

var _ = ginkgo.Describe("ACP conversation core (triple protocol)", func() {
	ginkgo.DescribeTable("prompt 基础流: thinking + text 增量 → end_turn",
		func(p mockllm.Protocol) {
			client, mock, sid, _ := startACPTurn(p, []mockllm.Step{{
				Reply: mockllm.Stream(
					mockllm.Thinking("让我想想"),
					mockllm.Text("你好,世界!"),
					mockllm.Usage(100, 20),
					mockllm.Finish("stop"),
					mockllm.Done(),
				),
			}}, nil, nil)

			resp := prompt(client, sid, "你好")

			// 流式增量通过 SessionUpdate 通知到达
			gomega.Expect(client.Rec.Thoughts()).To(gomega.ContainSubstring("让我想想"))
			gomega.Expect(client.Rec.Text()).To(gomega.ContainSubstring("你好,世界!"))
			// 终态: stop → end_turn, usage 回传
			gomega.Expect(resp.StopReason).To(gomega.Equal(acpapi.StopReasonEndTurn))
			gomega.Expect(resp.Usage).NotTo(gomega.BeNil())
			gomega.Expect(resp.Usage.InputTokens).To(gomega.Equal(100))

			gomega.Expect(mock.Requests()).To(gomega.HaveLen(1))
			gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())
		},
		ginkgo.Entry("OpenAI", mockllm.ProtocolOpenAI),
		ginkgo.Entry("Anthropic", mockllm.ProtocolAnthropic),
		ginkgo.Entry("OpenAI Responses", mockllm.ProtocolOpenAIResponses),
	)

	ginkgo.DescribeTable("工具调用循环: 权限流 + Bash 真实执行 + 结果回传",
		func(p mockllm.Protocol) {
			client, mock, sid, _ := startACPTurn(p, []mockllm.Step{
				{Reply: mockllm.Stream(
					mockllm.ToolCallStart("call_1", "Bash", `{"command":"ls"}`),
					mockllm.Usage(120, 30),
					mockllm.Finish("tool_calls"),
					mockllm.Done(),
				)},
				{
					// 第二轮请求必须回传工具结果
					Require: mockllm.HasToolResult("call_1", gomega.ContainSubstring("README.md")),
					Reply: mockllm.Stream(
						mockllm.Text("目录里有 README.md。"),
						mockllm.Usage(200, 20),
						mockllm.Finish("stop"),
						mockllm.Done(),
					),
				},
			}, map[string]string{"README.md": "# Fixture\n"}, []harness.Option{harness.WithBashAsk()})

			resp := prompt(client, sid, "看一下当前目录")

			// 工具调用经 editor 权限流批准后真实执行
			gomega.Expect(client.Rec.ToolCallIDs()).To(gomega.ContainElement(acpapi.ToolCallId("call_1")))
			gomega.Expect(client.PermissionRequests()).NotTo(gomega.BeEmpty())
			gomega.Expect(client.Rec.Text()).To(gomega.ContainSubstring("目录里有 README.md。"))
			gomega.Expect(resp.StopReason).To(gomega.Equal(acpapi.StopReasonEndTurn))

			// agent loop 确实把工具结果回传了第二次请求
			gomega.Expect(mock.Requests()).To(gomega.HaveLen(2))
			gomega.Expect(mock.Requests()[1].Messages).To(gomega.ContainElement(gomega.SatisfyAll(
				gomega.HaveField("Role", "tool"),
				gomega.HaveField("ToolCallID", "call_1"),
				gomega.HaveField("Content", gomega.ContainSubstring("README.md")),
			)))
			gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())
		},
		ginkgo.Entry("OpenAI", mockllm.ProtocolOpenAI),
		ginkgo.Entry("Anthropic", mockllm.ProtocolAnthropic),
		ginkgo.Entry("OpenAI Responses", mockllm.ProtocolOpenAIResponses),
	)

	ginkgo.DescribeTable("429 两次后重试成功",
		func(p mockllm.Protocol) {
			client, mock, sid, _ := startACPTurn(p, []mockllm.Step{
				{Reply: mockllm.StatusError(429, "rate limited")},
				{Reply: mockllm.StatusError(429, "rate limited")},
				{Reply: mockllm.Stream(
					mockllm.Text("重试后终于成功了"),
					mockllm.Usage(50, 10),
					mockllm.Finish("stop"),
					mockllm.Done(),
				)},
			}, nil, nil)

			resp := prompt(client, sid, "hi")

			gomega.Expect(resp.StopReason).To(gomega.Equal(acpapi.StopReasonEndTurn))
			gomega.Expect(client.Rec.Text()).To(gomega.ContainSubstring("重试后终于成功了"))
			// 初始 + 2 次重试 = 3 个请求(三条协议各自的 retry 语义一致)
			gomega.Expect(mock.Requests()).To(gomega.HaveLen(3))
			gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())
		},
		ginkgo.Entry("OpenAI", mockllm.ProtocolOpenAI),
		ginkgo.Entry("Anthropic", mockllm.ProtocolAnthropic),
		ginkgo.Entry("OpenAI Responses", mockllm.ProtocolOpenAIResponses),
	)

	ginkgo.DescribeTable("cancel 中断进行中的 turn",
		func(_ ginkgo.SpecContext, p mockllm.Protocol) {
			client, mock, sid, _ := startACPTurn(p, []mockllm.Step{{
				// 长 pause 模拟慢生成; 之后的 chunk 在 cancel 后绝不应到达
				Reply: mockllm.Stream(
					mockllm.Text("开始处理"),
					mockllm.Pause(30*time.Second),
					mockllm.Text("不应出现"),
					mockllm.Finish("stop"),
					mockllm.Done(),
				),
			}}, nil, nil)

			promptDone := make(chan struct{})
			var resp acpapi.PromptResponse
			var promptErr error
			go func() {
				resp, promptErr = client.Conn().Prompt(context.Background(), acpapi.PromptRequest{
					SessionId: sid,
					Prompt:    []acpapi.ContentBlock{acpapi.TextBlock("hi")},
				})
				close(promptDone)
			}()

			// 等 turn 开始流式输出后取消
			gomega.Eventually(func() string { return client.Rec.Text() }, 5*time.Second).
				Should(gomega.ContainSubstring("开始处理"))
			gomega.Expect(client.Conn().Cancel(context.Background(), acpapi.CancelNotification{
				SessionId: sid,
			})).NotTo(gomega.HaveOccurred())

			<-promptDone
			gomega.Expect(promptErr).NotTo(gomega.HaveOccurred())
			gomega.Expect(resp.StopReason).To(gomega.Equal(acpapi.StopReasonCancelled))
			// pause 后的 chunk 未到达 → 输出停在 cancel 点
			gomega.Expect(strings.Contains(client.Rec.Text(), "不应出现")).To(gomega.BeFalse())
			// 脚本未被耗尽(cancel 中断了循环), mock 无错误
			gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())
		},
		ginkgo.Entry("OpenAI", mockllm.ProtocolOpenAI, ginkgo.SpecTimeout(15*time.Second)),
		ginkgo.Entry("Anthropic", mockllm.ProtocolAnthropic, ginkgo.SpecTimeout(15*time.Second)),
		ginkgo.Entry("OpenAI Responses", mockllm.ProtocolOpenAIResponses, ginkgo.SpecTimeout(15*time.Second)),
	)
})
