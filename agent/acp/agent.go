// Package acp implements the Agent Client Protocol (ACP) server for Tachi.
// It bridges the ACP SDK's Agent interface with Tachi's AIAgent, enabling
// editors (Zed, VS Code, etc.) to use Tachi as their AI backend over JSON-RPC 2.0 stdio.
package acp

import (
	"context"
	"fmt"
	"os"

	acp "github.com/coder/acp-go-sdk"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/debuglog"
	"github.com/monsterxx03/tachi/session"
)

// TachiAgent implements the acp.Agent interface, bridging ACP protocol
// calls to Tachi's AIAgent instances.
type TachiAgent struct {
	cfg      *config.Config
	version  string
	sessions *ACPSessionManager
	conn     *acp.AgentSideConnection
	logger   *debuglog.Logger
}

// NewTachiAgent creates a new ACP agent backed by the given config.
func NewTachiAgent(cfg *config.Config, version string) *TachiAgent {
	return &TachiAgent{
		cfg:      cfg,
		version:  version,
		sessions: NewACPSessionManager(),
		logger:   debuglog.DefaultLogger,
	}
}

// SetConnection stores the SDK connection for sending notifications back to the client.
func (t *TachiAgent) SetConnection(conn *acp.AgentSideConnection) {
	t.conn = conn
}

// Initialize handles the ACP initialize handshake, advertising Tachi's capabilities.
func (t *TachiAgent) Initialize(_ context.Context, _ acp.InitializeRequest) (acp.InitializeResponse, error) {
	t.logger.Log("ACP: Initialize called")
	return acp.InitializeResponse{
		ProtocolVersion: acp.ProtocolVersionNumber,
		AgentCapabilities: acp.AgentCapabilities{
			LoadSession: false,
			SessionCapabilities: acp.SessionCapabilities{
				List:   &acp.SessionListCapabilities{},
				Close:  &acp.SessionCloseCapabilities{},
				Resume: &acp.SessionResumeCapabilities{},
			},
			McpCapabilities: acp.McpCapabilities{
				Http: true,
				Sse:  false,
			},
			PromptCapabilities: acp.PromptCapabilities{
				Image:           false,
				Audio:           false,
				EmbeddedContext: true,
			},
		},
		AgentInfo: &acp.Implementation{
			Name:    "tachi",
			Title:   acp.Ptr("Tachi"),
			Version: t.version,
		},
	}, nil
}

// NewSession creates a new ACP session with an independent AIAgent instance.
func (t *TachiAgent) NewSession(ctx context.Context, req acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	t.logger.Log("ACP: NewSession called, cwd=%s", req.Cwd)

	cwd := req.Cwd
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return acp.NewSessionResponse{}, fmt.Errorf("cannot determine working directory: %w", err)
		}
	}

	// Resolve provider from config
	resolved, err := config.Resolve(t.cfg, config.CLIFlags{})
	if err != nil {
		return acp.NewSessionResponse{}, fmt.Errorf("resolve provider: %w", err)
	}

	provider, err := llm.NewProvider(
		resolved.Provider.Type,
		resolved.Provider.APIKey,
		resolved.Provider.BaseURL,
		resolved.Provider.Model,
	)
	if err != nil {
		return acp.NewSessionResponse{}, fmt.Errorf("create provider: %w", err)
	}

	// Create independent AIAgent (no iteration limit for ACP sessions)
	aiAgent := agent.NewAIAgent(provider, resolved.Provider.Model, 0)
	aiAgent.SetPermissionMode(agent.PermissionModeExternal)
	aiAgent.SetContextWindow(resolved.Provider.ContextWindow)
	aiAgent.SetTitleGenEnabled(false) // Editor manages titles
	// Don't set steer channel — ACP doesn't use mid-turn injection

	// Configure agent (registers tools, connects MCP, sets up memory/skills)
	mcpMgr, err := aiAgent.Configure(ctx, t.cfg)
	if err != nil {
		t.logger.Log("ACP: agent configure warning: %v", err)
	}

	// Remove AskUser tool — ACP has no interactive question flow
	aiAgent.UnregisterTool(tools.ToolNameAskUser)

	// Set up session manager for persistence
	sm, smErr := session.NewManager()
	if smErr != nil {
		t.logger.Log("ACP: session manager init warning: %v", smErr)
	} else {
		sm.New(resolved.Provider.Type, resolved.Provider.Model, cwd)
		aiAgent.SetSessionManager(sm)
	}

	// Create ACP session
	sess := t.sessions.New(ctx, cwd, resolved.Provider.Type, aiAgent, mcpMgr, sm)

	// Wire up permission handler for this session
	if t.conn != nil {
		aiAgent.SetPermissionHandler(buildPermissionHandler(t.conn, sess.ID))
	}

	t.logger.Log("ACP: session created id=%s", sess.ID)
	return acp.NewSessionResponse{
		SessionId: acp.SessionId(sess.ID),
	}, nil
}

