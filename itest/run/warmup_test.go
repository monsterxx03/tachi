//go:build integration

package run_test

// 启动预热：一个入口起来的时候就把配置里的节点问一遍，而**没有任何人要求过它**。
//
// 这条 spec 存在的理由是一个具体的时刻：第一次需要「对端是谁」的，是第一轮对话的委派提醒和一个人
// 马上就要打开的 '#a:' 选择器 —— 而一个节点的身份只可能从一次握手（读它的 Card）里出来。所以
// 入口启动时就该把这一次握手花掉，代价是「启动的时候顺手问一句」，而不是「第一轮里等一圈网络」。
//
// 这里要证明的正是「没人要求过」：SUT 是一个一条 IM 消息都没收到过的 channel 进程，而那份 pin 记录
// 是它在启动预热里自己跟对端握手时写下的。另一半（不阻塞启动）是 node 包单测钉住的性质
// （node/delegatorcache_test.go），因为在进程这一层，一个**在回答**的对端分不出「等了」和「没等」。
//
// 覆盖不到的那一半：TUI 与 desktop 的预热走的是它们自己的 picker 列表（`node.PeerList.Probe` 在
// 装配那一刻跑一轮），两者都是包内的东西 —— desktop 有一份单测，TUI 那一行由它旁边这条同样的
// 装配逻辑背书（`tui/at_file.go` 的 syncAgentSource）。

import (
	"path/filepath"
	"strconv"
	"time"

	"github.com/monsterxx03/tachi/itest/harness"
	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/monsterxx03/tachi/itest/run"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

var _ = ginkgo.Describe("startup warmup", func() {
	ginkgo.It("channel 入口起来就跟配置里的节点握手，谁都没要求过它", func() {
		// ---- 对端：另一个真进程，无头承载，离开 loopback（于是 TLS + 要客户端证书）----
		peerMock := mockllm.NewServer()
		ginkgo.DeferCleanup(peerMock.Close)
		peerPort := nodePort(ginkgo.GinkgoT())
		peerURL := "https://127.0.0.1:" + strconv.Itoa(peerPort)
		peerHome := harness.NewHome(ginkgo.GinkgoT(), peerMock,
			harness.WithNodeListen("0.0.0.0:"+strconv.Itoa(peerPort)),
			harness.WithNodePublicURL(peerURL),
			harness.WithNodeName("tachi@warm-peer"),
			harness.WithNodeService(),
			// 让 channel 入口起得来：没有启用的 channel 它会直接报错退出。
			harness.WithChannel("mock"),
		)
		peer, err := run.Spawn(bin, ginkgo.GinkgoT().TempDir(), "channel", "--home", peerHome)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ginkgo.DeferCleanup(func() { _ = peer.Stop() })
		waitForNodeHost(ginkgo.GinkgoT(), peer, peerHome, peerPort)

		// 身份出自对端自己的密钥：预热要握的正是它。
		peerID := nodeIdentity(ginkgo.GinkgoT(), peerHome)

		// ---- SUT：另一个 channel 进程，只配了一条 peer，什么都不做 ----
		sutMock := mockllm.NewServer()
		ginkgo.DeferCleanup(sutMock.Close)
		sutHome := harness.NewHome(ginkgo.GinkgoT(), sutMock,
			harness.WithPeers("nas", peerURL),
			harness.WithChannel("mock"),
		)
		sut, err := run.Spawn(bin, ginkgo.GinkgoT().TempDir(), "channel", "--home", sutHome)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ginkgo.DeferCleanup(func() { _ = sut.Stop() })

		// 这条记录只可能是预热写下的：没有任何 IM 消息进过这个进程，而 pin 是握手唯一的落点。
		// 键是身份、名字在记录里（记录以身份为键，名字是它的字段）。
		gomega.Eventually(func() map[string]pinnedPeer {
			var pinned map[string]pinnedPeer
			if err := fileutil.ReadJSON(filepath.Join(sutHome, "a2a", "peer-identities.json"), &pinned); err != nil {
				return nil
			}
			return pinned
		}, 30*time.Second, 250*time.Millisecond).Should(gomega.HaveKey(peerID.NodeID),
			"入口起来时会自己跟配置里的节点握一次手（node.DelegatorCache.WarmUp）")
		gomega.Expect(sutMock.Requests()).To(gomega.BeEmpty(),
			"预热是握手，不是一轮对话：模型一次都没有被调用过")
	})
})
