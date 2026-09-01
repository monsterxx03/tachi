package manager

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// --- /new ---

func (m *Manager) handleNewCommand(threadID string) (string, error) {
	// Cancel any running agent turn first, so the next message
	// starts a fresh conversation rather than being steered
	// into the old turn. Also cancel any running one-off command
	// (/commit, /review) — /new is the user's "reset everything" escape
	// hatch after a mis-issued /review 10.
	m.cancelThreadTurn(threadID)
	m.cancelOneoff(threadID)

	// Drop the cached AIAgent for this thread so any state that
	// accumulated during the previous session (skill activation,
	// reminder cadence, MCP discovered set, etc.) is reset.
	m.evictAgent(threadID)

	sm := m.newSessionManager()
	if sm == nil {
		return "", fmt.Errorf("session manager unavailable")
	}

	sess, err := sm.FindByThreadID(threadID)
	if err != nil {
		m.logger.Error(context.Background(), "channel: /new find session failed", err, "thread", threadID)
	}

	if sess != nil {
		// Clear the ThreadID on the old session so FindByThreadID won't
		// match it on the next message, then end the current session.
		sm.SetThreadID("")
		sm.EndCurrent()
		m.logger.Info(context.Background(), "channel: /new ended session", "id", sess.ID, "thread", threadID)
	}

	// If the thread has defaults (e.g. configured via the group
	// announcement), immediately start a fresh session with them so the
	// next message runs in the configured directory with the configured
	// provider — and the reply can confirm what was applied.
	applied := ""
	if _, ok := m.threadDefaultsFor(threadID); ok {
		providerName, wd := m.resolveThreadDefaults(threadID, m.defaultResolvedProvider.Name)
		ns, err := sm.New(providerName, wd)
		if err != nil {
			m.logger.Error(context.Background(), "channel: /new create default session failed", err, "thread", threadID)
		} else {
			sm.SetThreadID(threadID)
			wdMsg := wd
			if wdMsg == "" {
				wdMsg = initialWorkDir()
			}
			applied = fmt.Sprintf("\n✅ 已应用群配置：工作目录 `%s` · provider `%s`", wdMsg, providerName)
			m.logger.Info(context.Background(), "channel: /new session with thread defaults", "thread", threadID, "session", ns.ID, "workdir", wd, "provider", providerName)
		}
	}

	return "✅ Started a new conversation. Previous session has been ended." + applied, nil
}

// --- /cd ---

// handleCDCommand changes the working directory for the current thread.
// The new directory takes effect on the next agent turn; all tools (Bash,
// Read, Write, Edit, Glob, etc.) resolve relative paths against it.
func (m *Manager) handleCDCommand(threadID, dir string) (string, error) {
	if dir == "" {
		return "Usage: /cd <directory>", nil
	}

	// Expand ~ to home directory.
	if strings.HasPrefix(dir, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot expand ~: %w", err)
		}
		if dir == "~" {
			dir = home
		} else {
			dir = filepath.Join(home, dir[1:])
		}
	}

	// Read or create the cached agent — on a new thread before the first
	// message, no cache entry exists yet. We create a lightweight placeholder
	// (agent == nil) to track the workDir; the AIAgent is lazily built by
	// acquireAgent on the first message.
	m.agentCacheMu.Lock()
	if m.agentCache == nil {
		m.agentCache = make(map[string]*cachedAgent)
	}
	ca, ok := m.agentCache[threadID]
	if !ok {
		// Fetch provider name outside the lock to avoid lock ordering
		// issues with the agent cache.
		m.agentCacheMu.Unlock()
		curName := m.getProviderForThread(threadID).Name
		m.agentCacheMu.Lock()
		if ca, ok = m.agentCache[threadID]; !ok {
			ca = &cachedAgent{
				providerName: curName,
				workDir:      initialWorkDir(),
			}
			m.agentCache[threadID] = ca
		}
	}
	curDir := ca.workDir
	m.agentCacheMu.Unlock()

	// Resolve path relative to current workDir.
	target := dir
	if !filepath.IsAbs(dir) {
		target = filepath.Join(curDir, dir)
	}

	// Clean and verify.
	target = filepath.Clean(target)
	info, err := os.Stat(target)
	if err != nil {
		return fmt.Sprintf("❌ Directory %q does not exist", target), nil
	}
	if !info.IsDir() {
		return fmt.Sprintf("❌ %q is not a directory", target), nil
	}

	// Update the cached agent's workDir under lock.
	m.agentCacheMu.Lock()
	if ca, ok := m.agentCache[threadID]; ok {
		ca.workDir = target
	}
	m.agentCacheMu.Unlock()

	// Persist the new working directory to the thread's session metadata
	// so it survives restarts. This is best-effort; the in-memory cache has
	// already been updated.
	m.persistThreadWorkDir(threadID, target)

	return fmt.Sprintf("✅ Working directory changed to `%s`", target), nil
}

// --- /stop ---

// handleStopCommand stops the currently-running LLM turn for the given
// thread — both agent turns (threadActivation) and one-off commands
// (/commit, /review registered via registerOneoff). If nothing is running,
// returns a message indicating that (instead of a misleading "stopped").
func (m *Manager) handleStopCommand(threadID string) (string, error) {
	if m.cancelThreadTurn(threadID) || m.cancelOneoff(threadID) {
		return "⏹️ 已停止当前任务。", nil
	}
	return "当前没有运行中的任务。", nil
}
