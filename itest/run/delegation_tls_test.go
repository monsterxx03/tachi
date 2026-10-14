//go:build integration

package run_test

// 跨机的委派：这一次「另一台机器」真的离开了 loopback，于是它必须走 TLS，而且必须问调用方要
// 客户端证书。
//
// 唯一的开关是地址，不是配置项（node/options.go isLoopbackListen）：离开本机才是加密和识别
// 调用方的理由，留在本机上它们没有意义。node/a2a.go 用这一个表达式同时决定三件事 —— 加不加密、
// 要不要证书、把证书里的身份带不带进请求 —— 所以「三件事里漏了一件」在形状上就不可能发生。
//
// 这条 spec 覆盖包内测试够不着的那一层：**真实的 config.yaml**（node.listen / node.name /
// peers: 都经 config.Load 走到二进制里）、**真二进制**（-p 入口才把发送方装配起来），以及身份在
// **两端各自的磁盘上**留下的痕迹 —— 这一侧 pin 住对端的身份，那一侧记下调用方是谁。
//
// node/tls_test.go 的 TestTwoRealNodesTalkOverTLS 已经把发送方和接收方这两半接起来过了，但它是
// in-process 地构造 Options；这里走的是配置和进程边界。

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/itest/harness"
	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/monsterxx03/tachi/itest/run"
	"github.com/monsterxx03/tachi/node"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/session"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

