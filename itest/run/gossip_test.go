//go:build integration

package run_test

// 这个来源的组网，整个跑一遍：**三台都开着节点服务、都开着 gossip**，而配置里只有两条 —— A 配了 B，
// B 配了 C，C 什么都没配。最后要证明的是**互相都认识**：
//
//   - A 的表里有 B 和 C —— A 认识 C 这一条只可能是转述来的（A 的配置里没有 C 一个字）；
//   - B 的表里有 A 和 C —— B 认识 C 是配置给的，B 认识 A 只可能是「A 来问我时自报的地址」给的
//     （B 的配置里没有 A）；
//   - C 的表里有 A 和 B —— 它什么都没配，所以两条都只能靠「别人来问它」和「它问别人时听到的转述」。
//
// 三台各是什么：B 与 C 是两个真进程（`tachi channel` 无头承载节点，各自一份身份、一份 pin 记录、
// 一个数据目录 —— itest.md 的「没有终端的节点」）。A 在测试进程里，但**它是一台完整的机器**：同一
// 份 config 既起了它的节点服务（所以 B、C 拨得到它，它才能被别人认识），也起了它的发送侧（所以它
// 会去问 B）。这是一条真实入口的形状，只是它的进程是测试自己的 —— 能在 itest 里被驱动的常驻入口
// 只有两个：channel（无头承载，但接收不了指令）和 ACP（能接收指令，但不挂发送侧），而这条链的落点
// 是「A 点名委派」，需要一个能发起委派的发送侧。
//
// 「谁认识谁」分两种读法：A 在测试进程里，直接读它的目录；B、C 在别的进程里，读它们磁盘上的 TOFU
// 记录 —— 那份文件里每一行都是「这个进程自己拨通过一次」的证据（pin 是出站握手时做的证书检查写下
// 的），所以它是进程之外唯一读得到的答案。
//
// 它不依赖组播，所以在一台跑不了 mDNS 的机器上照样真跑（那条会被环境闸门 skip）。

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/itest/harness"
	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/monsterxx03/tachi/itest/run"
	"github.com/monsterxx03/tachi/node"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

// gossipWait bounds each step of the chain. It is generous because every step is a real handshake
// between real processes on a loaded runner, and short enough that a chain which does not form fails
// the spec rather than the suite.
const gossipWait = 30 * time.Second

