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
//
// 环境那道闸在用例最开头（probeMulticast），而且它用的是**库本身**，不是 Tachi 的浏览：用被测代码做
// 探针等于什么都没分开 —— 闸门要分的正是「这个网络不行」和「我们坏了」这两件事。闸门过了之后再等不
// 到公告，就是这段代码的问题，所以那时它是**失败**而不是 skip。

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/libp2p/zeroconf/v2"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/itest/harness"
	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/monsterxx03/tachi/node"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

// discoveryWait is how long the browsing side is given to hear an announcement. Multicast has no
// retry contract to lean on — the library re-queries on its own schedule — so this is generous.
const discoveryWait = 20 * time.Second

var _ = ginkgo.Describe("discovery", func() {
	ginkgo.It("公告 → 浏览 → 连上 → 入目录 → 点名委派", func() {
		// ---- the gate: what this machine can do, before anything is started ----
		ifaces, err := node.LANInterfaces()
		if err == nil {
			err = probeMulticast(ifaces)
		}
		if err != nil {
			ginkgo.Skip(fmt.Sprintf("%s (%s)", skipNoMulticastHere, err))
		}

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
		// 一个开关管两半：开了就既浏览、又公告自己，而公告只发生在承载节点服务的那个进程里 ——
		// 也就是对端这个进程（node/discovery.go 那一侧）。它没有发送侧，所以这里真正生效的只有公告。
		peerCfg.Node.Discovery.Mdns.Enabled = true

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

		// No onSighting hook: this spec reads DirectoryRows itself, so a sighting has nowhere it
		// needs pushing — the three entry points without a live view pass nil for the same reason.
		// No session root either: this caller has no conversations, so a file a peer sent back would
		// go to the spill directory (the branch storeFile takes for a caller with no session).
		cache := node.NewDelegatorCache("tachi@discovery-caller", node.Discovery{Enabled: true},
			logger.New("itest-discovery"), nil, "")
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
			"the peer announced itself and this machine has to find it; the probe said this machine "+
				"can multicast, so a browse that hears nothing is this code's problem")
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

// skipNoMulticastHere is what this spec says when the machine running it cannot multicast at all: the
// probe below proved it, so this run says nothing about this code either way. It exists because
// multicast is simply absent on some networks — a container, a firewall, a WiFi that drops it, a
// machine whose only LAN interface carries no address a peer could be handed.
const skipNoMulticastHere = "this machine cannot do multicast mDNS; discovery cannot be exercised here"

const (
	// probeInstance and probeService are the announcement the gate sends. The service type is not
	// Tachi's own, so nothing on this network answers the probe by accident: the only thing it can
	// hear is its own announcement.
	probeInstance = "tachi-itest-probe"
	probeService  = "_tachi-itest-probe._tcp"
	// probePort is never connected to; the announcement is the whole point of the probe.
	probePort = 34567
	// probeWait bounds the probe, and it is shorter than discoveryWait on purpose: a machine that
	// cannot multicast pays this on every run.
	probeWait = 8 * time.Second
)

// probeMulticast reports whether the library can carry mDNS over these interfaces at all: an
// announcement and a browse in one process, which is what a discovery round is. Nil means it worked,
// and the error is what the skip above says when it did not.
//
// It is the library DIRECTLY — no Tachi announce, no Tachi browse — and that is the whole value of it:
// the question is whether this machine's network can carry the packets, and a probe assembled out of
// the code under test cannot tell "the network cannot" from "our browse is broken". The system
// resolver is not an answer either: macOS's mDNSResponder keeps seeing the whole network on machines
// where a process cannot receive multicast on its own socket, which is exactly the case this gate
// exists for.
//
// The interfaces are the ones discovery itself would browse on (node.LANInterfaces): a probe over a
// set this machine would never use answers a question nobody asked.
func probeMulticast(ifaces []net.Interface) error {
	if len(ifaces) == 0 {
		return fmt.Errorf("no interface to probe")
	}
	server, err := zeroconf.Register(probeInstance, probeService, "local.", probePort, nil, ifaces)
	if err != nil {
		return fmt.Errorf("announcing over %s failed: %w", ifaceNames(ifaces), err)
	}
	defer server.Shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), probeWait)
	defer cancel()
	entries := make(chan *zeroconf.ServiceEntry)
	seen := make(chan struct{}, 1)
	go func() {
		for range entries {
			select {
			case seen <- struct{}{}:
			default:
			}
		}
	}()
	// IPv4 alone, and that is the narrowest question both ends can answer: the library's own default
	// (both families) refuses to browse at all when an interface it was given has no IPv6 address, so
	// asking it that way would report a dead network on the IPv4-only leases where Tachi's browse
	// works fine (node/discovery.go trafficFor).
	go func() {
		_ = zeroconf.Browse(ctx, probeService, "local.", entries,
			zeroconf.SelectIfaces(ifaces), zeroconf.SelectIPTraffic(zeroconf.IPv4))
	}()

	select {
	case <-seen:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("no announcement came back on %s: multicast is not usable here (a container, "+
			"a firewall, or a network with no multicast-capable interface)", ifaceNames(ifaces))
	}
}

// ifaceNames spells an interface set out for a message, which is what the skip text needs: which
// interfaces were tried is the first thing to check when the answer is "nothing came back".
func ifaceNames(ifaces []net.Interface) string {
	names := make([]string, 0, len(ifaces))
	for _, iface := range ifaces {
		names = append(names, iface.Name)
	}
	return strings.Join(names, ", ")
}