var _ = ginkgo.Describe("delegation to another machine", func() {
	ginkgo.It("走 mTLS：两端各自在自己的记录里留下对方的身份", func() {
		// ---- 对端：真节点，在这个进程里，离开 loopback，有自己的 home ----
		peerMock := mockllm.NewServer()
		ginkgo.DeferCleanup(peerMock.Close)
		peerMock.Script(mockllm.Step{
			// 对端真的读到了这段请求 —— 这是「活儿是在另一台机器上干的」的另一半
			// （另一半是答案回到了这一侧的模型手里，由下面 sutMock 的脚本要求）。
			Require: mockllm.HasUserMessage(containsSubstring("不修改文件")),
			Reply: mockllm.Stream(
				mockllm.Text("备份检查完成：两个目录异常"),
				mockllm.Finish("stop"),
				mockllm.Done(),
			),
		})

		peerWorkspace := filepath.Join(ginkgo.GinkgoT().TempDir(), "peer-ws")
		// 0.0.0.0 是本进程里唯一能安全地「离开本机」的方式：活到这个 spec 结束，而它对外那一面在回答
		// 任何东西之前先要一张客户端证书。端口是这里先挑好的，因为绑一个通配地址就推不出可连地址，而
		// 一个说不清自己在哪儿的节点拒绝启动（node/a2a.go）。
		peerPort := nodePort(ginkgo.GinkgoT())
		peerURL := "https://127.0.0.1:" + strconv.Itoa(peerPort)
		peerHome := harness.NewHome(ginkgo.GinkgoT(), peerMock,
			harness.WithNodeListen("0.0.0.0:"+strconv.Itoa(peerPort)),
			harness.WithNodePublicURL(peerURL),
			harness.WithNodeName("tachi@home-nas"),
			harness.WithNodeWorkspace(peerWorkspace),
			harness.WithNodeService(),
		)
		peerCfg := harness.Config(ginkgo.GinkgoT(), peerHome)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		ginkgo.DeferCleanup(cancel)
		peerHandle, err := node.StartOrAttachNode(ctx, peerCfg)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(peerHandle.Hosting()).To(gomega.BeTrue())
		ginkgo.DeferCleanup(func() { _ = peerHandle.Close() })

		// Card 是这台机器对自己「实际做了什么」的声明：它对外提供 TLS 并要求调用方出示证书，
		// 所以就声明 mTLS —— 而不是声明一个更宽松、它并不执行的方案。名字来自 node.name。
		card := peerHandle.Card()
		gomega.Expect(card.Name).To(gomega.Equal("tachi@home-nas"))
		gomega.Expect(card.SecuritySchemes).To(gomega.HaveLen(1))
		gomega.Expect(card.SecuritySchemes).To(gomega.HaveKey(a2a.SecuritySchemeName("mtls")))
		gomega.Expect(card.SecuritySchemes[a2a.SecuritySchemeName("mtls")]).
			To(gomega.BeAssignableToTypeOf(a2a.MutualTLSSecurityScheme{}))
		gomega.Expect(card.SecurityRequirements).To(gomega.HaveLen(1))
		gomega.Expect(card.SecurityRequirements[0]).To(gomega.HaveKey(a2a.SecuritySchemeName("mtls")))
		// 绑在通配地址上，所以 Card 里的地址来自配置：0.0.0.0 不是任何东西能拨的地方，而一个说不清
		// 自己在哪儿的节点根本不会启动 —— 它说的那句必须是运维者给的、真的能拨的那一个。
		gomega.Expect(card.SupportedInterfaces).To(gomega.HaveLen(1))
		gomega.Expect(card.SupportedInterfaces[0].URL).To(gomega.Equal(peerURL))

		// ---- 本机：真二进制，节点服务关着，只配了一条 https 的 peer ----
		sutMock := mockllm.NewServer()
		ginkgo.DeferCleanup(sutMock.Close)
		sutMock.Script(
			mockllm.Step{Reply: mockllm.Stream(
				mockllm.ToolCallStart("call_t1", tools.ToolNameDelegateTask,
					`{"node":"`+peerHandle.NodeID()+`","prompt":"检查昨天的备份是否完整，返回异常清单，不修改文件。"}`),
				mockllm.Finish("tool_calls"),
				mockllm.Done(),
			)},
			// 只有对端的答案出现在工具结果里时才会被服务到：这就是往返真的穿过了另一台机器的
			// 证明，而不是本机编出来的。
			mockllm.Step{
				Require: mockllm.HasToolResult("call_t1", containsSubstring("备份检查完成：两个目录异常")),
				Reply: mockllm.Stream(
					mockllm.Text("远端说对了：两个目录没有备份完。"),
					mockllm.Finish("stop"),
					mockllm.Done(),
				),
			},
		)

		sutHome := harness.NewHome(ginkgo.GinkgoT(), sutMock,
			harness.WithPeers("nas", peerURL),
			// 这半边它的节点服务是关的 —— 委派和承载是两回事。但名字仍然要有：node.name 就是
			// 它发给对端的那个自称，写在对端的工作记录里。
			harness.WithNodeName("tachi@mac"),
		)
		workdir := harness.SeedWorkDir(ginkgo.GinkgoT(), nil)

		res := run.Binary(bin, workdir, "--home", sutHome, "-p",
			"检查一下备份，活儿在别的机器上", "-o", "json-stream")
		gomega.Expect(res.Err).To(gomega.BeNil())
		gomega.Expect(res.ExitCode).To(gomega.Equal(0), "stdout=%s stderr=%s", res.Stdout, res.Stderr)
		gomega.Expect(res.Stdout).To(gomega.ContainSubstring("远端说对了"))

		gomega.Expect(sutMock.Requests()).To(gomega.HaveLen(2), "one call to decide, one to act on the result")
		gomega.Expect(peerMock.Requests()).To(gomega.HaveLen(1), "对端真的跑了一轮")
		gomega.Expect(sutMock.Error()).NotTo(gomega.HaveOccurred())
		gomega.Expect(peerMock.Error()).NotTo(gomega.HaveOccurred())

		// ---- 这一侧：pin 住的是对端的身份，记录绑定的也是身份 ----
		//
		// 只发不收的机器也有密钥和证书（node/sender.go）：说自己是谁是发起的代价，而一个匿名的
		// 调用方正是对端会拒绝的东西。它就在这个 home 下，和承载节点的机器用同一套材料。
		sutID := nodeIdentity(ginkgo.GinkgoT(), sutHome)
		peerID := peerHandle.NodeID()
		gomega.Expect(sutID.NodeID).NotTo(gomega.Equal(peerID), "调用方不是它调的那台机器")

		var pinned map[string]pinnedPeer
		gomega.Expect(fileutil.ReadJSON(filepath.Join(sutHome, "a2a", "peer-identities.json"), &pinned)).
			To(gomega.Succeed(), "首次接触必须留下 pin，否则下一次又要重新信任一个陌生人")
		gomega.Expect(pinned).To(gomega.HaveKey(peerID),
			"pin 的键是 TLS 握手证明的那个身份，不是对端自称的任何东西")
		gomega.Expect(pinned[peerID].Alias).To(gomega.Equal("nas"),
			"名字是记录里的一个字段：改名就是这一行变，键不动")
		gomega.Expect(pinned[peerID].URL).To(gomega.Equal(peerURL),
			"pin 的记录里还写着它是在哪儿 pin 的 —— 对端换地址时这正是要读的那一行")
		gomega.Expect(pinned[peerID].FirstSeen.IsZero()).To(gomega.BeFalse())

		// 委派记录绑定的同样是身份：别名哪一天改了，这些活儿还是同一台机器的（design §4.1）。
		record := string(readOnlyRecord(ginkgo.GinkgoT(), filepath.Join(sutHome, "a2a", "delegations"), "d_"))
		gomega.Expect(record).To(gomega.ContainSubstring(`"peer": "nas"`),
			"alias 也留一份：它是模型和用户用的那个词")
		gomega.Expect(record).To(gomega.ContainSubstring(`"peer_node_id": "` + peerID + `"`))
		gomega.Expect(record).To(gomega.ContainSubstring(`"remote_task_id"`))
		gomega.Expect(record).To(gomega.ContainSubstring(`"state": "TASK_STATE_COMPLETED"`))

		// ---- 那一侧：它的工作记录说得出是谁让它干的 ----
		task := string(readOnlyRecord(ginkgo.GinkgoT(), filepath.Join(peerHome, "a2a", "tasks"), ""))
		gomega.Expect(task).To(gomega.ContainSubstring(`"caller_id": "`+sutID.NodeID+`"`),
			"接收侧的身份取自证书里的公钥，调用方无法冒充成别人")
		gomega.Expect(task).To(gomega.ContainSubstring(`"self_reported_name": "tachi@mac"`),
			"而名字是自称的，所以它单独一个字段、按自称记")
		gomega.Expect(task).To(gomega.ContainSubstring(`"state": "TASK_STATE_COMPLETED"`))

		// ---- 而它自己那个会话，也说得出是谁派的 ----
		//
		// 「谁的活儿」这件事不只落在任务记录里：受管会话自己身上也写着（界面标一行时要读的
		// 就是它）。读的是前端走的同一条路 —— 会话目录里还住着保留记录这类管理目录，
		// 「哪些条目算会话」不该由这条 spec 自己再推一遍。
		//
		// 它记的是身份，不是名字：名字要去本机的对端目录里查，查不到就老实说不知道。那一步
		// 还没做（这一轮只让事实先落到盘上），所以这里断言的是那个算出来的 id。
		held := receivingSession(ginkgo.GinkgoT(), peerHome)
		gomega.Expect(held.Kind).To(gomega.Equal(session.KindA2AExecution),
			"这条会话归子系统，不归坐在键盘前的人")
		gomega.Expect(held.SourceNodeID).To(gomega.Equal(sutID.NodeID),
			"来源是证书算出来的那个身份，和任务记录里的 caller_id 是同一个")
	})
})

