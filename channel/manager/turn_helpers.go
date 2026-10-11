package manager

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/channel"
	"github.com/monsterxx03/tachi/pkg/strutil"
	"github.com/monsterxx03/tachi/session"
)

// channelMaxAttachmentBytes is what this entry will hand an IM platform. It is the CHANNEL's own
// limit, not the tool's: the platform drops a file past it, so refusing here is the difference
// between a model told "too large" and a reader receiving nothing.
const channelMaxAttachmentBytes = 50 * 1024 * 1024

// prepareThreadSession returns a ready-to-use session manager and prior LLM
// history for the given thread. If an existing session exists for the
// ThreadID it is reloaded; otherwise a fresh session is created. All errors
// are logged and degraded — callers receive a usable (sm, history) pair or
// (nil, nil) when even the fallback path fails.
//
// Used by every entry point that runs an agent turn on a thread:
// runAgentTurn (cached + compact) and OnCronTrigger.
func (m *Manager) prepareThreadSession(threadID string, resolved *llm.ResolvedProvider) (*session.Manager, []llm.Message) {
	sm, priorHistory, err := m.loadThreadSession(threadID, resolved)
	if err != nil {
		m.logger.Error(context.Background(), "channel: session setup failed", err, "thread", threadID)
		sm = m.newSessionManager()
		priorHistory = nil
	}

	if sm != nil && !sm.HasCurrent() {
		providerName, wd := m.resolveThreadDefaults(threadID, resolved.Name)
		if _, err := sm.New(providerName, wd); err != nil {
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
	t.SetCallback(func(name, mimeType, localPath string) error {
		// The IM platforms' own limit, enforced where it belongs: this is the receiver that knows
		// it, and the tool reports the refusal to the model as a failed call.
		if info, err := os.Stat(localPath); err == nil && info.Size() > channelMaxAttachmentBytes {
			return fmt.Errorf("a file of %s cannot be sent to this chat: the limit is %s",
				strutil.HumanBytes(info.Size()), strutil.HumanBytes(channelMaxAttachmentBytes))
		}
		sink.mu.Lock()
		sink.list = append(sink.list, channel.OutgoingAttachment{
			Type:      channel.AttachmentTypeFile,
			FileName:  name,
			MimeType:  mimeType,
			LocalPath: localPath,
		})
		sink.mu.Unlock()
		return nil
	})
	return t, sink
}
