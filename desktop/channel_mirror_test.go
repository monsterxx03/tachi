package main

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/cron"
	"github.com/monsterxx03/tachi/session"
)

// 手机与「channel 拥有的会话」之间那条线：手机可以说，但这一轮必须由那个会话的渠道运行时跑
// （desktop/channel_mirror.go）。这些用例钉住的是路由和拒绝，不是投影的每一个事件 —— 投影走的
// 是 handleEventIn 这一条现有路，mobile_new*.go 里的用例已经在看它。

// fakeChannelTurns stands in for the process's IM runtime. It records what it was asked to run and
// can hold a turn open, which is what makes "in flight" observable.
type fakeChannelTurns struct {
	mu       sync.Mutex
	turns    []fakeChannelTurn
	refusal  error
	platform string
	block    chan struct{}
	events   []agent.AgentEvent
}

type fakeChannelTurn struct {
	threadID string
	content  string
}

func (f *fakeChannelTurns) SubmitTurn(ctx context.Context, threadID, content string, observe func(agent.AgentEvent)) (string, error) {
	f.mu.Lock()
	f.turns = append(f.turns, fakeChannelTurn{threadID: threadID, content: content})
	block, events := f.block, f.events
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	for _, ev := range events {
		if observe != nil {
			observe(ev)
		}
	}
	return "渠道那边的回答", nil
}

func (f *fakeChannelTurns) ExternalRefusal(string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refusal
}

func (f *fakeChannelTurns) ThreadPlatform(string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.platform
}

func (f *fakeChannelTurns) recorded() []fakeChannelTurn {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeChannelTurn(nil), f.turns...)
}

// channelSession is a conversation a channel thread owns, as far as this process can tell.
func channelSession(t *testing.T, store *session.FileStore, id, threadID string) *session.Session {
	t.Helper()
	return addSession(t, store, &session.Session{ID: id, Title: id, ThreadID: threadID})
}

// waitFor polls a condition that another goroutine satisfies. The turn runs off the request, so
// every "it happened" assertion here has to wait for it rather than read once.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestMobileSend_RefusesAChannelSessionWithoutARuntime(t *testing.T) {
	d, store := cronTestApp(t)
	sv := channelSession(t, store, "s1", "discord:1")

	status, code := d.mobileSend(sv.ID, "你好", "send", nil)
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, "channel_offline", code, "with no channel runtime this side must not run the turn itself")
}

func TestMobileSend_RoutesAChannelSessionToTheRuntime(t *testing.T) {
	d, store := cronTestApp(t)
	sv := channelSession(t, store, "s1", "discord:1")
	turns := &fakeChannelTurns{}
	d.setChannels(turns)

	status, code := d.mobileSend(sv.ID, "你好", "send", nil)
	require.Zero(t, status, "accepted: %s", code)
	require.Empty(t, code)

	waitFor(t, "the turn to run", func() bool { return len(turns.recorded()) == 1 })
	got := turns.recorded()[0]
	assert.Equal(t, "discord:1", got.threadID, "the turn belongs to the session's thread")
	assert.Contains(t, got.content, "你好")
}

func TestMobileSend_RefusesACommandAnsweredInTheChannel(t *testing.T) {
	d, store := cronTestApp(t)
	sv := channelSession(t, store, "s1", "discord:1")
	turns := &fakeChannelTurns{refusal: errors.New("answered over there")}
	d.setChannels(turns)

	status, code := d.mobileSend(sv.ID, "/model", "send", nil)
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, "channel_command", code)
	assert.Empty(t, turns.recorded(), "a refused command must not reach the runtime")
}

// TestChannelTurn_ReadsAsRunningAndTakesSteer: while the channel is running a turn for this
// conversation, the phone must see it as running — that is what turns its next message into steer
// instead of a second turn (and what gives its Stop something to hit).
func TestChannelTurn_ReadsAsRunningAndTakesSteer(t *testing.T) {
	d, store := cronTestApp(t)
	sv := channelSession(t, store, "s1", "weixin:acct:user")
	release := make(chan struct{})
	turns := &fakeChannelTurns{block: release}
	d.setChannels(turns)

	status, code := d.mobileSend(sv.ID, "第一句", "send", nil)
	require.Zero(t, status, code)

	waitFor(t, "the mirror to report running", func() bool {
		running, _ := d.mobileRunState(sv.ID)
		return running
	})
	running, hasSteer := d.mobileRunState(sv.ID)
	assert.True(t, running)
	assert.True(t, hasSteer, "a channel turn steers — that is what a mid-turn message becomes")

	status, code = d.mobileSend(sv.ID, "补一句", "steer", nil)
	require.Zero(t, status, code)
	waitFor(t, "the steer to reach the runtime", func() bool { return len(turns.recorded()) == 2 })

	close(release)
	waitFor(t, "the turn to finish", func() bool {
		running, _ := d.mobileRunState(sv.ID)
		return !running
	})
}

