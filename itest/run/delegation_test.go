//go:build integration

package run_test

// 发起侧：本机把一件活儿委派给另一个节点，对端真的跑了一轮，结果回到本机模型手里。
//
// 这条 spec 覆盖包内测试够不着的那一层：**真实的 config.yaml**（顶格 `peers:` 经 config.Load
// 走到发送方目录）、**真二进制**（`-p` 入口把工具按 run 装上），以及对端是**另一个真节点**
// （它有自己的数据目录、自己的工作区、自己的模型脚本）。
//
// 对端在这个 spec 进程里承载：`--home` 让二进制有自己的 base dir，所以本进程的全局 base dir
// 可以指向对端。这正是 `node/runner.go` 那条"每个 context 的 session manager 必须按构造时的
// 目录建"修掉之后才成立的形状 —— 修之前，同进程两个 home 会让节点自己的第一轮就失败。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/itest/harness"
	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/monsterxx03/tachi/itest/run"
	"github.com/monsterxx03/tachi/node"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

var _ = ginkgo.Describe("delegation", func() {
	ginkgo.It("把活儿委派给另一个节点，对端跑完一轮，结果回到本地模型", func() {
		// ---- the peer: a real node in THIS process, on its own home ----
		peerMock := mockllm.NewServer()
		ginkgo.DeferCleanup(peerMock.Close)
		peerMock.Script(mockllm.Step{Reply: mockllm.Stream(
			mockllm.Text("备份检查完成：两个目录异常"),
			mockllm.Finish("stop"),
			mockllm.Done(),
		)})

		peerWorkspace := filepath.Join(ginkgo.GinkgoT().TempDir(), "peer-ws")
		peerHome := harness.NewHome(ginkgo.GinkgoT(), peerMock,
			harness.WithNodeWorkspace(peerWorkspace),
			harness.WithNodeTools("ReadFile"),
		)
		peerCfg := harness.Config(ginkgo.GinkgoT(), peerHome)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		ginkgo.DeferCleanup(cancel)
		peerHandle, err := node.StartOrAttachNode(ctx, peerCfg)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(peerHandle).NotTo(gomega.BeNil())
		gomega.Expect(peerHandle.Hosting()).To(gomega.BeTrue())
		ginkgo.DeferCleanup(func() { _ = peerHandle.Close() })

		// ---- the caller: the real binary, with the peer in its peers: ----
		sutMock := mockllm.NewServer()
		ginkgo.DeferCleanup(sutMock.Close)
		sutMock.Script(
			// The model decides to delegate. The prompt is what the other machine reads.
			mockllm.Step{Reply: mockllm.Stream(
				mockllm.ToolCallStart("call_d1", tools.ToolNameDelegateTask,
					`{"node":"peer","prompt":"检查昨天的备份是否完整，返回异常清单，不修改文件。"}`),
				mockllm.Finish("tool_calls"),
				mockllm.Done(),
			)},
			// Served only if the tool result carried the PEER's answer: that is what proves the
			// round trip went through the other node rather than being made up locally.
			mockllm.Step{
				Require: mockllm.HasToolResult("call_d1", containsSubstring("备份检查完成：两个目录异常")),
				Reply: mockllm.Stream(
					mockllm.Text("远端说对了：两个目录没有备份完。"),
					mockllm.Finish("stop"),
					mockllm.Done(),
				),
			},
		)

		sutHome := harness.NewHome(ginkgo.GinkgoT(), sutMock,
			harness.WithPeers("peer", "http://"+peerHandle.RemoteAddr()),
		)
		workdir := harness.SeedWorkDir(ginkgo.GinkgoT(), nil)

		res := run.Binary(bin, workdir, "--home", sutHome, "-p",
			"检查一下备份，活儿在别的机器上", "-o", "json-stream")
		gomega.Expect(res.Err).To(gomega.BeNil())
		gomega.Expect(res.ExitCode).To(gomega.Equal(0), "stdout=%s stderr=%s", res.Stdout, res.Stderr)
		gomega.Expect(res.Stdout).To(gomega.ContainSubstring("远端说对了"))

		// The peer really ran a turn of its own.
		gomega.Expect(peerMock.Requests()).To(gomega.HaveLen(1))
		gomega.Expect(peerMock.Error()).NotTo(gomega.HaveOccurred())
		gomega.Expect(sutMock.Error()).NotTo(gomega.HaveOccurred())

		// And this machine kept its own record of what it delegated: that is what a later turn
		// refers to, and what makes the work findable after the local conversation is compacted.
		records, err := os.ReadDir(filepath.Join(sutHome, "delegations"))
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "the delegating side must file a record")
		gomega.Expect(records).To(gomega.HaveLen(1))
		gomega.Expect(records[0].Name()).To(gomega.HavePrefix("d_"))

		raw, err := os.ReadFile(filepath.Join(sutHome, "delegations", records[0].Name()))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		body := string(raw)
		gomega.Expect(body).To(gomega.ContainSubstring(`"remote_task_id"`),
			"the record has to name the remote task, or a follow-up cannot reference it")
		gomega.Expect(body).To(gomega.ContainSubstring(`"peer": "peer"`))
		gomega.Expect(body).To(gomega.ContainSubstring(`"state": "TASK_STATE_COMPLETED"`))
		gomega.Expect(body).To(gomega.ContainSubstring("不修改文件"),
			"the request itself is kept: a retry after a lost response has to send the same thing")
	})
})

// containsSubstring is a mockllm.Matcher: the mock's core keeps gomega out, and this spec needs
// one substring check.
type containsSubstring string

func (c containsSubstring) Match(actual any) (bool, error) {
	s, ok := actual.(string)
	if !ok {
		return false, nil
	}
	return strings.Contains(s, string(c)), nil
}
