//go:build integration

package run_test

// 两个真进程的 A2A：一台机器**无头承载**节点，另一台真的委派给它。
//
// 这条 spec 覆盖的是**进程边界**，别处拿不到的那一半：`itest/run` 现有的委派 spec 把对端养在
// 测试进程里（`node.StartOrAttachNode`），它证明的是「配置 → Options → 真监听面」；这里两个端点
// 各自是一个真的 `tachi` 进程 —— 两份身份、两份 TOFU pin、两个数据目录、两把锁、两套日志，全由
// 各自的进程写出。
//
// 对端为什么是 `tachi channel`：节点服务只活在活着的进程里，而只有三个入口承载它（TUI、
// desktop、channel），其中**只有 channel 入口能无头运行** —— 但它拒绝在没有启用任何 channel 时
// 启动。`channel/mock` 就是那个什么都不连的 channel，它的 `Run` 不返回到进程被要求退出为止
// （docs/agents/itest.md 的「A node with no terminal」）。
//
// 地址是**配置里钉死的**：`local.json` 只公布本机控制接口的地址，不公布 A2A 面，所以宿主起来
// 之后没人能问出"该往哪儿拨" —— 除非 fixture 自己把端口写死。就绪信号则用 `local.json` 的存在：
// `host()` 的发布顺序是固定的（锁 → 本地面 → store → 远端面 → 才 publish），所以它出现就等于
// 远端面已经在听。

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/itest/harness"
	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/monsterxx03/tachi/itest/run"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/flock"
	"github.com/monsterxx03/tachi/session"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

