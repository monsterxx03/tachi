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
		// after the resume response (same routing rule as session/new).
		lines := client.WireLines()
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

		_, lines := newSessionSequence(client, home, 1)

		// Grab the first available-commands notification and check its names.
		var names []string
		for _, l := range lines {
			var m wireMsg
			if json.Unmarshal([]byte(l.Data), &m) != nil {
				continue
			}
			if m.Method == "session/update" && len(m.Params.Update.AvailableCommands) > 0 {
				var cmds []struct {
					Name string `json:"name"`
				}
				if json.Unmarshal(m.Params.Update.AvailableCommands, &cmds) == nil {
					for _, c := range cmds {
						names = append(names, c.Name)
					}
				}
				break
			}
		}
		gomega.Expect(names).To(gomega.ContainElements(
			"commit", "review", "init", "compact", "usage", "mcp", "skill", "transcript", "research",
		), "all ACP static commands must be advertised")
	})
})
