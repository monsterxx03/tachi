// Package acp implements the Agent Client Protocol (ACP) server for Tachi.
// It bridges the ACP SDK's Agent interface with Tachi's AIAgent, enabling
// editors (Zed, VS Code, etc.) to use Tachi as their AI backend over JSON-RPC 2.0 stdio.
package acp

import (
	"context"
	"fmt"
	"os"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/agent/wdctx"
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
			LoadSession: true,
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
				Image:           true,
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
	resolved, err := config.Resolve(t.cfg)
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
	// Don't set steer channel — ACP doesn't use mid-turn injection

	// Configure agent (registers tools, connects MCP, sets up memory/skills)
	mcpMgr, err := aiAgent.Configure(ctx, t.cfg)
	if err != nil {
		t.logger.Log("ACP: agent configure warning: %v", err)
	}

	// Connect editor-provided MCP servers (if any)
	if len(req.McpServers) > 0 && mcpMgr != nil {
		editorServers := convertMCPServers(req.McpServers, t.cfg.ACP.MCPConflictPolicy, t.cfg.MCPServers)
		if len(editorServers) > 0 {
			editorTools, errs := mcpMgr.ConnectAll(ctx, editorServers)
			for _, e := range errs {
				t.logger.Log("ACP: editor MCP connect error: %v", e)
			}
			for _, tool := range editorTools {
				aiAgent.RegisterTool(tool)
			}
		}
	}

	// Remove AskUser tool — ACP has no interactive question flow
	aiAgent.UnregisterTool(tools.ToolNameAskUser)

	// Set up session manager for persistence
	sm, smErr := session.NewManager()
	if smErr != nil {
		t.logger.Log("ACP: session manager init warning: %v", smErr)
	} else {
		sm.SetMaxKeep(t.cfg.SessionCleanupMaxCount)
		sm.New(resolved.Provider.Type, resolved.Provider.Model, cwd)
		aiAgent.SetSessionManager(sm)
	}

	// Create ACP session
	sess := t.sessions.New(ctx, cwd, resolved.Provider.Type, t.cfg, aiAgent, mcpMgr, sm)

	// Wire up permission handler for this session
	if t.conn != nil {
		aiAgent.SetPermissionHandler(buildPermissionHandler(t.conn, sess.ID, aiAgent))
	}

	// Defer available commands notification to avoid a race condition on the
	// client side: the session/update notification must arrive AFTER the
	// session/new response, because client-side ACP libraries (e.g. agentic.nvim)
	// subscribe to session updates only *after* receiving the session/new
	// response callback. If the notification arrives first, the subscriber
	// hasn't been registered yet and the update gets silently dropped.
	//
	// Using time.AfterFunc(0, ...) ensures the notification is sent from a
	// separate goroutine, so the NewSession response (written synchronously
	// by the SDK after this function returns) hits the wire first.
	acpCommands := buildACPAvailableCommands(aiAgent)
	sessID := acp.SessionId(sess.ID)
	time.AfterFunc(0, func() {
		if t.conn != nil {
			_ = t.conn.SessionUpdate(context.Background(), acp.SessionNotification{
				SessionId: sessID,
				Update: acp.SessionUpdate{
					AvailableCommandsUpdate: &acp.SessionAvailableCommandsUpdate{
						AvailableCommands: acpCommands,
					},
				},
			})
		}
	})

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

	// Create cancellable prompt context with working directory.
	// Derive from ctx (SDK's per-prompt cancellable context) so the SDK's
	// built-in session/cancel mechanism can interrupt the agent loop.
	promptCtx, promptCancel := context.WithCancel(ctx)
	defer promptCancel()
	sess.setPromptCancel(promptCancel)
	defer sess.setPromptCancel(nil)

	// Bind session's working directory to context so tools resolve relative paths correctly
	promptCtx = wdctx.WithDir(promptCtx, sess.cwd)

	// Convert ACP content blocks to Tachi message
	userMsg, userImages := convertContentBlocks(req.Prompt)

	// ---- Slash command interception ----
	if cmd, args := parseSlashCommand(userMsg, sess.agent); cmd != nil {
		t.logger.Log("ACP: slash command detected: %s (args=%q)", cmd.Name, args)
		stopReason, err := cmd.Handler(promptCtx, sess, t.conn, args)
		if err != nil {
			return acp.PromptResponse{}, err
		}
		return acp.PromptResponse{StopReason: stopReason}, nil
	}
	// ---- END ----

	// Attach images (if any) for multi-modal LLM input.
	if len(userImages) > 0 {
		sess.agent.SetPendingImages(userImages)
	}

	// Build history from cache (populated on previous turns) or disk (first turn)
	var history []llm.Message
	if sess.history != nil {
		history = sess.history
	} else if sess.sessMgr != nil {
		msgs, err := sess.sessMgr.LoadMessages()
		if err == nil && len(msgs) > 0 {
			llmMsgs, convErr := agent.ConvertSessionToLLMMessages(msgs, sess.providerType)
			if convErr == nil {
				history = llmMsgs
			} else {
				t.logger.Log("ACP: Prompt ConvertSessionToLLMMessages failed: %v", convErr)
			}
		}
	}

	// Build system prompt (use session cwd for environment info)
	systemPrompt := buildSystemPromptForCwd(t.cfg.Language, sess.cwd)

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

// ListSessions lists active in-memory sessions, optionally filtered by cwd.
// Also includes recent sessions from disk that match the filter.
func (t *TachiAgent) ListSessions(_ context.Context, req acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	t.logger.Log("ACP: ListSessions called")

	// Start with in-memory sessions
	sessions := t.sessions.List()
	infos := make([]acp.SessionInfo, 0, len(sessions))
	inMemory := make(map[string]bool)

	for _, sess := range sessions {
		if req.Cwd != nil && *req.Cwd != sess.cwd {
			continue
		}
		info := acp.SessionInfo{
			SessionId: acp.SessionId(sess.ID),
			Cwd:       sess.cwd,
		}
		if sess.sessMgr != nil {
			if cur := sess.sessMgr.Current(); cur != nil {
				if cur.Title != "" {
					info.Title = &cur.Title
				}
				ts := cur.UpdatedAt.Format("2006-01-02T15:04:05Z")
				info.UpdatedAt = &ts
			}
		}
		infos = append(infos, info)
		inMemory[sess.ID] = true
	}

	// Also scan disk for recent sessions not currently in-memory
	diskMgr, err := session.NewManager()
	if err == nil {
		diskSessions, listErr := diskMgr.List()
		if listErr == nil {
			for _, ds := range diskSessions {
				if inMemory[ds.ID] {
					continue
				}
				if req.Cwd != nil && *req.Cwd != ds.WorkingDir {
					continue
				}
				info := acp.SessionInfo{
					SessionId: acp.SessionId(ds.ID),
					Cwd:       ds.WorkingDir,
				}
				if ds.Title != "" {
					info.Title = &ds.Title
				}
				ts := ds.UpdatedAt.Format("2006-01-02T15:04:05Z")
				info.UpdatedAt = &ts
				infos = append(infos, info)
			}
		}
	}

	return acp.ListSessionsResponse{
		Sessions: infos,
	}, nil
}

// ResumeSession resumes an existing session. If the session is still in-memory,
// it's simply acknowledged. If not, we attempt to reload from disk.
func (t *TachiAgent) ResumeSession(ctx context.Context, req acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	t.logger.Log("ACP: ResumeSession called for session %s", req.SessionId)

	// Check if already in memory
	if _, ok := t.sessions.Get(string(req.SessionId)); ok {
		return acp.ResumeSessionResponse{}, nil
	}

	// Try to load from disk
	diskMgr, err := session.NewManager()
	if err != nil {
		return acp.ResumeSessionResponse{}, fmt.Errorf("session manager: %w", err)
	}

	sessionID := string(req.SessionId)
	loaded, err := diskMgr.Load(sessionID)
	if err != nil {
		return acp.ResumeSessionResponse{}, fmt.Errorf("load session %s: %w", sessionID, err)
	}

	cwd := req.Cwd
	if cwd == "" {
		cwd = loaded.WorkingDir
	}

	// Rebuild AIAgent for this resumed session
	resolved, err := config.Resolve(t.cfg)
	if err != nil {
		return acp.ResumeSessionResponse{}, fmt.Errorf("resolve provider: %w", err)
	}

	// Use session's original provider if available
	provType := resolved.Provider.Type
	provModel := resolved.Provider.Model
	if loaded.Provider != "" {
		provType = loaded.Provider
	}
	if loaded.Model != "" {
		provModel = loaded.Model
	}

	provider, err := llm.NewProvider(
		provType,
		resolved.Provider.APIKey,
		resolved.Provider.BaseURL,
		provModel,
	)
	if err != nil {
		return acp.ResumeSessionResponse{}, fmt.Errorf("create provider: %w", err)
	}

	aiAgent := agent.NewAIAgent(provider, provModel, 0)
	aiAgent.SetPermissionMode(agent.PermissionModeExternal)
	aiAgent.SetContextWindow(resolved.Provider.ContextWindow)

	mcpMgr, cfgErr := aiAgent.Configure(ctx, t.cfg)
	if cfgErr != nil {
		t.logger.Log("ACP: resume configure warning: %v", cfgErr)
	}

	aiAgent.UnregisterTool(tools.ToolNameAskUser)
	aiAgent.SetSessionManager(diskMgr)

	// Create ACP session with existing disk session manager
	sess := t.sessions.New(ctx, cwd, provType, t.cfg, aiAgent, mcpMgr, diskMgr)

	// Wire up permission handler
	if t.conn != nil {
		aiAgent.SetPermissionHandler(buildPermissionHandler(t.conn, sess.ID, aiAgent))
	}

	t.logger.Log("ACP: session resumed id=%s (disk session: %s)", sess.ID, sessionID)
	return acp.ResumeSessionResponse{}, nil
}

// LoadSession creates a new ACP session, optionally restoring a previous
// Tachi session from disk.
//
// Two lookup modes:
//   - If req.SessionId is provided, loads that specific session (ACP spec path).
//   - Otherwise, scans disk for the most recent session matching req.Cwd
//     (fallback for editors that don't track session IDs across restarts).
//
// If no matching session exists, a fresh session is created (same as NewSession).
func (t *TachiAgent) LoadSession(ctx context.Context, req acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
	t.logger.Log("ACP: LoadSession called, sessionId=%s, cwd=%s", req.SessionId, req.Cwd)

	cwd := req.Cwd
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return acp.LoadSessionResponse{}, fmt.Errorf("cannot determine working directory: %w", err)
		}
	}

	// Try to load an existing session from disk first (before creating provider,
	// so we can use the session's stored provider/model if available).
	sm, smErr := session.NewManager()
	var loaded *session.Session
	if smErr == nil {
		sm.SetMaxKeep(t.cfg.SessionCleanupMaxCount)

		if req.SessionId != "" {
			// Primary path: load by explicit session ID (ACP spec).
			// If the session doesn't exist on disk, return an error (unlike the cwd
			// fallback below which gracefully creates a new session). This matches
			// ResumeSession's behavior — the client explicitly requested a session
			// ID, so we should not silently create a different one.
			s, loadErr := sm.Load(string(req.SessionId))
			if loadErr != nil {
				t.logger.Log("ACP: LoadSession cannot load sessionId=%s: %v", req.SessionId, loadErr)
				return acp.LoadSessionResponse{}, fmt.Errorf("session not found on disk: %s", req.SessionId)
			}
			loaded = s
			t.logger.Log("ACP: LoadSession loaded by sessionId=%s", req.SessionId)
		} else {
			// Fallback: scan disk by cwd (editors that don't track session IDs)
			loaded = findLatestSessionByCwd(sm, cwd)
		}
	} else {
		t.logger.Log("ACP: LoadSession session manager init warning: %v", smErr)
		// If session manager failed AND a specific session was requested, we can't proceed.
		if req.SessionId != "" {
			return acp.LoadSessionResponse{}, fmt.Errorf("session manager unavailable, cannot load session: %s", req.SessionId)
		}
	}

	// Resolve provider from config
	resolved, err := config.Resolve(t.cfg)
	if err != nil {
		return acp.LoadSessionResponse{}, fmt.Errorf("resolve provider: %w", err)
	}

	// Use session's stored provider/model if available (matches ResumeSession behavior)
	provType := resolved.Provider.Type
	provModel := resolved.Provider.Model
	if loaded != nil {
		if loaded.Provider != "" {
			provType = loaded.Provider
		}
		if loaded.Model != "" {
			provModel = loaded.Model
		}
	}

	provider, err := llm.NewProvider(
		provType,
		resolved.Provider.APIKey,
		resolved.Provider.BaseURL,
		provModel,
	)
	if err != nil {
		return acp.LoadSessionResponse{}, fmt.Errorf("create provider: %w", err)
	}

	// Create independent AIAgent
	aiAgent := agent.NewAIAgent(provider, provModel, 0)
	aiAgent.SetPermissionMode(agent.PermissionModeExternal)
	aiAgent.SetContextWindow(resolved.Provider.ContextWindow)

	// Configure agent (registers tools, connects MCP, sets up memory/skills)
	mcpMgr, cfgErr := aiAgent.Configure(ctx, t.cfg)
	if cfgErr != nil {
		t.logger.Log("ACP: LoadSession configure warning: %v", cfgErr)
	}

	// Connect editor-provided MCP servers
	if len(req.McpServers) > 0 && mcpMgr != nil {
		editorServers := convertMCPServers(req.McpServers, t.cfg.ACP.MCPConflictPolicy, t.cfg.MCPServers)
		if len(editorServers) > 0 {
			editorTools, errs := mcpMgr.ConnectAll(ctx, editorServers)
			for _, e := range errs {
				t.logger.Log("ACP: LoadSession editor MCP connect error: %v", e)
			}
			for _, tool := range editorTools {
				aiAgent.RegisterTool(tool)
			}
		}
	}

	// Remove AskUser tool
	aiAgent.UnregisterTool(tools.ToolNameAskUser)

	// Set session manager on AIAgent
	if sm != nil {
		if loaded != nil {
			// Session already loaded as current — AIAgent will resume from its history
			aiAgent.SetSessionManager(sm)
		} else {
			t.logger.Log("ACP: LoadSession no existing session, creating new")
			sm.New(provType, provModel, cwd)
			aiAgent.SetSessionManager(sm)
		}
	}

	// Create ACP session
	sess := t.sessions.New(ctx, cwd, provType, t.cfg, aiAgent, mcpMgr, sm)

	// Wire up permission handler
	if t.conn != nil {
		aiAgent.SetPermissionHandler(buildPermissionHandler(t.conn, sess.ID, aiAgent))
	}

	// Replay session history — required by ACP spec for session/load.
	// Must happen synchronously BEFORE the response so the client receives
	// all message history before considering the session ready.
	if loaded != nil && t.conn != nil {
		replaySessionHistory(context.Background(), t.conn, sess)
	}

	// Defer available commands notification (see NewSession for rationale)
	acpCommands := buildACPAvailableCommands(aiAgent)
	sessID := acp.SessionId(sess.ID)
	time.AfterFunc(0, func() {
		if t.conn != nil {
			_ = t.conn.SessionUpdate(context.Background(), acp.SessionNotification{
				SessionId: sessID,
				Update: acp.SessionUpdate{
					AvailableCommandsUpdate: &acp.SessionAvailableCommandsUpdate{
						AvailableCommands: acpCommands,
					},
				},
			})
		}
	})

	t.logger.Log("ACP: session loaded id=%s", sess.ID)
	return acp.LoadSessionResponse{}, nil
}

// findLatestSessionByCwd scans disk sessions and returns the most recent
// session whose WorkingDir matches the given cwd. Returns nil if none found.
func findLatestSessionByCwd(sm *session.Manager, cwd string) *session.Session {
	all, err := sm.List()
	if err != nil {
		return nil
	}
	for _, s := range all {
		if s.WorkingDir == cwd {
			// Load to make it the current session
			if _, loadErr := sm.Load(s.ID); loadErr == nil {
				return s
			}
		}
	}
	return nil
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
