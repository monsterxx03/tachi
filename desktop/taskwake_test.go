package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/agent/tasks"
	"github.com/monsterxx03/tachi/session"
	"github.com/stretchr/testify/require"
)

// A batch of tasks that finish together must become ONE wake turn. The settle
// window is what makes that true: draining immediately would spend a turn per
// task.
func TestSettleAndTakeCoalescesABatch(t *testing.T) {
	d := newDesktopApp()
	q := tasks.New()

	got := make(chan []tasks.Event, 1)
	go func() { got <- d.settleAndTake(context.Background(), q, 40*time.Millisecond) }()

	// Two events inside the window: neither ends it, so both ride in the same batch.
	q.Post(tasks.Event{Name: "a", Kind: tasks.KindBackground})
	time.Sleep(10 * time.Millisecond)
	q.Post(tasks.Event{Name: "b", Kind: tasks.KindBackground})

	select {
	case events := <-got:
		require.Len(t, events, 2)
		require.Equal(t, "a", events[0].Name)
		require.Equal(t, "b", events[1].Name)
	case <-time.After(2 * time.Second):
		t.Fatal("settleAndTake never returned")
	}
}

// An event posted after the window simply belongs to the next batch.
func TestSettleAndTakeReturnsAfterTheWindow(t *testing.T) {
	d := newDesktopApp()
	q := tasks.New()
	q.Post(tasks.Event{Name: "a"})

	events := d.settleAndTake(context.Background(), q, 10*time.Millisecond)
	require.Len(t, events, 1)

	// Nothing left: the next drain is silent rather than repeating the batch.
	require.Empty(t, q.Pending())
}

// The queue is keyed by LOGICAL conversation: a compaction moves the
// conversation into a child session, and the events pending at that moment must
// still land in the same queue afterwards.
func TestTaskQueueIsPerConversation(t *testing.T) {
	d := newDesktopApp()
	a := d.taskQueueFor("conv-a")
	b := d.taskQueueFor("conv-b")

	require.NotSame(t, a, b)
	require.Same(t, a, d.taskQueueFor("conv-a"))

	a.Post(tasks.Event{Name: "a1"})
	require.Len(t, a.Pending(), 1)
	require.Empty(t, b.Pending())
}

// waitForIdle blocks while a turn is in flight and resolves the conversation's
// current leaf afterwards — the same shape cron uses, and the reason a wake
// never starts a second turn on a session that already has one.
func TestWaitForIdleWaitsForTheTurnToEnd(t *testing.T) {
	d, store := cronTestApp(t)
	addSession(t, store, &session.Session{ID: "s1", Title: "conversation"})

	r := func() *sessionRun {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.getRun("s1")
	}()

	d.mu.Lock()
	r.running = true
	done := make(chan struct{})
	r.turnDone = done
	d.mu.Unlock()

	result := make(chan string, 1)
	go func() {
		id, ok := d.waitForIdle(context.Background(), "s1")
		if ok {
			result <- id
		}
	}()

	select {
	case <-result:
		t.Fatal("waitForIdle returned while the turn was still running")
	case <-time.After(50 * time.Millisecond):
	}

	// endTurn's state: not busy, done channel gone (the turn goroutine closes its own).
	d.mu.Lock()
	r.running = false
	r.turnDone = nil
	d.mu.Unlock()
	close(done)

	select {
	case id := <-result:
		require.Equal(t, "s1", id)
	case <-time.After(2 * time.Second):
		t.Fatal("waitForIdle never returned after the turn ended")
	}
}

func TestWakeDisplayText(t *testing.T) {
	one := wakeDisplayText([]tasks.Event{{Name: "build"}})
	require.Equal(t, "🔔 build 完成", one)

	failed := wakeDisplayText([]tasks.Event{{Name: "test", ExitCode: 2}})
	require.Contains(t, failed, "test")
	require.Contains(t, failed, "退出码 2")

	many := wakeDisplayText([]tasks.Event{{Name: "a"}, {Name: "b"}, {Name: "c"}})
	require.Contains(t, many, "3 个任务完成")
	require.True(t, strings.Contains(many, "a") && strings.Contains(many, "c"))
}

// A display line is a summary, not the payload: the whole block goes to the
// model, and this is what keeps the transcript readable.
func TestWakeDisplayTextStaysShort(t *testing.T) {
	events := make([]tasks.Event, 0, 40)
	for i := 0; i < 40; i++ {
		events = append(events, tasks.Event{Name: strings.Repeat("x", 20)})
	}
	line := wakeDisplayText(events)
	require.Less(t, len([]rune(line)), 200)
}

// The user's Stop is the brake on automatic waking: it holds the conversation's
// queue until the user speaks again. Without it, a task chain would keep running
// after "stop" (wakes have no count limit).
func TestStopPausesWakeAndSpeakingResumesIt(t *testing.T) {
	d, store := cronTestApp(t)
	s := addSession(t, store, &session.Session{ID: "s1", Title: "conversation"})

	sm := session.NewManagerWithStore(store, nil)
	_, err := sm.Load(s.ID)
	require.NoError(t, err)

	d.mu.Lock()
	r := d.getRun(s.ID)
	r.sm = sm
	d.mu.Unlock()

	q := d.taskQueueFor(s.ID)
	require.False(t, q.Paused())

	// The turn ends because the user stopped it.
	d.endTurn(s.ID, "interrupted")
	require.True(t, q.Paused(), "a stopped turn must hold automatic waking")

	// The user speaks: the hold is released.
	d.resumeWake(d.wakeForSession(s.ID))
	require.False(t, q.Paused(), "a user message must release the hold")
}

func TestWakeDisplayTextForARemoteTask(t *testing.T) {
	done := wakeDisplayText([]tasks.Event{{Kind: tasks.KindRemote, Name: "d_1a2b"}})
	require.Contains(t, done, "远程任务 d_1a2b")
	require.Contains(t, done, "完成")

	waiting := wakeDisplayText([]tasks.Event{{Kind: tasks.KindRemote, Name: "d_9", Status: tasks.StatusWaiting}})
	require.Contains(t, waiting, "等待回答")
}

// 「全停」 is a slash command, so it is advertised in the palette and executable
// without an agent (the notice still tells the user what happened).
func TestStopAllIsAdvertisedAndSafeWithoutAnAgent(t *testing.T) {
	d, store := cronTestApp(t)
	addSession(t, store, &session.Session{ID: "s1", Title: "conversation"})

	found := false
	for _, c := range (&AgentService{desk: d}).ListCommands() {
		if c.Name == "stopall" {
			found = true
		}
	}
	require.True(t, found, "stopall must be offered to the composer")

	require.Equal(t, "没有活跃会话", d.stopAllTasks(""))
	require.Equal(t, "没有在跑的会话", d.stopAllTasks("s1"))
}