var _ = ginkgo.Describe("gossip 转述", func() {
	ginkgo.It("三台都开着节点服务：A 只配了 B，最后三张表都是满的", func() {
		// ---- 端口先定死 ----
		//
		// 承载进程的远端地址没有任何东西会公布（local.json 只有本机控制面），所以三边都得由 fixture
		// 自己写出来（itest.md 的 nodePort 陷阱在里面）。
		ports := hostPorts(ginkgo.GinkgoT(), 3)
		aPort, bPort, cPort := ports[0], ports[1], ports[2]
		aURL := "https://127.0.0.1:" + strconv.Itoa(aPort)
		bURL := "https://127.0.0.1:" + strconv.Itoa(bPort)
		cURL := "https://127.0.0.1:" + strconv.Itoa(cPort)

		// ---- C：真进程，无头承载，**什么都不配**，gossip 开着 ----
		//
		// 它是被介绍的那一台：它不认识任何人，也没有任何人在它的配置里。
		cMock := mockllm.NewServer()
		ginkgo.DeferCleanup(cMock.Close)
		cMock.Script(mockllm.Step{
			Require: mockllm.HasUserMessage(containsSubstring("第三台机器")),
			Reply: mockllm.Stream(
				mockllm.Text("这活儿是在第三台机器上干的"),
				mockllm.Finish("stop"),
				mockllm.Done(),
			),
		})
		cHome := harness.NewHome(ginkgo.GinkgoT(), cMock,
			harness.WithNodeListen("0.0.0.0:"+strconv.Itoa(cPort)),
			harness.WithNodePublicURL(cURL),
			harness.WithNodeName("tachi@gossip-c"),
			harness.WithNodeWorkspace(filepath.Join(ginkgo.GinkgoT().TempDir(), "c-ws")),
			harness.WithNodeService(),
			harness.WithGossipDiscovery(),
			harness.WithChannel("mock"),
		)
		c := spawnHost(cHome)
		waitForNodeHost(ginkgo.GinkgoT(), c, cHome, cPort)
		cID := nodeIdentity(ginkgo.GinkgoT(), cHome)

		// ---- B：真进程，无头承载，配了 C，gossip 开着 ----
		//
		// 它是**转述者**：能说出来的只有自己亲手够到过的机器，而 C 是它配置里的（读过一次卡片，
		// 那就是一次接触）。
		bMock := mockllm.NewServer()
		ginkgo.DeferCleanup(bMock.Close)
		bHome := harness.NewHome(ginkgo.GinkgoT(), bMock,
			harness.WithNodeListen("0.0.0.0:"+strconv.Itoa(bPort)),
			harness.WithNodePublicURL(bURL),
			harness.WithNodeName("tachi@gossip-b"),
			harness.WithNodeWorkspace(filepath.Join(ginkgo.GinkgoT().TempDir(), "b-ws")),
			harness.WithNodeService(),
			harness.WithGossipDiscovery(),
			harness.WithPeers("gossip-c", cURL),
			harness.WithChannel("mock"),
		)
		b := spawnHost(bHome)
		waitForNodeHost(ginkgo.GinkgoT(), b, bHome, bPort)
		bID := nodeIdentity(ginkgo.GinkgoT(), bHome)
		gomega.Expect(bID.NodeID).NotTo(gomega.Equal(cID.NodeID), "两个进程，两份身份")

		// B 先把 C 记下来，这条链才有下一环 —— 而且这一步必须**先**完成：A 的循环只在启动时问一轮
		// （之后是 3 分钟的节拍），如果那一轮问到的是一个还不知道 C 的 B，A 就要等上三分钟。
		gomega.Eventually(func() string { return nodeLog(ginkgo.GinkgoT(), bHome) },
			gossipWait, 250*time.Millisecond).Should(gomega.ContainSubstring("node_id="+cID.NodeID),
			"B 必须先把 C 记进自己的表，否则它没有任何东西可以转述")

		// ---- A：一台完整的机器，只配了 B，进程在测试里 ----
		//
		// 两半都要起：节点服务（否则 B 拨不通它，它进不了 B 的表）与发送侧（否则它不会去问 B）。
		aMock := mockllm.NewServer()
		ginkgo.DeferCleanup(aMock.Close)
		aHome := harness.NewHome(ginkgo.GinkgoT(), aMock,
			harness.WithNodeListen("0.0.0.0:"+strconv.Itoa(aPort)),
			harness.WithNodePublicURL(aURL),
			harness.WithNodeName("tachi@gossip-a"),
			harness.WithNodeWorkspace(filepath.Join(ginkgo.GinkgoT().TempDir(), "a-ws")),
			harness.WithNodeService(),
			harness.WithGossipDiscovery(),
			harness.WithPeers("gossip-b", bURL),
		)
		cfg := harness.Config(ginkgo.GinkgoT(), aHome)
		gomega.Expect(cfg.Node.Discovery.Gossip.Enabled).To(gomega.BeTrue(),
			"fixture 必须把 gossip 打开，否则这条 spec 什么都没测")
		gomega.Expect(cfg.Node.Discovery.Mdns.Enabled).To(gomega.BeFalse(),
			"组播必须是关的：C 要是能被组播直接看见，这条 spec 就不再是「经由转述」了")
		gomega.Expect(cfg.Peers).To(gomega.HaveLen(1), "A 只配了 B")

		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		ginkgo.DeferCleanup(cancel)
		handle, err := node.StartOrAttachNode(ctx, cfg)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(handle.Hosting()).To(gomega.BeTrue(), "A 是一台承载着的机器，不是只当调用方那种")
		ginkgo.DeferCleanup(func() { _ = handle.Close() })
		aID := nodeIdentity(ginkgo.GinkgoT(), aHome).NodeID

		cache := node.NewDelegatorCache(cfg.Node.Name, node.DiscoveryOf(cfg), logger.New("itest-gossip"), nil, "")
		ginkgo.DeferCleanup(cache.Close)
		sender := cache.ForSender(ctx, cfg.Peers)
		gomega.Expect(sender).NotTo(gomega.BeNil(), "gossip 开着就是发送侧")

		// ---- 收敛 ----
		//
		// 第一跳（A 认识 B 与 C）走的是配置 + 一次转述；第二跳（B 认识 A、C 认识 A）要先由「对方来问
		// 我」把地址递过来，而 C 认识 A 还要再晚一轮 —— 它得先认识 B，才有邻居可问。
		//
		// 等最慢的那一格：C 的 TOFU 记录里出现 A。它成立就意味着整条链走完了。
		gomega.Eventually(func() string { return peerIdentities(cHome) },
			60*time.Second, 250*time.Millisecond).Should(gomega.ContainSubstring(aID),
			"C 什么都没配：它认识 A 只能靠「B 来问过它」+「它问了 B」这两跳")

		// A 对 B 和 C 是它自己的目录说的（它认得它们的身份）。
		var aKnows []string
		for _, row := range sender.DirectoryRows() {
			aKnows = append(aKnows, row.NodeID)
		}
		gomega.Expect(aKnows).To(gomega.ConsistOf(bID.NodeID, cID.NodeID),
			"A 的表里有 B（配置）和 C（转述），而且只有这两个")

		// B、C 在别的进程里，证据是它们各自拨通过的对方（pin 由出站握手写下）。
		gomega.Expect(peerIdentities(bHome)).To(gomega.And(
			gomega.ContainSubstring(aID),
			gomega.ContainSubstring(cID.NodeID)),
			"B 认识 A（A 来问它时自报的地址）和 C（它的配置）")
		gomega.Expect(peerIdentities(cHome)).To(gomega.And(
			gomega.ContainSubstring(aID),
			gomega.ContainSubstring(bID.NodeID)),
			"C 认识 B（B 来问它时自报的地址）和 A（它问 B 时听到的转述）")

		// ---- 点名委派：模型手里只有身份，而那是转述 + 一次握手给的 ----
		view, err := sender.Delegate(ctx, tools.DelegationRequest{
			Peer:      cID.NodeID,
			Prompt:    "报告一下第三台机器上有什么",
			LogicalID: "conv-1",
			Wait:      30 * time.Second,
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(view.State).To(gomega.Equal("completed"), "note=%q", view.Note)
		gomega.Expect(view.Result).To(gomega.ContainSubstring("这活儿是在第三台机器上干的"),
			"答案是 C 那个进程跑出来的")

		// 活儿确实是在 C 的进程里跑的：转述者 B 一次模型调用都没有发生。
		gomega.Expect(cMock.Requests()).To(gomega.HaveLen(1))
		gomega.Expect(bMock.Requests()).To(gomega.BeEmpty(),
			"B 只是转述者：它说的话不产生任何一轮，被拨通的是 C")
	})
})

// spawnHost brings up a headless hosting process (`tachi channel`, the only entry point that hosts a
// node service with no terminal) and owns it: a leftover host holds the node lock, which would make
// the next spec of this worker fail for a reason that has nothing to do with it (itest.md).
func spawnHost(home string) *run.Daemon {
	ginkgo.GinkgoT().Helper()
	d, err := run.Spawn(bin, ginkgo.GinkgoT().TempDir(), "channel", "--home", home)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	ginkgo.DeferCleanup(func() { _ = d.Stop() })
	return d
}

// hostPorts picks n DISTINCT ports out of this worker's block.
//
// nodePort cannot do this: it scans from the block's start every time, so calling it twice hands back
// the same number — harmless for every spec that hosts one node, and wrong for this one, which hosts
// three. The failure it produces is quiet rather than loud: the second host cannot bind, and the
// channel entry point keeps running anyway (its node service gives up and only says so in a log line),
// so the spec's readiness wait is what has to notice.
//
// Within one spec the scan is exact; across specs it skips whatever a previous spec's host may still
// hold, which is the same assumption nodePort makes.
func hostPorts(t ginkgo.GinkgoTInterface, n int) []int {
	t.Helper()
	base := nodePortBase + ginkgo.GinkgoParallelProcess()*nodePortBlockSize
	out := make([]int, 0, n)
	for port := base; port < base+nodePortBlockSize && len(out) < n; port++ {
		if portIsFree(port) {
			out = append(out, port)
		}
	}
	gomega.Expect(out).To(gomega.HaveLen(n), "worker %d has no %d free ports in %d..%d",
		ginkgo.GinkgoParallelProcess(), n, base, base+nodePortBlockSize-1)
	return out
}

// peerIdentities reads one --home's TOFU record — every identity that process has reached ITSELF over
// TLS, because the pin is filed by the outbound handshake's certificate check. For a process this one
// cannot look inside, it is the only readable answer to "who does that machine know". It returns ""
// while the file is not there yet, which is what makes it usable inside an Eventually.
func peerIdentities(home string) string {
	raw, err := os.ReadFile(filepath.Join(home, "a2a", "peer-identities.json"))
	if err != nil {
		return ""
	}
	return string(raw)
}
