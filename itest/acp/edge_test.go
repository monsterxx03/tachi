//go:build integration

package acp_test

// L3 — edge cases over the real ACP wire: the provider request timeout
// (spec.timeout) aborts a hanging upstream fast instead of wedging the editor.

import (
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

})