// receivingSession returns the ONE session a --home's store holds, read through the same manager
// a frontend uses rather than by scanning the directory: the session dir also holds the retention
// bookkeeping (a management dir beside the sessions), and "which entries are sessions" is exactly
// the kind of thing a spec should not re-derive.
//
// The base dir is a process global and this spec has it pointed at the PEER's home (harness.Config
// above) — which is where the node resolved its own session store from when it wrote the record.
func receivingSession(t ginkgo.GinkgoTInterface, home string) *session.Session {
	t.Helper()
	previous := config.BaseDir()
	config.SetBaseDir(home)
	defer config.SetBaseDir(previous)

	sm, err := session.NewManager(nil)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	sessions, err := sm.List()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(sessions).To(gomega.HaveLen(1), "the run created exactly one conversation")
	return sessions[0]
}

// pinnedPeer is one entry of peer-identities.json (node/knownpeers.go's knownPeer), read here
// rather than through an accessor because what this spec asserts is the FILE a restart reads.
// pinnedPeer mirrors peer-identities.json as the binary writes it: keyed by the identity the
// handshake proved, with the name this machine used at the time beside it.
type pinnedPeer struct {
	Alias     string    `json:"alias"`
	URL       string    `json:"url"`
	FirstSeen time.Time `json:"first_seen"`
}

// nodeIdentity reads the key a --home's node dir holds, creating nothing: the identity under
// test is the one the binary wrote. The dir name is config's own (`node`, beside sessions and
// delegations) and is spelled out because this spec deliberately reads another --home's disk
// rather than the process-global base dir config would resolve.
func nodeIdentity(t ginkgo.GinkgoTInterface, home string) *node.Identity {
	t.Helper()
	id, err := node.LoadIdentity(filepath.Join(home, "a2a"))
	gomega.Expect(err).NotTo(gomega.HaveOccurred(), "the entry point must have a key of its own")
	return id
}

// readOnlyRecord returns the contents of the ONE record file in dir, skipping the derived
// index — and insists there is exactly one, because "which files this run left behind" is
// part of what the spec pins.
func readOnlyRecord(t ginkgo.GinkgoTInterface, dir, prefix string) []byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	gomega.Expect(err).NotTo(gomega.HaveOccurred(), "%s must exist after the run", dir)
	var found []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" || e.Name() == "index.json" {
			continue // index.json is DERIVED: it names records, it is not one
		}
		if prefix != "" && !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		found = append(found, filepath.Join(dir, e.Name()))
	}
	gomega.Expect(found).To(gomega.HaveLen(1), "records in %s", dir)
	raw, err := os.ReadFile(found[0])
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return raw
}
