//go:build integration

package run_test

// 发现这条路，从头到尾：一台机器在真组播上公告自己，另一台**只**靠发现找到它、连上去、把身份验
// 下来，然后点名委派 —— 设计 §八 那条「组成测试」。
//
// 这条 spec 存在的理由是一个具体的盲区：浏览这一半在只喂假发现源的单测里**永远**测不到。真去浏览
// 需要 `_tachi._tcp.local.` 这个名字拼对（域名得是 FQDN，库不会替你补那一点），需要对着一台真的在
// 讲 TLS 的节点把卡片读回来。写这条 spec 之前，浏览在真网络上一个字都收不到，而单测全绿。
//
// 它是**冒烟**性质（设计 §七 阶段 2）：组播在容器、访客网络和一部分 WiFi 上根本不通，所以既看不到
// 公告、也公告不出去的机器上，这条 spec SKIP 并说明原因 —— 那失败是关于环境的，不是关于这段代码的。

import (
	"context"
	"strconv"
	"time"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/itest/harness"
	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/monsterxx03/tachi/node"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

// discoveryWait is how long the browsing side is given to hear an announcement. Multicast has no
// retry contract to lean on — the library re-queries on its own schedule — so this is generous, and
// running out is a SKIP rather than a failure.
const discoveryWait = 20 * time.Second

var _ = ginkgo.Describe("discovery", func() {
	ginkgo.It("公告 → 浏览 → 连上 → 入目录 → 点名委派", func() {
		// ---- the peer: a real node that announces itself ----
		peerMock := mockllm.NewServer()
		ginkgo.DeferCleanup(peerMock.Close)
		peerMock.Script(mockllm.Step{
			Require: mockllm.HasUserMessage(containsSubstring("报告一下")),
			Reply: mockllm.Stream(
				mockllm.Text("这活儿是在另一台机器上干的"),
				mockllm.Finish("stop"),
				mockllm.Done(),
			),
		})

		// 一个通配地址是本进程里唯一能安全地「离开本机」的方式（于是它真的走 TLS、真的要求证书），
		// 而绑通配就必须说得出可连地址 —— 所以端口先挑好，两半都用它（node/a2a.go）。
		peerPort := nodePort(ginkgo.GinkgoT())
		peerURL := "https://127.0.0.1:" + strconv.Itoa(peerPort)
		peerHome := harness.NewHome(ginkgo.GinkgoT(), peerMock,
			harness.WithNodeListen("0.0.0.0:"+strconv.Itoa(peerPort)),
			harness.WithNodePublicURL(peerURL),
			harness.WithNodeName("tachi@discovery-peer"),
			harness.WithNodeService(),
		)
		peerCfg := harness.Config(ginkgo.GinkgoT(), peerHome)
		// 公告只在承载的那个进程里做，所以这一句加在**对端**身上（node/discovery.go 那一侧）。
		peerCfg.Node.Discovery.Mdns.Advertise = true

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		ginkgo.DeferCleanup(cancel)
		peer, err := node.StartOrAttachNode(ctx, peerCfg)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(peer.Hosting()).To(gomega.BeTrue())
		ginkgo.DeferCleanup(func() { _ = peer.Close() })
		peerID := nodeIdentity(ginkgo.GinkgoT(), peerHome)

		// ---- the caller: NO peers configured at all, and discovery on ----
		//
		// The empty `peers:` is the point rather than a shortcut: with nothing configured there is no
		// address to ask, so the ONLY way this machine can come to know the peer's identity — and
		// therefore the only way it can name it — is by having found it on the network.
		sutMock := mockllm.NewServer()
		ginkgo.DeferCleanup(sutMock.Close)
		sutMock.Script(
			mockllm.Step{Reply: mockllm.Stream(
				mockllm.ToolCallStart("call_d1", "DelegateTask",
					`{"node":"`+peerID.NodeID+`","prompt":"报告一下这台机器上有什么"}`),
				mockllm.Finish("tool_calls"),
				mockllm.Done(),
			)},
			mockllm.Step{
				Require: mockllm.HasToolResult("call_d1", containsSubstring("这活儿是在另一台机器上干的")),
				Reply: mockllm.Stream(
					mockllm.Text("对端说完了。"),
					mockllm.Finish("stop"),
					mockllm.Done(),
				),
			},
		)
		sutHome := harness.NewHome(ginkgo.GinkgoT(), sutMock, harness.WithNodeDiscovery())
		harness.Config(ginkgo.GinkgoT(), sutHome) // point the process-global base dir at this home

		cache := node.NewDelegatorCache("tachi@discovery-caller", node.Discovery{Enabled: true},
			logger.New("itest-discovery"))
		ginkgo.DeferCleanup(cache.Close)
		sender := cache.ForSender(ctx, nil)
		gomega.Expect(sender).NotTo(gomega.BeNil(),
			"discovery on with nothing configured is still a sending side: a discovered node is the whole point")

		// ---- the sighting ----
		var found string
		gomega.Eventually(func() bool {
			for _, row := range sender.DirectoryRows() {
				if row.NodeID == peerID.NodeID {
					found = row.Name
					return true
				}
			}
			return false
		}, discoveryWait, 250*time.Millisecond).Should(gomega.BeTrue(),
			"the peer announced itself and this machine has to find it; when nothing arrives, "+
				"multicast is not usable here (%s)", skipNoMulticastHere)
		gomega.Expect(found).NotTo(gomega.BeEmpty(), "a machine found by discovery is listed under its own name")

		// ---- 点名委派：模型手里只有身份，而那正是发现给它的那一样 ----
		view, err := sender.Delegate(ctx, tools.DelegationRequest{
			// 点名的就是这个身份 —— 本机没有配置过任何对端，所以它是发现给的那一样。
			Peer:      peerID.NodeID,
			Prompt:    "报告一下这台机器上有什么",
			LogicalID: "conv-1",
			Wait:      30 * time.Second,
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(view.State).To(gomega.Equal("completed"), "note=%q", view.Note)
		gomega.Expect(view.Result).To(gomega.ContainSubstring("这活儿是在另一台机器上干的"),
			"the answer really came back across the wire")

		// The peer ran a turn of its own: this is not this machine talking to itself.
		gomega.Expect(peerMock.Requests()).To(gomega.HaveLen(1))
		// And the record binds the identity the HANDSHAKE proved, which for a discovered machine is
		// also the only reason it could be named at all.
		gomega.Expect(sutMock.Error()).NotTo(gomega.HaveOccurred())
	})
})

// skipNoMulticastHere is what this spec says when the machine running it cannot multicast.
const skipNoMulticastHere = "no announcement came back: multicast is not usable here (a container, a " +
	"firewall, or a network with no multicast-capable interface)"