var _ = ginkgo.Describe("delegation to another process", func() {
	ginkgo.It("一台无头进程承载节点，另一台进程委派给它", func() {
		// ---- 对端：真二进制，无头承载，离开 loopback（于是 TLS + 要客户端证书）----
		peerMock := mockllm.NewServer()
		ginkgo.DeferCleanup(peerMock.Close)
		peerMock.Script(mockllm.Step{
			Require: mockllm.HasUserMessage(containsSubstring("不修改文件")),
			Reply: mockllm.Stream(
				mockllm.Text("备份检查完成：两个目录异常"),
				mockllm.Finish("stop"),
				mockllm.Done(),
			),
		})

		peerPort := nodePort(ginkgo.GinkgoT())
		peerWorkspace := filepath.Join(ginkgo.GinkgoT().TempDir(), "peer-ws")
		peerHome := harness.NewHome(ginkgo.GinkgoT(), peerMock,
			harness.WithNodeListen("0.0.0.0:"+strconv.Itoa(peerPort)),
			harness.WithNodePublicURL("https://127.0.0.1:"+strconv.Itoa(peerPort)),
			harness.WithNodeName("tachi@headless"),
			harness.WithNodeWorkspace(peerWorkspace),
			harness.WithNodeService(),
			// 让 channel 入口起得来：没有启用的 channel 它会直接报错退出。
			harness.WithChannel("mock"),
		)

		peer, err := run.Spawn(bin, ginkgo.GinkgoT().TempDir(), "channel", "--home", peerHome)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ginkgo.DeferCleanup(func() { _ = peer.Stop() })
		waitForNodeHost(ginkgo.GinkgoT(), peer, peerHome, peerPort)

		// 模型点名的是身份：它出自对端的密钥，脚本要在对端把这个文件写下之后才写得出来。
		peerID := nodeIdentity(ginkgo.GinkgoT(), peerHome)

		// 它真的是**另一个进程** —— 这条断言是整条 spec 的立足点，而 local.json 里就写着它的 pid。
		var info struct {
			PID int `json:"pid"`
		}
		gomega.Expect(fileutil.ReadJSON(filepath.Join(peerHome, "a2a", "local.json"), &info)).To(gomega.Succeed())
		gomega.Expect(info.PID).NotTo(gomega.Equal(os.Getpid()),
			"对端必须是另一个进程，否则这条 spec 和 itest/run 里已有的那些没有区别")

		// 而它是**承载**方，不是附着到别人身上的那一方。
		gomega.Expect(nodeLog(ginkgo.GinkgoT(), peerHome)).To(gomega.ContainSubstring("hosting local node service"))

		// ---- 发起侧：真二进制，节点服务关着，只配了一条 https 的 peer ----
		sutMock := mockllm.NewServer()
		ginkgo.DeferCleanup(sutMock.Close)
		sutMock.Script(
			mockllm.Step{Reply: mockllm.Stream(
				mockllm.ToolCallStart("call_p1", tools.ToolNameDelegateTask,
					`{"node":"`+peerID.NodeID+`","prompt":"检查昨天的备份是否完整，返回异常清单，不修改文件。"}`),
				mockllm.Finish("tool_calls"),
				mockllm.Done(),
			)},
			mockllm.Step{
				Require: mockllm.HasToolResult("call_p1", containsSubstring("备份检查完成：两个目录异常")),
				Reply: mockllm.Stream(
					mockllm.Text("远端说对了：两个目录没有备份完。"),
					mockllm.Finish("stop"),
					mockllm.Done(),
				),
			},
		)
		sutHome := harness.NewHome(ginkgo.GinkgoT(), sutMock,
			harness.WithPeers("nas", "https://127.0.0.1:"+strconv.Itoa(peerPort)),
			harness.WithNodeName("tachi@mac"),
		)
		workdir := harness.SeedWorkDir(ginkgo.GinkgoT(), nil)

		res := run.Binary(bin, workdir, "--home", sutHome, "-p",
			"检查一下备份，活儿在另一台机器上", "-o", "json-stream")
		gomega.Expect(res.Err).To(gomega.BeNil())
		gomega.Expect(res.ExitCode).To(gomega.Equal(0), "stdout=%s stderr=%s", res.Stdout, res.Stderr)
		gomega.Expect(res.Stdout).To(gomega.ContainSubstring("远端说对了"))
		gomega.Expect(peerMock.Requests()).To(gomega.HaveLen(1), "活儿是在对端那个进程里跑的一轮")

		// ---- 两份身份：各由各的进程写出，互不相同 ----
		sutID := nodeIdentity(ginkgo.GinkgoT(), sutHome)
		gomega.Expect(peerID.NodeID).NotTo(gomega.Equal(sutID.NodeID))

		// ---- 发起侧：pin 住的是对端进程的身份，记录绑定的也是身份 ----
		var pinned map[string]pinnedPeer
		gomega.Expect(fileutil.ReadJSON(filepath.Join(sutHome, "a2a", "peer-identities.json"), &pinned)).To(gomega.Succeed())
		gomega.Expect(pinned[peerID.NodeID].Alias).To(gomega.Equal("nas"),
			"pin 是这一侧的进程在握手时写下的，键是身份、名字在记录里")

		record := string(readOnlyRecord(ginkgo.GinkgoT(), filepath.Join(sutHome, "a2a", "delegations"), "d_"))
		gomega.Expect(record).To(gomega.ContainSubstring(`"peer_node_id": "` + peerID.NodeID + `"`))

		// ---- 接收侧：那个进程自己的工作记录与受管会话，说得出是谁派的 ----
		task := string(readOnlyRecord(ginkgo.GinkgoT(), filepath.Join(peerHome, "a2a", "tasks"), ""))
		gomega.Expect(task).To(gomega.ContainSubstring(`"caller_id": "` + sutID.NodeID + `"`))
		gomega.Expect(task).To(gomega.ContainSubstring(`"self_reported_name": "tachi@mac"`))

		held := receivingSession(ginkgo.GinkgoT(), peerHome)
		gomega.Expect(held.Kind).To(gomega.Equal(session.KindA2AExecution))
		gomega.Expect(held.SourceNodeID).To(gomega.Equal(sutID.NodeID))
	})

	// The single-instance rule, observed from outside: two processes, one home, and exactly one
	// of them serving. This is the acceptance item "本机已有 Desktop 时新开 TUI 不起第二个节点
	// 服务" — and it has no test in the package, because in-process there is only ever one process.
	ginkgo.It("同 --home 的第二个进程附着，而不是起第二套服务", func() {
		peerMock := mockllm.NewServer()
		ginkgo.DeferCleanup(peerMock.Close)
		peerPort := nodePort(ginkgo.GinkgoT())
		peerHome := harness.NewHome(ginkgo.GinkgoT(), peerMock,
			harness.WithNodeListen("0.0.0.0:"+strconv.Itoa(peerPort)),
			harness.WithNodePublicURL("https://127.0.0.1:"+strconv.Itoa(peerPort)),
			harness.WithNodeName("tachi@headless"),
			harness.WithNodeWorkspace(filepath.Join(ginkgo.GinkgoT().TempDir(), "ws")),
			harness.WithChannel("mock"),
		)

		host, err := run.Spawn(bin, ginkgo.GinkgoT().TempDir(), "channel", "--home", peerHome)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ginkgo.DeferCleanup(func() { _ = host.Stop() })
		waitForNodeHost(ginkgo.GinkgoT(), host, peerHome, peerPort)

		// The lock is what elects the host, and it is held: one process, not zero, not two.
		_, err = flock.TryAcquire(filepath.Join(peerHome, "a2a", "node.lock"))
		gomega.Expect(errors.Is(err, flock.ErrHeld)).To(gomega.BeTrue(),
			"the hosting process must hold the node lock (got %v)", err)

		// A second process on the SAME home, same entry point: it has to attach. Waiting on the
		// published file or the port would prove nothing here — both are the first process's.
		second, err := run.Spawn(bin, ginkgo.GinkgoT().TempDir(), "channel", "--home", peerHome)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ginkgo.DeferCleanup(func() { _ = second.Stop() })
		attached := waitForLogLine(ginkgo.GinkgoT(), second, peerHome, "attached to local node service")

		// It did NOT start a second service: no remote listener, no second host.
		gomega.Expect(attached).NotTo(gomega.ContainSubstring("hosting local node service"))
		gomega.Expect(attached).NotTo(gomega.ContainSubstring("remote interface listening"))

		// And it attached to THAT instance: the id in its log is the one the host published.
		var published struct {
			InstanceID string `json:"instance_id"`
			PID        int    `json:"pid"`
		}
		gomega.Expect(fileutil.ReadJSON(filepath.Join(peerHome, "a2a", "local.json"), &published)).To(gomega.Succeed())
		gomega.Expect(attached).To(gomega.ContainSubstring("instance="+published.InstanceID),
			"the handshake is what settles which instance a process attached to")
		gomega.Expect(published.PID).To(gomega.Equal(host.PID()),
			"the attaching process must not publish itself over the host's info")

		// The host is untouched and still serving.
		gomega.Expect(portAnswers(peerPort)).To(gomega.BeTrue())
		gomega.Expect(logLinesFor(ginkgo.GinkgoT(), peerHome, host.PID())).
			To(gomega.ContainSubstring("hosting local node service"))
	})

	// What a hard death leaves behind, and what the NEXT start makes of it (design §9.3).
	//
	// The peer is parked on a question when it is killed — a durable unfinished state that outlives
	// the goroutine waiting for the answer, which is exactly why it cannot stay "waiting" in a
	// record nobody will ever finish. Loopback on purpose: this spec is about the store, and the
	// transport is the other spec's subject.
	ginkgo.It("被 kill -9 的宿主重启后，停着的任务落成中断", func() {
		peerMock := mockllm.NewServer()
		ginkgo.DeferCleanup(peerMock.Close)
		peerMock.Script(mockllm.Step{Reply: mockllm.Stream(
			mockllm.ToolCallStart("ask_1", "AskUserQuestion",
				`{"questions":[{"question":"检查照片还是文档？","header":"范围","multiSelect":false}]}`),
			mockllm.Finish("tool_calls"),
			mockllm.Done(),
		)})

		peerPort := nodePort(ginkgo.GinkgoT())
		peerHome := harness.NewHome(ginkgo.GinkgoT(), peerMock,
			harness.WithNodeListen("0.0.0.0:"+strconv.Itoa(peerPort)),
			harness.WithNodePublicURL("https://127.0.0.1:"+strconv.Itoa(peerPort)),
			harness.WithNodeName("tachi@headless"),
			harness.WithNodeWorkspace(filepath.Join(ginkgo.GinkgoT().TempDir(), "ws")),
			harness.WithNodeService(),
			harness.WithChannel("mock"),
		)

		host, err := run.Spawn(bin, ginkgo.GinkgoT().TempDir(), "channel", "--home", peerHome)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ginkgo.DeferCleanup(func() { _ = host.Stop() })
		waitForNodeHost(ginkgo.GinkgoT(), host, peerHome, peerPort)

		// 点名用身份，所以脚本要等对端把自己的密钥写下来（见上面那条 spec）。
		peerID := nodeIdentity(ginkgo.GinkgoT(), peerHome)

		// The caller asks for work, the peer parks on its question, and the answer comes back as
		// the state it is in — the delegation does NOT hang (§6.2, §7.4).
		sutMock := mockllm.NewServer()
		ginkgo.DeferCleanup(sutMock.Close)
		sutMock.Script(
			mockllm.Step{Reply: mockllm.Stream(
				mockllm.ToolCallStart("call_q1", tools.ToolNameDelegateTask,
					`{"node":"`+peerID.NodeID+`","prompt":"检查一下备份"}`),
				mockllm.Finish("tool_calls"),
				mockllm.Done(),
			)},
			mockllm.Step{Reply: mockllm.Stream(
				mockllm.Text("它问我检查照片还是文档。"),
				mockllm.Finish("stop"),
				mockllm.Done(),
			)},
		)
		sutHome := harness.NewHome(ginkgo.GinkgoT(), sutMock,
			harness.WithPeers("nas", "https://127.0.0.1:"+strconv.Itoa(peerPort)),
			harness.WithNodeName("tachi@mac"),
		)
		workdir := harness.SeedWorkDir(ginkgo.GinkgoT(), nil)
		res := run.Binary(bin, workdir, "--home", sutHome, "-p", "让对端查一下备份", "-o", "json-stream")
		gomega.Expect(res.ExitCode).To(gomega.Equal(0), "stdout=%s stderr=%s", res.Stdout, res.Stderr)

		// The caller's own record says where the work went and what state it is in.
		raw := readOnlyRecord(ginkgo.GinkgoT(), filepath.Join(sutHome, "a2a", "delegations"), "d_")
		var record struct {
			RemoteTaskID string `json:"remote_task_id"`
		}
		gomega.Expect(json.Unmarshal(raw, &record)).To(gomega.Succeed())
		gomega.Expect(record.RemoteTaskID).NotTo(gomega.BeEmpty())
		// The record keeps the PROTOCOL state (what the peer reported) and the question with it —
		// the tool result the model reads is the rendered form of exactly this.
		gomega.Expect(string(raw)).To(gomega.ContainSubstring(`"state": "TASK_STATE_INPUT_REQUIRED"`))
		gomega.Expect(string(raw)).To(gomega.ContainSubstring(`"question_id": "ask_1"`))
		gomega.Expect(string(raw)).To(gomega.ContainSubstring("检查照片还是文档？"))

		// The peer's own record, before the death: unfinished, with the question in it.
		taskPath := filepath.Join(peerHome, "a2a", "tasks", record.RemoteTaskID+".json")
		before := readFile(ginkgo.GinkgoT(), taskPath)
		gomega.Expect(before).To(gomega.ContainSubstring(`"state": "TASK_STATE_INPUT_REQUIRED"`))

		// Kill it. Nothing gets to tidy up: no shutdown, no release.
		gomega.Expect(host.Kill()).To(gomega.Succeed())

		// Restarting the SAME home takes the lock the dead process released (a flock dies with its
		// holder) and resolves what was left in flight — before it serves anything, which is why
		// the file is already resolved the moment it is ready.
		restarted, err := run.Spawn(bin, ginkgo.GinkgoT().TempDir(), "channel", "--home", peerHome)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ginkgo.DeferCleanup(func() { _ = restarted.Stop() })
		waitForNodeHost(ginkgo.GinkgoT(), restarted, peerHome, peerPort)
		// Waited for rather than required to be there already: the line is what proves THIS process
		// took the lock, and a start that has not got there yet is not a failure.
		gomega.Expect(waitForLogLine(ginkgo.GinkgoT(), restarted, peerHome, "hosting local node service")).
			To(gomega.ContainSubstring("hosting local node service"),
				"the lock is free again once its holder is gone")

		after := readFile(ginkgo.GinkgoT(), taskPath)
		gomega.Expect(after).To(gomega.ContainSubstring(`"state": "TASK_STATE_FAILED"`),
			"a task nobody can finish must not keep claiming somebody is waiting for it")
		gomega.Expect(after).To(gomega.ContainSubstring("interrupt"))
		gomega.Expect(after).To(gomega.ContainSubstring("检查照片还是文档？"),
			"what was asked survives the death: it is the only thing that explains it")
	})
})

