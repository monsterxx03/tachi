//go:build integration

package acp_test

// L3 — edge cases over the real ACP wire: the provider request timeout
// (spec.timeout) aborts a hanging upstream fast instead of wedging the
// editor, and SetSessionMode round-trips the mode-change notification.

import (
	"context"
	"time"

	acpapi "github.com/coder/acp-go-sdk"
	"github.com/monsterxx03/tachi/itest/harness"
	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

var _ = ginkgo.Describe("ACP edge cases (triple protocol)", func() {
	ginkgo.DescribeTable("spec.timeout 生效: 挂起的上游快速失败而非卡死编辑器",
		func(_ ginkgo.SpecContext, p mockllm.Protocol) {
			// Hold 保持响应打开直到客户端中止 —— 模拟"永远慢"的上游。
			client, mock, sid, _ := startACPTurn(p, []mockllm.Step{
				{Reply: mockllm.Hold()},
			}, nil, []harness.Option{harness.WithSpecTimeout(200 * time.Millisecond)})

			start := time.Now()
			resp := prompt(client, sid, "hi")
			elapsed := time.Since(start)

			// 快速失败（无 timeout 时 Hold 永不返回, prompt 挂死）
			gomega.Expect(elapsed).To(gomega.BeNumerically("<", 5*time.Second))
			// 超时错误以文本更新形式到达编辑器（"Error: ..."）
			gomega.Expect(client.Rec.Text()).To(gomega.ContainSubstring("Error:"))
			// SDK 超时直接返回不重试 → 恰好 1 个请求
			gomega.Expect(mock.Requests()).To(gomega.HaveLen(1))
			// Hold 被客户端中止, mock 无错误
			gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())
			gomega.Expect(resp.StopReason).To(gomega.Equal(acpapi.StopReasonEndTurn))
		},
		ginkgo.Entry("OpenAI", mockllm.ProtocolOpenAI, ginkgo.SpecTimeout(15*time.Second)),
		ginkgo.Entry("Anthropic", mockllm.ProtocolAnthropic, ginkgo.SpecTimeout(15*time.Second)),
		ginkgo.Entry("OpenAI Responses", mockllm.ProtocolOpenAIResponses, ginkgo.SpecTimeout(15*time.Second)),
	)

	ginkgo.DescribeTable("set_session_mode: mode 切换通知回传",
		func(p mockllm.Protocol) {
			client, mock, sid, _ := startACPTurn(p, []mockllm.Step{{
				Reply: mockllm.Stream(
					mockllm.Text("ok"),
					mockllm.Usage(10, 5),
					mockllm.Finish("stop"),
					mockllm.Done(),
				),
			}}, nil, nil)

			_, err := client.Conn().SetSessionMode(context.Background(), acpapi.SetSessionModeRequest{
				SessionId: sid,
				ModeId:    acpapi.SessionModeId("plan"),
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// CurrentModeUpdate 通知到达 client
			var gotMode acpapi.SessionModeId
			gomega.Eventually(func() string {
				for _, n := range client.Rec.ForSession(sid) {
					if u := n.Update.CurrentModeUpdate; u != nil {
						gotMode = u.CurrentModeId
						return string(gotMode)
					}
				}
				return ""
			}, 5*time.Second).Should(gomega.Equal("plan"))

			// 模式切换后 turn 仍正常
			gomega.Expect(prompt(client, sid, "hi").StopReason).To(gomega.Equal(acpapi.StopReasonEndTurn))
			gomega.Expect(mock.Error()).NotTo(gomega.HaveOccurred())
		},
		ginkgo.Entry("OpenAI", mockllm.ProtocolOpenAI),
		ginkgo.Entry("Anthropic", mockllm.ProtocolAnthropic),
		ginkgo.Entry("OpenAI Responses", mockllm.ProtocolOpenAIResponses),
	)
})
