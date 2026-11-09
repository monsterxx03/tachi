package tui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/systemreminder"
	"github.com/monsterxx03/tachi/agent/tasks"
)

const (
	// taskWakeSettle is how long a burst of finished tasks is collected before a
	// wake turn starts: a batch that finishes together should cost ONE turn.
	taskWakeSettle = 800 * time.Millisecond

	// wakeAskTimeout bounds one permission card or question in a wake turn. The
	// user may have walked away; a wake must not park the terminal forever.
	wakeAskTimeout = 2 * time.Minute
)

// taskWakeMsg says "the queue has something to look at". The watcher sends it;
// the Update goroutine decides whether a turn may start (the model is only
// mutated there).
type taskWakeMsg struct{}

// watchTaskQueue is the idle half of the task queue for the TUI: the reminder
// takes the same events before a request when a turn is already running, so an
// event is reported exactly once, by whichever consumer got there first.
func (m *Model) watchTaskQueue(ctx context.Context, p *tea.Program) {
	if m.agent == nil {
		return
	}
	q := m.agent.Config.TaskQueue
	if q == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-q.Signals():
		}

		// Settle: a burst becomes one batch.
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
		p.Send(taskWakeMsg{})
	}
}

// tryWake starts a wake turn when this terminal is idle and the CURRENT
// conversation has finished tasks. It deliberately takes nothing when:
//
//   - a turn is running or a modal is open (the events stay queued for the next
//     turn's reminder),
//   - the user pressed Stop (wakePaused; the next message releases it),
//   - the pending events belong to another conversation (the TUI keeps one agent
//     across sessions, so the queue can hold several conversations' events).
func (m *Model) tryWake() tea.Cmd {
	if m.agent == nil || m.wakePaused || m.state != stateIdle {
		return nil
	}
	q := m.agent.Config.TaskQueue
	if q == nil {
		return nil
	}
	events := q.PendingFor(m.currentLogicalConversation())
	if len(events) == 0 {
		return nil
	}
	return m.sendWake(events)
}

// currentLogicalConversation is the conversation the terminal is showing; its
// empty value is a scope of its own (a run with no session).
func (m *Model) currentLogicalConversation() string {
	if m.agent == nil {
		return ""
	}
	sm := m.agent.SessionManager()
	if sm == nil {
		return ""
	}
	if cur := sm.Current(); cur != nil {
		return cur.LogicalConversationID()
	}
	return ""
}

// sendWake runs one turn whose user message is the finished tasks. Everything
// else is an ordinary turn: the same history, steer channel and delegation; the
// only differences are the bound on asks (nobody may be watching) and the display
// line the transcript shows.
func (m *Model) sendWake(events []tasks.Event) tea.Cmd {
	block := systemreminder.RenderPieces([]systemreminder.Piece{{
		Name:  "completed-tasks",
		Lines: systemreminder.TaskLines(events),
	}})
	display := systemreminder.TaskDisplayText(events)

	m.chatview.AddMessage(chatMessage{Role: "user", Content: display})
	m.setState(stateWaiting)
	m.chatview.ResetStreaming()
	m.thinkingView.Reset()
	m.thinkingMode = false

	ctx := agent.WithAskTimeout(m.startTurn(), wakeAskTimeout)
	// After startTurn, which clears it: this run is the one started on its own.
	m.wakeTurn = true
	m.steerCh = make(chan agent.SteerInput, 1)
	ropts := []agent.RunOption{
		agent.WithSteerChannel(m.steerCh),
		agent.WithDisplayUserMessage(display),
	}
	if delegator := m.delegator(); delegator != nil {
		ropts = append(ropts, agent.WithDelegation(delegator))
	}

	m.eventCh = m.agent.RunConversationStream(ctx, m.history, block, m.systemPrompt, m.chatOpts, ropts...)
	return tea.Batch(
		m.statusbar.Tick(),
		m.nextEvent(),
	)
}