// nodePort picks the port a hosting process will bind. The fixture has to NAME it because
// nothing publishes a hosting process's A2A address (see the file comment) — so what this
// function must not do is ask the kernel for one.
//
// Asking (`:0`, then close) is a race a loader on a busy runner loses: the number comes out of
// the EPHEMERAL range, which is the same pool every outbound connection on the machine draws
// its source port from, so between closing the probe and the host binding, a dial anywhere can
// be handed the very same number. The host then fails to bind, and because the channel entry
// point survives a node failure by design (it logs "node service unavailable" and keeps serving
// channels) nothing ever listens — a readiness wait that times out with the reason sitting in
// the node's own log. Hence a per-worker block BELOW the ephemeral range (Linux 32768-60999,
// macOS 49152-65535): the kernel never hands one of these to a dialer, and no two ginkgo workers
// ask for the same one.
func nodePort(t ginkgo.GinkgoTInterface) int {
	t.Helper()
	base := nodePortBase + ginkgo.GinkgoParallelProcess()*nodePortBlockSize
	for port := base; port < base+nodePortBlockSize; port++ {
		if portIsFree(port) {
			return port
		}
	}
	t.Fatalf("no free port in %d..%d (worker %d)", base, base+nodePortBlockSize-1,
		ginkgo.GinkgoParallelProcess())
	return 0
}

