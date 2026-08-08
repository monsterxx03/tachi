//go:build integration

package acp_test

// L2 — session state over the real ACP wire: model switch via
// SetSessionConfigOption, resume from disk after close, permission denial,
// list-by-cwd, and session mode switch. These exercise the editor-facing
// session-management surface on top of the working conversation core.

import (
	"context"
	"time"

	acpapi "github.com/coder/acp-go-sdk"
	"github.com/monsterxx03/tachi/itest/acp"
	"github.com/monsterxx03/tachi/itest/harness"
	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

// setConfigOption sends a ValueId config-option change (model /
// reasoning_effort), asserts no transport error, and waits for the
// ConfigOptionUpdate notification that confirms the agent adopted the change.
func setConfigOption(client *acp.Client, sid acpapi.SessionId, configID, value string) {
	_, err := client.Conn().SetSessionConfigOption(context.Background(), acpapi.SetSessionConfigOptionRequest{
		ValueId: &acpapi.SetSessionConfigOptionValueId{
			SessionId: sid,
			ConfigId:  acpapi.SessionConfigId(configID),
			Value:     acpapi.SessionConfigValueId(value),
		},
	})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	// The option update is acknowledged via a session/update notification —
	// wait for it so a silent no-op on the agent side fails the test.
	gomega.Eventually(func() bool {
		for _, n := range client.Rec.ForSession(sid) {
			if n.Update.ConfigOptionUpdate != nil {
				return true
			}
		}
		return false
	}, 5*time.Second).Should(gomega.BeTrue())
}

var _ = ginkgo.Describe("ACP session state", func() {
	ginkgo.DescribeTable("/model 切换: SetSessionConfigOption 后下一请求用新 provider",
		func(p mockllm.Protocol) {
			client, mock, sid, _ := startACPTurn(p, []mockllm.Step{
				{Require: mockllm.HasModel(gomega.Equal("mock-model")), Reply: mockllm.Stream(
					mockllm.Text("默认模型"),
					mockllm.Usage(10, 5),
					mockllm.Finish("stop"),
					mockllm.Done(),
				)},
				{Require: mockllm.HasModel(gomega.Equal("mock2-model")), Reply: mockllm.Stream(
					mockllm.Text("切换后的模型"),
					mockllm.Usage(10, 5),
					mockllm.Finish("stop"),
					mockllm.Done(),
				)},
			}, nil, []harness.Option{harness.WithSecondProvider("mock2-model")})

			gomega.Expect(prompt(client, sid, "第一轮").StopReason).To(gomega.Equal(acpapi.StopReasonEndTurn))

			// 切换到第二个 provider（value 是 config provider 名）
			setConfigOption(client, sid, "model", "mock2")

			gomega.Expect(prompt(client, sid, "第二轮").StopReason).To(gomega.Equal(acpapi.StopReasonEndTurn))
			gomega.Expect(mock.Requests()).To(gomega.HaveLen(2))
			gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())
		},
		ginkgo.Entry("OpenAI", mockllm.ProtocolOpenAI),
		ginkgo.Entry("Anthropic", mockllm.ProtocolAnthropic),
		ginkgo.Entry("OpenAI Responses", mockllm.ProtocolOpenAIResponses),
	)

	ginkgo.DescribeTable("thinking effort 切换: SetSessionConfigOption 后请求体带 effort",
		func(p mockllm.Protocol) {
			client, mock, sid, _ := startACPTurn(p, []mockllm.Step{
				{Reply: mockllm.Stream(
					mockllm.Text("默认"),
					mockllm.Usage(10, 5),
					mockllm.Finish("stop"),
					mockllm.Done(),
				)},
				// 各协议渲染 effort 的字段不同(reasoning_effort /
				// output_config.effort / reasoning.effort) — HasEffort 按协议分派
				{Require: mockllm.HasEffort("low"), Reply: mockllm.Stream(
					mockllm.Text("带 effort"),
					mockllm.Usage(10, 5),
					mockllm.Finish("stop"),
					mockllm.Done(),
				)},
			}, nil, nil)

			gomega.Expect(prompt(client, sid, "第一轮").StopReason).To(gomega.Equal(acpapi.StopReasonEndTurn))

			setConfigOption(client, sid, "reasoning_effort", "low")

			gomega.Expect(prompt(client, sid, "第二轮").StopReason).To(gomega.Equal(acpapi.StopReasonEndTurn))
			gomega.Expect(mock.Requests()).To(gomega.HaveLen(2))
			// 脚本被完整消费 → 第二轮的 effort 前置断言真的生效了
			gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())
		},
		ginkgo.Entry("OpenAI", mockllm.ProtocolOpenAI),
		ginkgo.Entry("Anthropic", mockllm.ProtocolAnthropic),
		ginkgo.Entry("OpenAI Responses", mockllm.ProtocolOpenAIResponses),
	)

	ginkgo.DescribeTable("resume_session: close 后从磁盘恢复, 第二轮带第一轮历史",
		func(p mockllm.Protocol) {
			client, mock, sid, workdir := startACPTurn(p, []mockllm.Step{
				{Reply: mockllm.Stream(
					mockllm.Text("第一轮回答"),
					mockllm.Usage(10, 5),
					mockllm.Finish("stop"),
					mockllm.Done(),
				)},
				{
					// 恢复后的请求必须携带第一轮的 user 消息（历史从磁盘回放）
					Require: mockllm.HasUserMessage(gomega.ContainSubstring("第一轮的问题")),
					Reply: mockllm.Stream(
						mockllm.Text("第二轮回答"),
						mockllm.Usage(10, 5),
						mockllm.Finish("stop"),
						mockllm.Done(),
					),
				},
			}, nil, nil)

			gomega.Expect(prompt(client, sid, "第一轮的问题").StopReason).To(gomega.Equal(acpapi.StopReasonEndTurn))

			// 关闭释放内存 session（历史已落盘）
			_, err := client.Conn().CloseSession(context.Background(), acpapi.CloseSessionRequest{SessionId: sid})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// resume 从磁盘加载同一 session
			_, err = client.Conn().ResumeSession(context.Background(), acpapi.ResumeSessionRequest{
				SessionId:  sid,
				Cwd:        workdir,
				McpServers: []acpapi.McpServer{},
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Expect(prompt(client, sid, "继续").StopReason).To(gomega.Equal(acpapi.StopReasonEndTurn))
			gomega.Expect(mock.Requests()).To(gomega.HaveLen(2))
			gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())
		},
		ginkgo.Entry("OpenAI", mockllm.ProtocolOpenAI),
		ginkgo.Entry("Anthropic", mockllm.ProtocolAnthropic),
		ginkgo.Entry("OpenAI Responses", mockllm.ProtocolOpenAIResponses),
	)

	ginkgo.DescribeTable("权限拒绝: client reject → 工具不执行, 错误回传 LLM",
		func(p mockllm.Protocol) {
			client, mock, sid, _ := startACPTurn(p, []mockllm.Step{
				{Reply: mockllm.Stream(
					mockllm.ToolCallStart("call_1", "Bash", `{"command":"ls"}`),
					mockllm.Usage(120, 30),
					mockllm.Finish("tool_calls"),
					mockllm.Done(),
				)},
				{
					// 被拒后的工具结果必须是错误, 回传给 LLM
					Require: mockllm.HasToolError("call_1"),
					Reply: mockllm.Stream(
						mockllm.Text("工具被拒绝了"),
						mockllm.Usage(200, 20),
						mockllm.Finish("stop"),
						mockllm.Done(),
					),
				},
			}, map[string]string{"README.md": "# Fixture\n"},
				[]harness.Option{harness.WithBashAsk()},
				acp.WithPermission(acp.PermissionReject))

			resp := prompt(client, sid, "看一下目录")

			gomega.Expect(resp.StopReason).To(gomega.Equal(acpapi.StopReasonEndTurn))
			// 权限请求确实到达 client 且被拒绝
			gomega.Expect(client.PermissionRequests()).To(gomega.HaveLen(1))
			// 拒绝后 Bash 未执行 → 第二轮收到错误结果
			gomega.Expect(client.Rec.Text()).To(gomega.ContainSubstring("工具被拒绝了"))
			gomega.Expect(mock.Requests()).To(gomega.HaveLen(2))
			gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())
		},
		ginkgo.Entry("OpenAI", mockllm.ProtocolOpenAI),
		ginkgo.Entry("Anthropic", mockllm.ProtocolAnthropic),
		ginkgo.Entry("OpenAI Responses", mockllm.ProtocolOpenAIResponses),
	)

	ginkgo.DescribeTable("list_sessions: 按 cwd 精确过滤",
		func(p mockllm.Protocol) {
			client, mock, sid, workdir := startACPTurn(p, []mockllm.Step{{
				Reply: mockllm.Stream(
					mockllm.Text("ok"),
					mockllm.Usage(10, 5),
					mockllm.Finish("stop"),
					mockllm.Done(),
				),
			}}, nil, nil)

			// 在另一个 cwd 再开一个 session
			otherDir := harness.SeedWorkDir(ginkgo.GinkgoT(), nil)
			otherSid, err := newSession(client, otherDir)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(otherSid).NotTo(gomega.Equal(sid))

			// 按第一个 session 的 cwd 精确过滤(ListSessions 用 == 匹配)
			resp, err := client.Conn().ListSessions(context.Background(), acpapi.ListSessionsRequest{
				Cwd: &workdir,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(resp.Sessions).To(gomega.HaveLen(1))
			gomega.Expect(resp.Sessions[0].SessionId).To(gomega.Equal(sid))

			// 不传过滤 → 两个都列出
			all, err := client.Conn().ListSessions(context.Background(), acpapi.ListSessionsRequest{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(all.Sessions).To(gomega.HaveLen(2))
			gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())
		},
		ginkgo.Entry("OpenAI", mockllm.ProtocolOpenAI),
		ginkgo.Entry("Anthropic", mockllm.ProtocolAnthropic),
		ginkgo.Entry("OpenAI Responses", mockllm.ProtocolOpenAIResponses),
	)
})