// Prompt handles a user prompt within an existing session.
// This is a blocking call — it runs the full agent loop and streams updates via SessionUpdate notifications.
func (t *TachiAgent) Prompt(ctx context.Context, req acp.PromptRequest) (acp.PromptResponse, error) {
	sess, ok := t.sessions.Get(string(req.SessionId))
	if !ok {
		return acp.PromptResponse{}, fmt.Errorf("session not found: %s", req.SessionId)
	}

	t.logger.Log("ACP: Prompt called for session %s", sess.ID)

	// Serialize prompts within a session (defensive — ACP protocol guarantees sequential)
	sess.mu.Lock()
	defer sess.mu.Unlock()

	// Create cancellable prompt context
	promptCtx, promptCancel := context.WithCancel(sess.ctx)
	defer promptCancel()
	sess.setPromptCancel(promptCancel)
	defer sess.setPromptCancel(nil)

	// Convert ACP content blocks to Tachi message
	userMsg := convertContentBlocks(req.Prompt)

	// Build history from session
	var history []llm.Message
	if sess.sessMgr != nil {
		msgs, err := sess.sessMgr.LoadMessages()
		if err == nil && len(msgs) > 0 {
			llmMsgs, convErr := agent.ConvertSessionToLLMMessages(msgs, sess.providerType)
			if convErr == nil {
				history = llmMsgs
			}
		}
	}

	// Build system prompt
	systemPrompt := buildSystemPrompt(t.cfg.Language)

	// Run the agent loop (blocking)
	eventCh := sess.agent.RunConversationStream(promptCtx, history, userMsg, systemPrompt, llm.ChatOptions{
		MaxTokens: config.DefaultMaxTokens,
	})

	// Stream events to ACP client
	stopReason := streamToACP(promptCtx, sess, t.conn, eventCh)

	return acp.PromptResponse{
		StopReason: stopReason,
	}, nil
}

// Cancel cancels an ongoing prompt for the given session.
func (t *TachiAgent) Cancel(_ context.Context, req acp.CancelNotification) error {
	sess, ok := t.sessions.Get(string(req.SessionId))
	if !ok {
		return nil // session not found, silently ignore
	}

	t.logger.Log("ACP: Cancel called for session %s", sess.ID)

	sess.mu.Lock()
	cancel := sess.promptCancel
	sess.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	return nil
}

// CloseSession closes an active session and frees resources.
func (t *TachiAgent) CloseSession(_ context.Context, req acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	sess, ok := t.sessions.Get(string(req.SessionId))
	if !ok {
		return acp.CloseSessionResponse{}, nil
	}

	t.logger.Log("ACP: CloseSession called for session %s", sess.ID)
	sess.Close()
	t.sessions.Delete(sess.ID)
	return acp.CloseSessionResponse{}, nil
}

// ListSessions lists all known ACP sessions.
func (t *TachiAgent) ListSessions(_ context.Context, req acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	t.logger.Log("ACP: ListSessions called")

	sessions := t.sessions.List()
	infos := make([]acp.SessionInfo, 0, len(sessions))
	for _, sess := range sessions {
		info := acp.SessionInfo{
			SessionId: acp.SessionId(sess.ID),
			Cwd:       sess.cwd,
		}
		// Filter by cwd if specified
		if req.Cwd != nil && *req.Cwd != sess.cwd {
			continue
		}
		infos = append(infos, info)
	}

	return acp.ListSessionsResponse{
		Sessions: infos,
	}, nil
}

// ResumeSession resumes an existing session.
func (t *TachiAgent) ResumeSession(_ context.Context, req acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	_, ok := t.sessions.Get(string(req.SessionId))
	if !ok {
		return acp.ResumeSessionResponse{}, fmt.Errorf("session not found: %s", req.SessionId)
	}

	t.logger.Log("ACP: ResumeSession called for session %s", req.SessionId)
	// Session is still in memory — just acknowledge
	return acp.ResumeSessionResponse{}, nil
}

// Authenticate is not supported — returns empty response.
func (t *TachiAgent) Authenticate(_ context.Context, _ acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, nil
}

// SetSessionConfigOption is a stub — not yet supported.
func (t *TachiAgent) SetSessionConfigOption(_ context.Context, _ acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	return acp.SetSessionConfigOptionResponse{}, nil
}

// SetSessionMode is a stub — not yet supported.
func (t *TachiAgent) SetSessionMode(_ context.Context, _ acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, nil
}

// CloseAll closes all sessions. Called on process exit.
func (t *TachiAgent) CloseAll() {
	t.sessions.CloseAll()
}