const (
	// nodePortBase sits below every platform's ephemeral range — see nodePort.
	nodePortBase = 21000
	// nodePortBlockSize is one worker's share: room for the handful of ports a worker needs, and
	// small enough that the blocks of the workers that exist do not overlap.
	nodePortBlockSize = 100
)

// portIsFree reports whether a port can be bound right now. Inside the reserved block this is
// only ever about a port somebody else already has (nothing else is asking for these), so the
// close-then-bind window that makes the same trick unusable on an ephemeral port is harmless.
// The probe is on 0.0.0.0 because that is the stricter question: it fails if anything holds the
// port on ANY interface, which is what a host binding 0.0.0.0 needs.
func portIsFree(port int) bool {
	ln, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// waitForNodeHost waits until the hosting process is really serving, and reports its own output
// when it dies trying.
//
// Two signals, and the FIRST one alone is a trap: `local.json` outlives the process that wrote it
// (`Close` leaves it in place on purpose — the attach path re-checks the instance id and the
// handshake, so a stale file is harmless THERE), and a host that dies right after publishing
// leaves it behind. So the file says "this process got as far as publishing" and the DIAL says
// "something is answering on the A2A port" — only the second one is what this wait needs.
//
// What makes the file worth checking at all is the order the host itself fixes: lock → local
// listener → task store → REMOTE SURFACE → publish. A dial is enough on its own in principle, but
// pairing them keeps the intent readable and catches a host that published and then died.
//
// A process that EXITS while we wait is reported with its output: "never became ready" sends a
// reader looking in the wrong place, and the answer (an enabled channel that is not registered, a
// taken port, a bad config) is sitting in that output. The node's OWN reason reaches the log file
// and not stderr — a node service that fails leaves the channel entry point serving channels —
// so every failure here reports those lines too, and one that gave up fails at once.
func waitForNodeHost(t ginkgo.GinkgoTInterface, d *run.Daemon, home string, port int) {
	t.Helper()
	deadline := time.Now().Add(nodeHostTimeout)
	local := filepath.Join(home, "a2a", "local.json")
	for {
		if _, err := os.Stat(local); err == nil && portAnswers(port) {
			return
		}
		if exited, err := d.Exited(); exited {
			t.Fatalf("the hosting process exited before it was ready (%v)\nstdout:\n%s\nstderr:\n%s\nlog:\n%s",
				err, d.Stdout(), d.Stderr(), processLog(home, d.PID()))
		}
		// A node service that GAVE UP is not a slow start: the channel entry point keeps the
		// process alive after one (by design), so nothing will ever listen. Report the reason now
		// rather than spend the whole budget on a process that is done trying.
		if log := processLog(home, d.PID()); strings.Contains(log, nodeServiceGaveUp) {
			t.Fatalf("the hosting process is up but its node service gave up, so nothing serves on %d\nlog:\n%s",
				port, log)
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("the node is not serving after %s\nstdout:\n%s\nstderr:\n%s\nlog:\n%s",
				nodeHostTimeout, d.Stdout(), d.Stderr(), processLog(home, d.PID()))
		}
		time.Sleep(nodeHostPollInterval)
	}
}

// nodeServiceGaveUp is the line main_channels.go writes when the node service could not start.
// Nothing on stderr says so — the process carries on serving channels — which is exactly why a
// readiness wait has to read the log to have anything to report.
const nodeServiceGaveUp = "node service unavailable"

// processLog is one process's own lines out of a log several processes may append to, read
// without failing when the file is not there: it is only ever called on failure paths, where a
// missing file must not mask the reason.
func processLog(home string, pid int) string {
	raw, err := os.ReadFile(filepath.Join(home, "logs", "debug.log"))
	if err != nil {
		return ""
	}
	return linesWithPid(string(raw), strconv.Itoa(pid))
}

// portAnswers reports whether anything accepts a connection on a local port. A TCP connection is
// all it can check: the A2A surface on a non-loopback address wants a client certificate, so the
// handshake is expected to fail, and a listener that refuses the connection outright is the thing
// being told apart from one that is not there.
func portAnswers(port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

const (
	nodeHostTimeout      = 15 * time.Second
	nodeHostPollInterval = 50 * time.Millisecond
)

// waitForLogLine waits until one process's own lines contain needle, and returns those lines.
//
// A second process on a home that ALREADY has a host cannot use waitForNodeHost: the published
// file and the listening port are the FIRST process's, so it would return before the second one
// had done anything at all. What proves the second one is up is its own log line — attributed by
// pid, since both processes append to the same file.
func waitForLogLine(t ginkgo.GinkgoTInterface, d *run.Daemon, home, needle string) string {
	t.Helper()
	deadline := time.Now().Add(nodeHostTimeout)
	pid := strconv.Itoa(d.PID())
	for {
		if lines := linesWithPid(nodeLog(t, home), pid); strings.Contains(lines, needle) {
			return lines
		}
		if exited, err := d.Exited(); exited {
			t.Fatalf("the process exited before it logged %q (%v)\nstdout:\n%s\nstderr:\n%s",
				needle, err, d.Stdout(), d.Stderr())
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("no line with pid=%s said %q within %s\nstdout:\n%s\nstderr:\n%s",
				pid, needle, nodeHostTimeout, d.Stdout(), d.Stderr())
		}
		time.Sleep(nodeHostPollInterval)
	}
}

// linesWithPid is the pid-attributed subset of a log that several processes may append to.
func linesWithPid(log, pid string) string {
	needle := "pid=" + pid
	var mine []string
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, needle) {
			mine = append(mine, line)
		}
	}
	return strings.Join(mine, "\n")
}

// nodeLog is what the hosting process said, read from its own log file (`<home>/logs/`) rather
// than scraped from stderr: that is where a person would look, and it stays complete.
func nodeLog(t ginkgo.GinkgoTInterface, home string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, "logs", "debug.log"))
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return string(raw)
}

// logLinesFor is one process's lines out of a log file that two processes may be appending to —
// which is what happens when they share a --home. Every line the node writes carries its pid, so
// the pid is what attributes a line to the process that wrote it.
func logLinesFor(t ginkgo.GinkgoTInterface, home string, pid int) string {
	t.Helper()
	mine := linesWithPid(nodeLog(t, home), strconv.Itoa(pid))
	gomega.Expect(mine).NotTo(gomega.BeEmpty(), "no log lines carry pid=%d", pid)
	return mine
}

// readFile is a raw read of one file a process wrote, for the assertions that are about its
// CONTENT rather than about a field some accessor exposes.
func readFile(t ginkgo.GinkgoTInterface, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return string(raw)
}
