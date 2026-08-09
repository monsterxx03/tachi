//go:build integration

package acp_test

// L0 — handshake & session lifecycle at the true process boundary: the
// JSON-RPC wire handshake, session creation, explicit close, and clean
// shutdown on stdin EOF. None of these make LLM calls, so the mock is
// script-less.

import (
	"context"

	acpapi "github.com/coder/acp-go-sdk"
	"github.com/monsterxx03/tachi/itest/acp"
	"github.com/monsterxx03/tachi/itest/harness"
	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

// startACPClient starts the real binary in acp mode against a fresh mock
// (no script) and home; registered for cleanup.
func startACPClient(opts ...acp.Option) (*acp.Client, string) {
	mock := mockllm.NewServer()
	ginkgo.DeferCleanup(mock.Close)
	home := harness.NewHome(ginkgo.GinkgoT(), mock)

	client, err := acp.Start(bin, home, opts...)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	ginkgo.DeferCleanup(client.Close)
	return client, home
}

// newSession opens a session for cwd via the client; requires a running
// client.
func newSession(client *acp.Client, cwd string) (acpapi.SessionId, error) {
	resp, err := client.Conn().NewSession(context.Background(), acpapi.NewSessionRequest{
		Cwd:        cwd,
		McpServers: []acpapi.McpServer{},
	})
	if err != nil {
		return "", err
	}
	return resp.SessionId, nil
}

var _ = ginkgo.Describe("ACP handshake & session lifecycle", func() {
	ginkgo.It("initialize 握手成功, 可创建会话", func() {
		client, home := startACPClient()

		// Initialize already completed inside Start; the connection is alive
		// and a session can be created — that round-trips the wire handshake.
		sid, err := newSession(client, home)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(string(sid)).NotTo(gomega.BeEmpty())
	})

	ginkgo.It("new_session 返回各自独立的 sessionId", func() {
		client, home := startACPClient()

		sid1, err := newSession(client, home)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		sid2, err := newSession(client, home)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(sid1).NotTo(gomega.Equal(sid2))
	})

	ginkgo.It("close_session 后 prompt 该会话报错", func() {
		client, home := startACPClient()

		sid, err := newSession(client, home)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		_, err = client.Conn().CloseSession(context.Background(), acpapi.CloseSessionRequest{
			SessionId: sid,
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		_, err = client.Conn().Prompt(context.Background(), acpapi.PromptRequest{
			SessionId: sid,
			Prompt:    []acpapi.ContentBlock{acpapi.TextBlock("hi")},
		})
		gomega.Expect(err).To(gomega.HaveOccurred())
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("session not found"))
	})

	ginkgo.It("stdin EOF 后 agent 优雅退出", func() {
		client, _ := startACPClient()

		// Closing stdin drives runACPAgent's connection loop to EOF → exit.
		gomega.Expect(client.Close()).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("session/new 带 additionalDirectories, session/list 可查到", func() {
		client, home := startACPClient()

		resp, err := client.Conn().NewSession(context.Background(), acpapi.NewSessionRequest{
			Cwd:                   home,
			AdditionalDirectories: []string{"/shared-lib", "/docs"},
			McpServers:            []acpapi.McpServer{},
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(string(resp.SessionId)).NotTo(gomega.BeEmpty())

		listResp, err := client.Conn().ListSessions(context.Background(), acpapi.ListSessionsRequest{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(listResp.Sessions).To(gomega.HaveLen(1))
		gomega.Expect(listResp.Sessions[0].Cwd).To(gomega.Equal(home))
		gomega.Expect(listResp.Sessions[0].AdditionalDirectories).To(gomega.Equal([]string{"/shared-lib", "/docs"}))
	})

	ginkgo.It("session/new 拒绝相对路径 additionalDirectories", func() {
		client, home := startACPClient()

		_, err := client.Conn().NewSession(context.Background(), acpapi.NewSessionRequest{
			Cwd:                   home,
			AdditionalDirectories: []string{"relative/path"},
			McpServers:            []acpapi.McpServer{},
		})
		gomega.Expect(err).To(gomega.HaveOccurred())
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("invalid_params"))
	})
})
