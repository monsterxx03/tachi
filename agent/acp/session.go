package acp

import (
	"context"
	"sync"

	"github.com/google/uuid"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/session"
)

// ACPSession represents a single ACP session with its own AIAgent instance.
type ACPSession struct {
	ID           string
	cwd          string
	providerType string
	cfg          *config.Config // for slash command handler access (provider resolution, language, etc.)

	agent   *agent.AIAgent
	mcpMgr  *mcp.Manager
	sessMgr *session.Manager

	ctx          context.Context
	cancel       context.CancelFunc
	promptCancel context.CancelFunc
	mu           sync.Mutex
}

// setPromptCancel stores or clears the cancel function for the current prompt.
func (s *ACPSession) setPromptCancel(fn context.CancelFunc) {
	s.promptCancel = fn
}

// Close cancels the session context and releases resources.
func (s *ACPSession) Close() {
	s.cancel()
	if s.mcpMgr != nil {
		s.mcpMgr.Close()
	}
	if s.sessMgr != nil {
		s.sessMgr.EndCurrent()
	}
}

// ACPSessionManager manages all active ACP sessions.
type ACPSessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*ACPSession
}

// NewACPSessionManager creates a new session manager.
func NewACPSessionManager() *ACPSessionManager {
	return &ACPSessionManager{
		sessions: make(map[string]*ACPSession),
	}
}

// New creates a new ACPSession with the given parameters.
// The ACP session ID is derived from the Tachi session ID (if available) to maintain
// a direct mapping between ACP sessions and on-disk session storage.
func (sm *ACPSessionManager) New(
	parentCtx context.Context,
	cwd string,
	providerType string,
	cfg *config.Config,
	aiAgent *agent.AIAgent,
	mcpMgr *mcp.Manager,
	sessMgr *session.Manager,
) *ACPSession {
	sessCtx, sessCancel := context.WithCancel(context.Background())
	_ = parentCtx // parentCtx not used for session lifecycle (sessions outlive individual requests)

	// Use Tachi session ID as ACP session ID for direct mapping.
	// This allows editors to resume sessions by the same ID that's stored on disk.
	id := uuid.New().String()
	if sessMgr != nil {
		if cur := sessMgr.Current(); cur != nil {
			id = cur.ID
		}
	}

	sess := &ACPSession{
		ID:           id,
		cwd:          cwd,
		providerType: providerType,
		cfg:          cfg,
		agent:        aiAgent,
		mcpMgr:       mcpMgr,
		sessMgr:      sessMgr,
		ctx:          sessCtx,
		cancel:       sessCancel,
	}

	sm.mu.Lock()
	sm.sessions[sess.ID] = sess
	sm.mu.Unlock()

	return sess
}

// Get retrieves a session by ID.
func (sm *ACPSessionManager) Get(id string) (*ACPSession, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	sess, ok := sm.sessions[id]
	return sess, ok
}

// Delete removes a session from the manager.
func (sm *ACPSessionManager) Delete(id string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	delete(sm.sessions, id)
}

// List returns all active sessions.
func (sm *ACPSessionManager) List() []*ACPSession {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	result := make([]*ACPSession, 0, len(sm.sessions))
	for _, sess := range sm.sessions {
		result = append(result, sess)
	}
	return result
}

// CloseAll closes all sessions and frees resources.
func (sm *ACPSessionManager) CloseAll() {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	for _, sess := range sm.sessions {
		sess.Close()
	}
	sm.sessions = make(map[string]*ACPSession)
}
