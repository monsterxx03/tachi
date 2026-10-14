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
	"strconv"
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
		// 0.0.0.0 是本进程里唯一能安全地「离开本机」的方式：活到这个 spec 结束，而它对外那一面在回答
		// 任何东西之前先要一张客户端证书。这也是被委派的**前提**：身份出自证书，而没有身份的对端，工具
		// 入参根本点不了名（node/peers.go）。端口先挑好，因为通配地址推不出可连地址（node/a2a.go）。
		peerPort := nodePort(ginkgo.GinkgoT())
		peerURL := "https://127.0.0.1:" + strconv.Itoa(peerPort)
		peerHome := harness.NewHome(ginkgo.GinkgoT(), peerMock,
			harness.WithNodeWorkspace(peerWorkspace),
			harness.WithNodeListen("0.0.0.0:"+strconv.Itoa(peerPort)),
			harness.WithNodePublicURL(peerURL),
			harness.WithNodeService(),
		)
		peerCfg := harness.Config(ginkgo.GinkgoT(), peerHome)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		ginkgo.DeferCleanup(cancel)
		peerHandle, err := node.StartOrAttachNode(ctx, peerCfg)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(peerHandle).NotTo(gomega.BeNil())
		gomega.Expect(peerHandle.Hosting()).To(gomega.BeTrue())
		ginkgo.DeferCleanup(func() { _ = peerHandle.Close() })

		// 同一张 Card 的另一面：绑通配地址时地址来自 public_url（0.0.0.0 不是任何东西能拨的地方，
		// 而说不清自己在哪儿的节点根本不启动），而它对外那一面是真加密、真要求证书的——因为开关是
		// 地址而不是配置项（node/a2a.go）。
		gomega.Expect(peerHandle.Card().SecuritySchemes).To(gomega.HaveLen(1))
		gomega.Expect(peerHandle.Card().SupportedInterfaces).To(gomega.HaveLen(1))
		gomega.Expect(peerHandle.Card().SupportedInterfaces[0].URL).To(gomega.Equal(peerURL))

		// ---- the caller: the real binary, with the peer in its peers: ----
		//
		// 模型点名的是**身份**，不是 `peers:` 里那个名字 —— 名字是给人看的，而身份是对端在握手时
		// 证明的、谁都冒充不了的那一样（设计 §一.6）。所以脚本要在对端起来之后才写得出。
		sutMock := mockllm.NewServer()
		ginkgo.DeferCleanup(sutMock.Close)
		sutMock.Script(
			// The model decides to delegate. The prompt is what the other machine reads.
			mockllm.Step{Reply: mockllm.Stream(
				mockllm.ToolCallStart("call_d1", tools.ToolNameDelegateTask,
					`{"node":"`+peerHandle.NodeID()+`","prompt":"检查昨天的备份是否完整，返回异常清单，不修改文件。"}`),
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
			harness.WithPeers("peer", peerURL),
		)
		workdir := harness.SeedWorkDir(ginkgo.GinkgoT(), nil)

		res := run.Binary(bin, workdir, "--home", sutHome, "-p",
			"检查一下备份，活儿在别的机器上", "-o", "json-stream")
		gomega.Expect(res.Err).To(gomega.BeNil())
		gomega.Expect(res.ExitCode).To(gomega.Equal(0), "stdout=%s stderr=%s", res.Stdout, res.Stderr)
		gomega.Expect(res.Stdout).To(gomega.ContainSubstring("远端说对了"))

		// The pipe entry runs with every reminder switched off by its own config
		// (DisableSystemReminders), so nothing here asserts a reminder: what it asserts is the
		// round trip. The reminders are exercised where they exist — an interactive conversation
		// (agent/agent_delegation_test.go) and the TUI's own node spec.
		gomega.Expect(sutMock.Requests()).To(gomega.HaveLen(2), "one call to decide, one to act on the result")

		// The peer really ran a turn of its own.
		gomega.Expect(peerMock.Requests()).To(gomega.HaveLen(1))
		gomega.Expect(peerMock.Error()).NotTo(gomega.HaveOccurred())
		gomega.Expect(sutMock.Error()).NotTo(gomega.HaveOccurred())

		// And this machine kept its own record of what it delegated: that is what a later turn
		// refers to, and what makes the work findable after the local conversation is compacted.
		records, err := os.ReadDir(filepath.Join(sutHome, "a2a", "delegations"))
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "the delegating side must file a record")
		gomega.Expect(records).To(gomega.HaveLen(1))
		gomega.Expect(records[0].Name()).To(gomega.HavePrefix("d_"))

		raw, err := os.ReadFile(filepath.Join(sutHome, "a2a", "delegations", records[0].Name()))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		body := string(raw)
		gomega.Expect(body).To(gomega.ContainSubstring(`"remote_task_id"`),
			"the record has to name the remote task, or a follow-up cannot reference it")
		gomega.Expect(body).To(gomega.ContainSubstring(`"peer": "peer"`),
			"记录里那个名字是标签，给人看的")
		gomega.Expect(body).To(gomega.ContainSubstring(`"peer_node_id": "`+peerHandle.NodeID()+`"`),
			"而真正绑住这台机器的是身份：改名换址都不改归属")
		gomega.Expect(body).To(gomega.ContainSubstring(`"state": "TASK_STATE_COMPLETED"`))
		gomega.Expect(body).To(gomega.ContainSubstring("不修改文件"),
			"the request itself is kept: a retry after a lost response has to send the same thing")
	})

	ginkgo.It("无对话的运行落在空作用域里，list 能看到该作用域的引用", func() {
		// Two runs of the SAME binary with no session at all. The empty scope is a SCOPE: the
		// second run's RemoteTask(action="list") shows the first run's task — and, being a scope
		// rather than a wildcard, it shows nothing from any real conversation.
		peerMock := mockllm.NewServer()
		ginkgo.DeferCleanup(peerMock.Close)
		peerMock.Script(
			mockllm.Step{Reply: mockllm.Stream(mockllm.Text("备份正常"), mockllm.Finish("stop"), mockllm.Done())},
		)

		peerPort := nodePort(ginkgo.GinkgoT())
		peerURL := "https://127.0.0.1:" + strconv.Itoa(peerPort)
		peerHome := harness.NewHome(ginkgo.GinkgoT(), peerMock,
			harness.WithNodeListen("0.0.0.0:"+strconv.Itoa(peerPort)),
			harness.WithNodePublicURL(peerURL),
			harness.WithNodeWorkspace(filepath.Join(ginkgo.GinkgoT().TempDir(), "peer-ws")))
		peerCfg := harness.Config(ginkgo.GinkgoT(), peerHome)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		ginkgo.DeferCleanup(cancel)
		peerHandle, err := node.StartOrAttachNode(ctx, peerCfg)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ginkgo.DeferCleanup(func() { _ = peerHandle.Close() })

		sutMock := mockllm.NewServer()
		ginkgo.DeferCleanup(sutMock.Close)
		sutMock.Script(
			// Run 1: delegate.
			mockllm.Step{Reply: mockllm.Stream(
				mockllm.ToolCallStart("call_a", tools.ToolNameDelegateTask,
					`{"node":"`+peerHandle.NodeID()+`","prompt":"检查备份"}`),
				mockllm.Finish("tool_calls"), mockllm.Done(),
			)},
			mockllm.Step{Reply: mockllm.Stream(mockllm.Text("交给 peer 了。"), mockllm.Finish("stop"), mockllm.Done())},
			// Run 2: a NEW pipe run asks what this scope has out, and its tool result must carry
			// the record run 1 created — the id is the only handle, and a sessionless run has no
			// reminder to be told about it (the pipe entry runs with reminders switched off).
			mockllm.Step{Reply: mockllm.Stream(
				mockllm.ToolCallStart("call_l", tools.ToolNameRemoteTask, `{"action":"list"}`),
				mockllm.Finish("tool_calls"), mockllm.Done(),
			)},
			mockllm.Step{
				Require: mockllm.HasToolResult("call_l", containsSubstring(`"title":"检查备份"`)),
				Reply: mockllm.Stream(
					mockllm.Text("看到了，之前让 peer 查过备份。"),
					mockllm.Finish("stop"), mockllm.Done(),
				),
			},
		)
		sutHome := harness.NewHome(ginkgo.GinkgoT(), sutMock,
			harness.WithPeers("peer", peerURL))
		workdir := harness.SeedWorkDir(ginkgo.GinkgoT(), nil)

		first := run.Binary(bin, workdir, "--home", sutHome, "-p", "让 peer 查一下备份", "-o", "json-stream")
		gomega.Expect(first.ExitCode).To(gomega.Equal(0), "stdout=%s stderr=%s", first.Stdout, first.Stderr)

		// The record names the empty scope, never somebody's conversation.
		records, err := os.ReadDir(filepath.Join(sutHome, "a2a", "delegations"))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(records).To(gomega.HaveLen(1))
		raw, err := os.ReadFile(filepath.Join(sutHome, "a2a", "delegations", records[0].Name()))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(string(raw)).To(gomega.ContainSubstring(`"logical_id": ""`),
			"a run with no session files under the empty scope, never under somebody's conversation")

		second := run.Binary(bin, workdir, "--home", sutHome, "-p", "我记得之前让它查过什么？", "-o", "json-stream")
		gomega.Expect(second.ExitCode).To(gomega.Equal(0), "stdout=%s stderr=%s", second.Stdout, second.Stderr)
		gomega.Expect(second.Stdout).To(gomega.ContainSubstring("之前让 peer 查过备份"))
		gomega.Expect(sutMock.Error()).NotTo(gomega.HaveOccurred())
	})

})

// requestText flattens one recorded request's messages: the reminder is part of a user message,
// and which one it lands in is the machinery's business, not this spec's.
func requestText(req *mockllm.RecordedRequest) string {
	var b strings.Builder
	for _, m := range req.Messages {
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}

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
