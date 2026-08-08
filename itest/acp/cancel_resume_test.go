//go:build integration

package acp_test

// Cancel-and-resume edge cases over the real ACP wire: a prompt cancelled at
// DIFFERENT stages (mid-LLM-stream, mid-tool-execution, after the tool result
// was sent but before the next LLM call) must leave the agent loop in a
// resumable state — the next Prompt continues from where the turn was cut
// off, carries NO orphan tool calls (an un-answered tool_use would be
// rejected by the LLM API, Anthropic in particular), and injects the new
// user message in the wire-specific form (OpenAI/Responses: a standalone
// user message; Anthropic: merged into the trailing tool_result user
// message via collectToolMessages).

import (
	"context"
	"time"

	acpapi "github.com/coder/acp-go-sdk"
	"github.com/monsterxx03/tachi/itest/acp"
	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

// startPrompt launches a Prompt in a goroutine and returns a channel closed
// when the response is ready (mirrors conversation_test's cancel pattern).
func startPrompt(client *acp.Client, sid acpapi.SessionId, text string) (done chan struct{}, resp *acpapi.PromptResponse, promptErr *error) {
	done = make(chan struct{})
	var r acpapi.PromptResponse
	var e error
	go func() {
		r, e = client.Conn().Prompt(context.Background(), acpapi.PromptRequest{
			SessionId: sid,
			Prompt:    []acpapi.ContentBlock{acpapi.TextBlock(text)},
		})
		close(done)
	}()
	return done, &r, &e
}

// cancelTurn waits for a condition on the client, cancels the running turn,
// and asserts the cancelled stop reason.
func cancelTurn(done chan struct{}, resp *acpapi.PromptResponse, promptErr *error, client *acp.Client, sid acpapi.SessionId) {
	<-done
	gomega.Expect(*promptErr).NotTo(gomega.HaveOccurred())
	gomega.Expect(resp.StopReason).To(gomega.Equal(acpapi.StopReasonCancelled))
}

var _ = ginkgo.Describe("ACP cancel→resume edge cases (triple protocol)", func() {
	ginkgo.DescribeTable("场景 A: LLM 流式输出中取消 → 继续, 新 user 注入且无残留",
		func(_ ginkgo.SpecContext, p mockllm.Protocol) {
			client, mock, sid, _ := startACPTurn(p, []mockllm.Step{
				{Reply: mockllm.Stream(
					mockllm.Text("第一轮部分输出"),
					mockllm.Pause(30*time.Second),
					mockllm.Text("不应出现"),
					mockllm.Finish("stop"),
					mockllm.Done(),
				)},
				{
					// 继续后的请求: 第一轮 user 消息在 history, 新 prompt 注入
					Require: mockllm.HasUserMessage(gomega.ContainSubstring("第一轮问题")),
					Reply: mockllm.Stream(
						mockllm.Text("继续的回答"),
						mockllm.Usage(20, 10),
						mockllm.Finish("stop"),
						mockllm.Done(),
					),
				},
			}, nil, nil)

			done, resp, promptErr := startPrompt(client, sid, "第一轮问题")
			// 等流式输出开始后取消
			gomega.Eventually(func() string { return client.Rec.Text() }, 5*time.Second).
				Should(gomega.ContainSubstring("第一轮部分输出"))
			gomega.Expect(client.Conn().Cancel(context.Background(), acpapi.CancelNotification{SessionId: sid})).NotTo(gomega.HaveOccurred())
			cancelTurn(done, resp, promptErr, client, sid)

			// 继续: 新 user 消息注入请求体, 无 orphan, 不出现取消后的文本
			gomega.Expect(prompt(client, sid, "继续").StopReason).To(gomega.Equal(acpapi.StopReasonEndTurn))
			reqs := mock.Requests()
			gomega.Expect(reqs).To(gomega.HaveLen(2))
			gomega.Expect(reqs[1].Messages).To(gomega.ContainElement(gomega.SatisfyAll(
				gomega.HaveField("Role", "user"),
				gomega.HaveField("Content", gomega.ContainSubstring("继续")),
			)))
			gomega.Expect(mockllm.HasNoOrphanToolCalls()(reqs[1])).To(gomega.Equal(""))
			gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())
		},
		ginkgo.Entry("OpenAI", mockllm.ProtocolOpenAI, ginkgo.SpecTimeout(15*time.Second)),
		ginkgo.Entry("Anthropic", mockllm.ProtocolAnthropic, ginkgo.SpecTimeout(15*time.Second)),
		ginkgo.Entry("OpenAI Responses", mockllm.ProtocolOpenAIResponses, ginkgo.SpecTimeout(15*time.Second)),
	)

	ginkgo.DescribeTable("场景 B: 工具执行中取消 → 继续, 无 orphan tool call",
		func(_ ginkgo.SpecContext, p mockllm.Protocol) {
			client, mock, sid, _ := startACPTurn(p, []mockllm.Step{
				// Bash 真实执行 sleep(2s) — 给 cancel 一个执行中窗口
				{Reply: mockllm.Stream(
					mockllm.ToolCallStart("call_1", "Bash", `{"command":"sleep 2"}`),
					mockllm.Usage(120, 30),
					mockllm.Finish("tool_calls"),
					mockllm.Done(),
				)},
				{
					// 继续后的请求: 无 orphan tool call(Bash 结果要么已回传、
					// 要么被 stripPendingToolCalls 清掉), 新 prompt 注入
					Require: mockllm.HasNoOrphanToolCalls(),
					Reply: mockllm.Stream(
						mockllm.Text("恢复的回答"),
						mockllm.Usage(20, 10),
						mockllm.Finish("stop"),
						mockllm.Done(),
					),
				},
			}, nil, nil)

			done, resp, promptErr := startPrompt(client, sid, "跑一下命令")
			// 等 agent 开始执行工具后取消
			gomega.Eventually(func() []acpapi.ToolCallId { return client.Rec.ToolCallIDs() }, 5*time.Second).
				Should(gomega.ContainElement(acpapi.ToolCallId("call_1")))
			gomega.Expect(client.Conn().Cancel(context.Background(), acpapi.CancelNotification{SessionId: sid})).NotTo(gomega.HaveOccurred())
			cancelTurn(done, resp, promptErr, client, sid)

			// 继续: 请求体无 orphan + 新 prompt 注入
			gomega.Expect(prompt(client, sid, "继续").StopReason).To(gomega.Equal(acpapi.StopReasonEndTurn))
			reqs := mock.Requests()
			gomega.Expect(reqs).To(gomega.HaveLen(2))
			gomega.Expect(mockllm.HasNoOrphanToolCalls()(reqs[1])).To(gomega.Equal(""))
			gomega.Expect(reqs[1].Messages).To(gomega.ContainElement(gomega.HaveField("Content", gomega.ContainSubstring("继续"))))
			gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())
		},
		ginkgo.Entry("OpenAI", mockllm.ProtocolOpenAI, ginkgo.SpecTimeout(20*time.Second)),
		ginkgo.Entry("Anthropic", mockllm.ProtocolAnthropic, ginkgo.SpecTimeout(20*time.Second)),
		ginkgo.Entry("OpenAI Responses", mockllm.ProtocolOpenAIResponses, ginkgo.SpecTimeout(20*time.Second)),
	)

	ginkgo.DescribeTable("场景 C: tool result 回传后、第二轮 LLM 前取消 → 继续, 完整轮次 + 新 user 注入形式",
		func(_ ginkgo.SpecContext, p mockllm.Protocol) {
			client, mock, sid, _ := startACPTurn(p, []mockllm.Step{
				{Reply: mockllm.Stream(
					mockllm.ToolCallStart("call_1", "Bash", `{"command":"ls"}`),
					mockllm.Usage(120, 30),
					mockllm.Finish("tool_calls"),
					mockllm.Done(),
				)},
				// 第二轮请求(agent 回传 tool result 后)挂起 — 在"发 tool
				// result 给 LLM 的过程中"取消
				{Reply: mockllm.Stream(
					mockllm.Pause(30*time.Second),
					mockllm.Text("不应出现"),
					mockllm.Finish("stop"),
					mockllm.Done(),
				)},
				{
					// 继续后的请求: 完整 tool 轮次(assistant tool_calls +
					// tool result)在 history, 无 orphan, 新 prompt 注入
					Require: mockllm.HasToolResult("call_1", gomega.ContainSubstring("README.md")),
					Reply: mockllm.Stream(
						mockllm.Text("继续的回答"),
						mockllm.Usage(20, 10),
						mockllm.Finish("stop"),
						mockllm.Done(),
					),
				},
			}, map[string]string{"README.md": "# Fixture\n"}, nil)

			done, resp, promptErr := startPrompt(client, sid, "看一下目录")
			// 等第二轮请求到达 mock(agent 已回传 tool result)后取消
			gomega.Eventually(func() int { return mock.RequestCount() }, 10*time.Second).
				Should(gomega.Equal(2))
			gomega.Expect(client.Conn().Cancel(context.Background(), acpapi.CancelNotification{SessionId: sid})).NotTo(gomega.HaveOccurred())
			cancelTurn(done, resp, promptErr, client, sid)

			// 继续: 完整 tool 轮次在 history + 新 prompt 注入(无 orphan)
			gomega.Expect(prompt(client, sid, "继续").StopReason).To(gomega.Equal(acpapi.StopReasonEndTurn))
			reqs := mock.Requests()
			gomega.Expect(reqs).To(gomega.HaveLen(3))
			gomega.Expect(mockllm.HasNoOrphanToolCalls()(reqs[2])).To(gomega.Equal(""))
			// 新 prompt 文本出现在请求体(注入形式按协议不同:
			// OpenAI/Responses 独立 user 消息, Anthropic 合并进 tool_result 消息)
			gomega.Expect(reqs[2].Messages).To(gomega.ContainElement(gomega.HaveField("Content", gomega.ContainSubstring("继续"))))
			gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())
		},
		ginkgo.Entry("OpenAI", mockllm.ProtocolOpenAI, ginkgo.SpecTimeout(20*time.Second)),
		ginkgo.Entry("Anthropic", mockllm.ProtocolAnthropic, ginkgo.SpecTimeout(20*time.Second)),
		ginkgo.Entry("OpenAI Responses", mockllm.ProtocolOpenAIResponses, ginkgo.SpecTimeout(20*time.Second)),
	)
})
