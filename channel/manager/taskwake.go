package manager

import (
	"context"
	"time"

	"github.com/monsterxx03/tachi/agent/systemreminder"
	"github.com/monsterxx03/tachi/agent/tasks"
	"github.com/monsterxx03/tachi/cron"
)

const (
	// taskWakeSettle collects a burst of finished tasks into one turn.
	taskWakeSettle = 800 * time.Millisecond
)

// taskQueueFor returns the thread's task queue, creating it (and its watcher) on
// first use. The queue is per THREAD rather than per agent because channel agents
// are cached per thread and a compact turn even builds a throwaway one; a
// background process must report to the conversation, not to whichever agent
// happens to be around when it exits.
func (m *Manager) taskQueueFor(threadID string) *tasks.Queue {
	m.taskMu.Lock()
	defer m.taskMu.Unlock()
	if q := m.taskQueues[threadID]; q != nil {
		return q
	}
	q := tasks.New()
	m.taskQueues[threadID] = q
	go m.watchTaskQueue(m.taskCtx, threadID, q)
	return q
}

// bindTaskConversation records which thread owns a logical conversation. It is
// what lets an event (which carries the conversation, not the thread) find its
// queue — including an event produced by the shared ProcessManager on behalf of
// any thread.
func (m *Manager) bindTaskConversation(logicalID, threadID string) {
	if logicalID == "" || threadID == "" {
		return
	}
	m.taskMu.Lock()
	m.taskThreads[logicalID] = threadID
	m.taskMu.Unlock()
}

// queueForThread returns a thread's queue without creating one (nil when the
// thread has never run here).
func (m *Manager) queueForThread(threadID string) *tasks.Queue {
	m.taskMu.Lock()
	defer m.taskMu.Unlock()
	return m.taskQueues[threadID]
}

// postTaskEvent is the shared ProcessManager's sink: a finished local task finds
// the queue of the thread whose conversation it belongs to.
func (m *Manager) postTaskEvent(e tasks.Event) {
	m.taskMu.Lock()
	threadID := m.taskThreads[e.Conversation]
	q := m.taskQueues[threadID]
	m.taskMu.Unlock()
	if q == nil {
		return
	}
	q.Post(e)
}

// PostTask hands a finished task to the channel thread that owns a conversation
// and reports whether the channel took it. The host's sink calls it first: a
// conversation a channel owns must be continued by the channel, not by a second
// runtime that happens to have a queue for it.
func (m *Manager) PostTask(logicalID string, e tasks.Event) bool {
	m.taskMu.Lock()
	threadID, ok := m.taskThreads[logicalID]
	q := m.taskQueues[threadID]
	m.taskMu.Unlock()
	if !ok || q == nil {
		return false
	}
	q.Post(e)
	return true
}

// pauseTaskQueue holds a thread's automatic wake until its next user message.
func (m *Manager) pauseTaskQueue(threadID string) {
	if q := m.queueForThread(threadID); q != nil {
		q.Pause()
	}
}

// resumeTaskQueue releases the hold. Called on every inbound user turn: the user
// is here, which is the condition the pause waits for.
func (m *Manager) resumeTaskQueue(threadID string) {
	if q := m.queueForThread(threadID); q != nil {
		q.Resume()
	}
}

// watchTaskQueue turns "something finished" into "the conversation continues".
// It is the idle half of the queue: a turn that is already running takes the
// same events before its next request (BackgroundTaskReminder), so an event is
// reported once.
func (m *Manager) watchTaskQueue(ctx context.Context, threadID string, q *tasks.Queue) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-q.Signals():
		}

		timer := time.NewTimer(taskWakeSettle)
	settle:
		for {
			select {
			case <-q.Signals():
				if !timer.Stop() {
					<-timer.C
				}
				timer.Reset(taskWakeSettle)
			case <-timer.C:
				break settle
			case <-ctx.Done():
				timer.Stop()
				return
			}
		}
		if q.Paused() {
			continue
		}
		events := q.Pending()
		if len(events) == 0 {
			continue
		}
		m.wakeThread(ctx, threadID, events)
	}
}

// wakeThread runs one turn whose user message is the finished tasks, and delivers
// the reply to the thread like any other turn.
//
// It rides the cron trigger's machinery on purpose: that is the one existing path
// that acquires a thread's agent, loads the thread's session, runs a turn and
// delivers the answer — and a wake turn is that, with a different prompt. The
// synthetic job is never scheduled or stored; it only names the thread and the
// text.
func (m *Manager) wakeThread(ctx context.Context, threadID string, events []tasks.Event) {
	block := systemreminder.RenderPieces([]systemreminder.Piece{{
		Name:  "completed-tasks",
		Lines: systemreminder.TaskLines(events),
	}})
	if block == "" {
		return
	}
	job := &cron.Job{
		ID:             "task-" + threadID,
		Name:           "任务完成",
		TargetType:     cron.TargetTypeChannel,
		TargetThreadID: threadID,
		Prompt:         block,
		Notify:         cron.NotifyAlways,
	}
	if err := m.OnCronTrigger(ctx, job); err != nil {
		m.logger.Error(ctx, "channel: task wake failed", err, "thread", threadID, "tasks", len(events))
	}
}