// TestChannelTurn_StopAndSendStartsTheNextTurn: "interrupt" stops the conversation's running turn and
// waits for it, because the new text must reach a conversation that is no longer answering the old
// one — and the new turn, not the old one's cleanup, is what the phone must then see as running.
func TestChannelTurn_StopAndSendStartsTheNextTurn(t *testing.T) {
	d, store := cronTestApp(t)
	sv := channelSession(t, store, "s1", "discord:1")
	release := make(chan struct{})
	turns := &fakeChannelTurns{block: release}
	d.setChannels(turns)

	status, code := d.mobileSend(sv.ID, "第一句", "send", nil)
	require.Zero(t, status, code)
	waitFor(t, "the first turn to be running", func() bool {
		running, _ := d.mobileRunState(sv.ID)
		return running
	})

	status, code = d.mobileSend(sv.ID, "改成这句", "interrupt", nil)
	require.Zero(t, status, code)
	waitFor(t, "the second turn to be accepted", func() bool { return len(turns.recorded()) == 2 })

	running, _ := d.mobileRunState(sv.ID)
	assert.True(t, running, "the new turn is running — the old one's cleanup must not unregister it")

	close(release)
	waitFor(t, "both turns to finish", func() bool {
		running, _ := d.mobileRunState(sv.ID)
		return !running
	})
}

func TestMobileSummary_NamesTheChannelThatOwnsTheConversation(t *testing.T) {
	d, store := cronTestApp(t)
	sv := channelSession(t, store, "s1", "discord:42")
	turns := &fakeChannelTurns{platform: "discord"}
	d.setChannels(turns)

	row := d.mobileSummary(sv, nil)
	assert.True(t, row.ChannelSession)
	assert.Equal(t, "discord", row.ChannelName)

	// Its own conversation is not a channel one.
	own := addSession(t, store, &session.Session{ID: "s2", Title: "自己的对话"})
	row = d.mobileSummary(own, nil)
	assert.False(t, row.ChannelSession)
	assert.Empty(t, row.ChannelName)

	// A channel conversation in a process that runs no channel (the windowed build): the fact stays,
	// the name cannot be resolved — unless the conversation's RECORD carries it, which is the durable
	// half of the same fact (written by the channel's own turn, so it survives a restart).
	d.setChannels(nil)
	row = d.mobileSummary(sv, nil)
	assert.True(t, row.ChannelSession)
	assert.Empty(t, row.ChannelName)

	sv.ChannelName = "weixin"
	require.NoError(t, d.sm.UpdateMeta(sv))
	row = d.mobileSummary(d.sessionRecord(sv.ID), nil)
	assert.Equal(t, "weixin", row.ChannelName, "the record names the channel when the live mapping cannot")
}

// TestMobilePanels_IncludeTheThreadsScheduledJobs: the phone opens a channel conversation and its
// scheduled-jobs panel names the jobs that fire INTO it — up to now they were invisible there.
func TestMobilePanels_IncludeTheThreadsScheduledJobs(t *testing.T) {
	// The cron STORE path is process-global (config.BaseDir → ~/.tachi), not part of cronTestApp's
	// temp store: without this the fixture lands in the user's own crons.json.
	config.SetBaseDir(t.TempDir())
	d, store := cronTestApp(t)
	sv := channelSession(t, store, "s1", "discord:42")
	d.setChannels(&fakeChannelTurns{platform: "discord"})

	cs := cron.NewStore(d.cronStorePath())
	require.NoError(t, cs.Create(&cron.Job{
		ID: "job-channel", Name: "群里报天气", Schedule: "0 8 * * *",
		Prompt: "今天的天气", Type: cron.JobTypeRecurring,
		TargetType: cron.TargetTypeChannel, TargetThreadID: "discord:42",
		Status: cron.JobStatusActive,
	}))
	require.NoError(t, cs.Create(&cron.Job{
		ID: "job-other-thread", Name: "别的线程", Schedule: "0 9 * * *",
		Prompt: "x", TargetType: cron.TargetTypeChannel, TargetThreadID: "discord:99",
		Status: cron.JobStatusActive,
	}))

	panels, status, code := d.mobileSessionPanels(sv.ID)
	require.Zero(t, status, code)

	byID := map[string]mobileCronRow{}
	for _, row := range panels.Cron {
		byID[row.ID] = row
	}
	row, ok := byID["job-channel"]
	require.True(t, ok, "the thread's own job must be listed")
	assert.Equal(t, "channel", row.TargetKind)
	assert.Equal(t, "discord", row.TargetTitle)
	assert.NotEmpty(t, row.NextRun, "an active job says when it runs next")
	assert.NotContains(t, byID, "job-other-thread", "another thread's job is not this conversation's")
}
