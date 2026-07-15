package manager

import (
	"context"
	"sync"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/channel"
	"github.com/monsterxx03/tachi/session"
)

// prepareThreadSession returns a ready-to-use session manager and prior LLM
// history for the given thread. If an existing session exists for the
// ThreadID it is reloaded; otherwise a fresh session is created. All errors
// are logged and degraded — callers receive a usable (sm, history) pair or
// (nil, nil) when even the fallback path fails.
//
// Used by every entry point that runs an agent turn on a thread:
// runAgentTurn (cached + compact) and OnCronTrigger.
func (m *Manager) prepareThreadSession(threadID string, resolved *config.ResolvedConfig) (*session.Manager, []llm.Message) {
	sm, priorHistory, err := m.loadThreadSession(threadID, resolved)
	if err != nil {
		m.logger.Error(context.Background(), "channel: session setup failed", err, "thread", threadID)
		sm = m.newSessionManager()
		priorHistory = nil
	}

	if sm != nil && !sm.HasCurrent() {
		if _, err := sm.New(resolved.Provider.Name, ""); err != nil {
			m.logger.Error(context.Background(), "channel: create fallback session failed", err, "thread", threadID)
		} else {
			sm.SetThreadID(threadID)
		}
	}
	return sm, priorHistory
}

// attachmentSink collects file attachments produced by the SendFile tool
// during a single turn. The sink is registered as the tool's callback;
// once the turn ends the caller calls Snapshot() to get the final list.
//
// Concurrency: SendFile may be invoked from parallel sub-tools, so the
// internal slice is mutex-guarded.
type attachmentSink struct {
	mu   sync.Mutex
	list []channel.OutgoingAttachment
}

// snapshot returns a copy of the collected attachments. Safe to call after
// the turn has ended.
func (s *attachmentSink) snapshot() []channel.OutgoingAttachment {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]channel.OutgoingAttachment, len(s.list))
	copy(out, s.list)
	return out
}

// newSendFileTool builds a SendFile tool wired to a fresh attachmentSink.
// The caller must register the returned tool on the agent for this turn,
// and read sink.Snapshot() after drainEvents returns.
func newSendFileTool() (*tools.SendFileTool, *attachmentSink) {
	sink := &attachmentSink{}
	t := tools.NewSendFileTool()
	t.SetCallback(func(name, mimeType, localPath string) {
		sink.mu.Lock()
		sink.list = append(sink.list, channel.OutgoingAttachment{
			Type:      channel.AttachmentTypeFile,
			FileName:  name,
			MimeType:  mimeType,
			LocalPath: localPath,
		})
		sink.mu.Unlock()
	})
	return t, sink
}
