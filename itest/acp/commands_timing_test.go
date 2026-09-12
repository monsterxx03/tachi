//go:build integration

package acp_test

// Available-commands delivery over the wire. Slash-command autocomplete in
// editors (Zed et al.) depends on the agent advertising AvailableCommands via
// a session/update notification AFTER the session/new (or resume/load)
// response: the editor routes notifications through a session table that is
// only populated once the response is processed, and a notification arriving
// first is dropped forever — the session then shows "Available commands:
// none" for its whole life.
//
// These specs assert the wire ordering directly (response line before
// notification line), which is the property the editors rely on, plus that a
// resumed session re-advertises its command list.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	acpapi "github.com/coder/acp-go-sdk"
	"github.com/monsterxx03/tachi/itest/acp"
	"github.com/monsterxx03/tachi/itest/harness"
	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

// wireMsg is a minimal JSON-RPC message shape for wire-order analysis.
type wireMsg struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result struct {
		SessionId string `json:"sessionId"`
	} `json:"result"`
	Params struct {
		SessionId string `json:"sessionId"`
		Update    struct {
			AvailableCommands json.RawMessage `json:"availableCommands"`
		} `json:"update"`
	} `json:"params"`
}

// newSessionSequence creates n sessions and returns their IDs plus the wire
// lines observed, in arrival order.
func newSessionSequence(client *acp.Client, cwd string, n int) ([]string, []acp.WireLine) {
	sids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		resp, err := client.Conn().NewSession(context.Background(), acpapi.NewSessionRequest{
			Cwd:        cwd,
			McpServers: []acpapi.McpServer{},
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		sids = append(sids, string(resp.SessionId))
	}
	return sids, client.WireLines()
}

// firstAdvertisedCommands parses the first session/update of sid that carries an
// availableCommands payload. found reports whether the session was seen advertising at all —
// which is a different fact from "it advertised an empty list", and the specs need both.
func firstAdvertisedCommands(lines []acp.WireLine, sid string) (names []string, found bool) {
	for _, l := range lines {
		var m wireMsg
		if json.Unmarshal([]byte(l.Data), &m) != nil {
			continue
		}
		if m.Method != "session/update" || m.Params.SessionId != sid || len(m.Params.Update.AvailableCommands) == 0 {
			continue
		}
		var cmds []struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(m.Params.Update.AvailableCommands, &cmds) == nil {
			for _, c := range cmds {
				names = append(names, c.Name)
			}
		}
		return names, true
	}
	return nil, false
}

// waitForAdvertisedCommands polls the recorded wire until EVERY session in sids has been
// seen advertising its commands, then returns the snapshot the callers assert on.
//
// Taking one snapshot right after NewSession/ResumeSession races the pipe: the SDK flushes
// this notification strictly AFTER the response bytes (that ordering is the property the
// whole spec file is about), so when the response reaches us the notification may still be
// in flight. Measured on this machine: the name-list spec below failed 1 run in 17 with an
// empty name list — i.e. the snapshot was simply taken too early. Waiting for the FACT
// (the line is here) instead of a duration is what makes the spec deterministic.
func waitForAdvertisedCommands(client *acp.Client, sids []string, timeout time.Duration) []acp.WireLine {
	deadline := time.Now().Add(timeout)
	for {
		lines := client.WireLines()
		var missing []string
		for _, sid := range sids {
			if _, found := firstAdvertisedCommands(lines, sid); !found {
				missing = append(missing, sid)
			}
		}
		if len(missing) == 0 {
			return lines
		}
		if time.Now().After(deadline) {
			ginkgo.AddReportEntry("wire-without-commands", fmt.Sprintf(
				"%d/%d session(s) never advertised: %v\nwire lines:\n%s",
				len(missing), len(sids), missing, dumpWire(lines)))
			return lines
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// dumpWire renders the recorded lines for a failure report. The specs used to fail with a
// bare "nil does not contain elements", which cannot tell "the notification never arrived"
// from "it arrived empty" — the two have completely different causes.
func dumpWire(lines []acp.WireLine) string {
	var b strings.Builder
	for _, l := range lines {
		var m wireMsg
		if json.Unmarshal([]byte(l.Data), &m) != nil {
			continue
		}
		if m.Method != "session/update" {
			fmt.Fprintf(&b, "[%d] %s\n", l.Order, truncateWire(l.Data, 140))
			continue
		}
		fmt.Fprintf(&b, "[%d] session/update sid=%q commands=%s\n", l.Order, m.Params.SessionId,
			truncateWire(string(m.Params.Update.AvailableCommands), 100))
	}
	return b.String()
}

func truncateWire(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

var _ = ginkgo.Describe("ACP available-commands delivery", func() {
	ginkgo.It("session/new response 先于 available-commands 通知到达 (slash command 可补全)", func() {
		mock := mockllm.NewServer()
		ginkgo.DeferCleanup(mock.Close)
		home := harness.NewHome(ginkgo.GinkgoT(), mock)

		client, err := acp.Start(bin, home)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ginkgo.DeferCleanup(client.Close)

		const n = 40
		sids, lines := newSessionSequence(client, home, n)
		// One snapshot right after the last response would race the pipe for that session's
		// notification (see waitForAdvertisedCommands) and turn into a "missing" count that
		// has nothing to do with the ordering being asserted here.
		lines = waitForAdvertisedCommands(client, sids, 3*time.Second)

		byOrder := map[int]wireMsg{}
		for _, l := range lines {
			var m wireMsg
			if json.Unmarshal([]byte(l.Data), &m) == nil {
				byOrder[l.Order] = m
			}
		}

		violations := 0
		missing := 0
		for _, sid := range sids {
			respOrder, cmdOrder := -1, -1
			for order, m := range byOrder {
				if len(m.ID) > 0 && m.Result.SessionId == sid {
					respOrder = order
				}
				if m.Method == "session/update" && m.Params.SessionId == sid && len(m.Params.Update.AvailableCommands) > 0 {
					cmdOrder = order
				}
			}
			switch {
			case respOrder < 0 || cmdOrder < 0:
				missing++
			case cmdOrder < respOrder:
				violations++
			}
		}

		gomega.Expect(missing).To(gomega.Equal(0), "every session must receive an available-commands notification")
		gomega.Expect(violations).To(gomega.Equal(0),
			"%d/%d sessions got their command list BEFORE the session/new response — Zed would drop it and show no slash commands",
			violations, n)
	})

	ginkgo.It("resume_session 后重新下发 available-commands 通知", func() {
		mock := mockllm.NewServer()
		ginkgo.DeferCleanup(mock.Close)
		home := harness.NewHome(ginkgo.GinkgoT(), mock)

		client, err := acp.Start(bin, home)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ginkgo.DeferCleanup(client.Close)

		// Create + close a session so it exists on disk, then resume it.
		resp, err := client.Conn().NewSession(context.Background(), acpapi.NewSessionRequest{
			Cwd:        home,
			McpServers: []acpapi.McpServer{},
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		sid := resp.SessionId
		_, err = client.Conn().CloseSession(context.Background(), acpapi.CloseSessionRequest{SessionId: sid})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		_, err = client.Conn().ResumeSession(context.Background(), acpapi.ResumeSessionRequest{
			SessionId:  sid,
			Cwd:        home,
			McpServers: []acpapi.McpServer{},
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// The resumed session must re-advertise its command list, strictly
		// after the resume response (same routing rule as session/new). Wait for the
		// notification to actually arrive — a snapshot taken now races the pipe.
		lines := waitForAdvertisedCommands(client, []string{string(sid)}, 3*time.Second)
		var resumeOrder, cmdOrder = -1, -1
		for _, l := range lines {
			var m wireMsg
			if json.Unmarshal([]byte(l.Data), &m) != nil {
				continue
			}
			if len(m.ID) > 0 && m.Result.SessionId == string(sid) {
				resumeOrder = l.Order
			}
			if m.Method == "session/update" && m.Params.SessionId == string(sid) && len(m.Params.Update.AvailableCommands) > 0 {
				cmdOrder = l.Order
			}
		}
		gomega.Expect(resumeOrder).To(gomega.BeNumerically(">", 0), "resume response must be on the wire")
		gomega.Expect(cmdOrder).To(gomega.BeNumerically(">", 0), "resumed session must re-advertise available commands")
		gomega.Expect(cmdOrder).To(gomega.BeNumerically(">", resumeOrder), "command notification must follow the resume response")
	})

	ginkgo.It("available-commands 列表包含全部 ACP 静态命令", func() {
		mock := mockllm.NewServer()
		ginkgo.DeferCleanup(mock.Close)
		home := harness.NewHome(ginkgo.GinkgoT(), mock)

		client, err := acp.Start(bin, home)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ginkgo.DeferCleanup(client.Close)

		sids, lines := newSessionSequence(client, home, 1)
		// Wait for the notification itself: the response arriving first is the property under
		// test, but it also means the notification can still be in flight when we look.
		lines = waitForAdvertisedCommands(client, sids, 3*time.Second)

		// The first available-commands notification of the session, and its names.
		names, advertised := firstAdvertisedCommands(lines, sids[0])
		gomega.Expect(advertised).To(gomega.BeTrue(),
			"the session must advertise available commands at all")
		gomega.Expect(names).To(gomega.ContainElements(
			"commit", "review", "init", "compact", "usage", "mcp", "skill", "transcript", "research",
		), "all ACP static commands must be advertised")
	})
})
