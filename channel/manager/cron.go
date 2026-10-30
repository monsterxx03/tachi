package manager

import (
	"context"
	"fmt"
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/cron"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/channel"
)

// initCron creates the cron store and scheduler with the manager as the
// trigger handler. Must be called before Start() fires channels.
func (m *Manager) initCron(ctx context.Context) error {
	storePath := m.cfg.Cron.StorePath
	if storePath == "" {
		storePath = cron.DefaultStorePath()
	}

	store := cron.NewStore(storePath)
	scheduler := cron.NewScheduler(cron.SchedulerConfig{
		Store:            store,
		Handler:          m.OnCronTrigger,
		Logger:           m.logger,
		MaxConcurrent:    m.cfg.Cron.MaxConcurrent,
		ExecutionTimeout: m.cfg.Cron.ExecutionTimeout,
		// A job belongs to exactly one entry point. The desktop can now schedule
		// too and both processes share crons.json, so without this both would
		// fire the same job. An empty target type is an old channel job.
		Owns: func(j *cron.Job) bool {
			return j.TargetType == "" || j.TargetType == cron.TargetTypeChannel
		},
	})

	m.scheduler = scheduler
	m.initNotifyGate(ctx)
	m.logger.Info(ctx, "channel: cron initialized", "path", storePath, "max_concurrent", m.cfg.Cron.MaxConcurrent, "timeout", m.cfg.Cron.ExecutionTimeout)
	return nil
}

// newThreadCronTool binds the Cron tool to one channel thread. ONE definition
// for both call sites (a regular turn here, a cron-triggered run in cron.go):
// the list scope and the target written on create must describe the same
// thread, or a job created in one place would be invisible in the other.
//
// The two ids are deliberately different. `ID` is the conversation the model is
// talking in and becomes CreatedBy (an audit trail); the TARGET is cronThread,
// which a channel with nested conversations lifts to the parent. SelfID stays the
// real thread: a job that will fire into the group's main area must not be
// described as "this conversation" to a model sitting in a topic.
func (m *Manager) newThreadCronTool(threadID string) *tools.CronTool {
	scope := m.cronScope(threadID)
	home := m.cronThread(threadID)
	return tools.NewCronTool(m.scheduler, tools.CronTarget{
		Type: cron.TargetTypeChannel,
		ID: func() string {
			return threadID
		},
		SelfID: func() string {
			return threadID
		},
		Matches: func(j *cron.Job) bool {
			return isChannelCronJob(j) && scope(j)
		},
		Prepare: func(j *cron.Job, mode string) error {
			j.TargetType = cron.TargetTypeChannel
			j.TargetThreadID = home
			j.CreatedBy = threadID
			return nil
		},
	})
}

// cronThread is the thread a cron job created from threadID belongs to.
//
// A channel whose threads nest (a topic inside a chat) implements
// channel.CronThreadChannel to lift it to the parent, so a job outlives the topic
// it was created in and stays manageable from the chat. Everything else — and any
// lookup that fails — keeps the thread as its own home.
func (m *Manager) cronThread(threadID string) string {
	if threadID == "" {
		return ""
	}
	ch := m.channelForThread(threadID)
	if ch == nil {
		return threadID
	}
	lifter, ok := ch.(channel.CronThreadChannel)
	if !ok {
		return threadID
	}
	if home := lifter.CronThread(threadID); home != "" {
		return home
	}
	return threadID
}

// cronScope reports whether a job is one this thread may see and operate.
//
// It matches the thread's cron home (see cronThread) and, so that jobs written
// before a channel lifted topics remain reachable, the thread itself. Both the
// slash command and the model-side tool use this one predicate, which is what
// keeps "not visible" and "does not exist" the same answer in both places.
func (m *Manager) cronScope(threadID string) func(*cron.Job) bool {
	if threadID == "" {
		return func(*cron.Job) bool { return true }
	}
	home := m.cronThread(threadID)
	return func(j *cron.Job) bool {
		return j.TargetThreadID == home || j.TargetThreadID == threadID
	}
}

// isChannelCronJob reports whether the job belongs to a channel conversation at
// all; a desktop job is out of scope for every channel command.
func isChannelCronJob(j *cron.Job) bool {
	return j != nil && (j.TargetType == "" || j.TargetType == cron.TargetTypeChannel)
}

// OnCronTrigger is the TriggerHandler callback invoked by the cron scheduler
// when a job fires. It simulates an incoming message from the cron system,
// reuses the per-thread cached AIAgent (so MCP discoveredSet, skills, etc.
// stay consistent with regular messages on the same thread), and delivers
// the response to the target thread's channel.
func (m *Manager) OnCronTrigger(ctx context.Context, job *cron.Job) error {
	m.logger.Info(ctx, "channel: cron trigger", "job", job.ID, "job_name", job.Name, "thread", job.TargetThreadID)

	resolved := m.getProviderForThread(job.TargetThreadID)

	ca, err := m.acquireAgent(ctx, job.TargetThreadID)
	if err != nil {
		return fmt.Errorf("cron: acquire agent: %w", err)
	}
	defer m.releaseAgent(ca)
	aiAgent := ca.agent

	// The CronTool is scoped to this run rather than registered on the cached
	// agent, so it can't leak into the next regular message turn on this
	// thread (its closure is bound to this job's target thread anyway).
	cronTool := m.newThreadCronTool(job.TargetThreadID)

	// Per-thread session.
	sm, diskHistory := m.prepareThreadSession(job.TargetThreadID, resolved)
	if sm != nil {
		aiAgent.SetSessionManager(sm)
	}

	// Use cached in-memory history when available; fall back to disk on first run.
	priorHistory := diskHistory
	if ca.history != nil {
		priorHistory = ca.history
	}

	// Bind the thread's working directory, matching runAgentTurn behavior.
	workDir := ca.WorkDir()
	if workDir != "" {
		ctx = wdctx.WithDir(ctx, workDir)
	}

	sid := ""
	if cur := aiAgent.SessionManager(); cur != nil {
		if s := cur.Current(); s != nil {
			sid = s.ID
		}
	}
	eventCh := aiAgent.RunConversationStream(ctx, priorHistory, m.buildCronPrompt(job), agent.BuildSystemPrompt(m.cfg.Language, workDir, sid, m.cfg.ExtraSystemPrompt), llm.ChatOptions{
		MaxTokens: resolved.MaxTokens,
	}, agent.WithExtraTools(cronTool))

	// sendProgress for cron: deliver intermediate tool results inline.
	sendProgress := func(text string) {
		m.sendToThread(ctx, job.TargetThreadID, text, fmt.Sprintf("cron_%s_%d", job.ID, time.Now().Unix()))
	}

	result, err := m.drainEvents(ctx, eventCh, aiAgent, sendProgress, nil, nil, nil)

	// Update cached history after cron turn.
	if msgs := aiAgent.GetLastMessages(); len(msgs) > 0 {
		ca.history = msgs
	}

	if err != nil {
		m.logger.Error(ctx, "channel: cron job drain error", err, "job", job.ID)
	}

	// Decide whether this run reaches the user. The judgment happens BEFORE the
	// error return: a run that failed after writing a partial report is
	// information the user asked to hear about, and the state carries the
	// failure for exactly that reason.
	decision := m.judgeNotify(ctx, job, result, sid, err)

	if !decision.Deliver {
		m.logger.Info(ctx, "channel: cron job suppressed",
			"job", job.ID, "decider", decision.Decider)
		return err
	}

	// Deliver the response to the target thread's channel.
	if decision.Content != "" {
		m.deliverCronResponse(ctx, channel.OutgoingMessage{
			ThreadID: job.TargetThreadID,
			Content:  decision.Content,
			ReplyTo:  fmt.Sprintf("cron_%s_%d", job.ID, time.Now().Unix()),
		})
	}

	return err
}

// deliverCronResponse sends a cron-triggered response to the channel
// responsible for the given ThreadID. It iterates all registered channels
// and tries each one that implements MessageSender.
func (m *Manager) deliverCronResponse(ctx context.Context, msg channel.OutgoingMessage) {
	m.mu.Lock()
	chans := make([]channel.Channel, len(m.channels))
	copy(chans, m.channels)
	m.mu.Unlock()

	for _, ch := range chans {
		sender, ok := ch.(channel.MessageSender)
		if !ok {
			continue
		}
		if err := sender.Send(ctx, msg); err != nil {
			m.logger.Error(ctx, "channel: cron send failed", err, "channel", ch.Name())
		} else {
			m.logger.Info(ctx, "channel: cron response delivered", "channel", ch.Name(), "thread", msg.ThreadID)
			return
		}
	}

	m.logger.Warn(ctx, "channel: cron response not delivered — no channel accepted thread", "thread", msg.ThreadID)
}

// The notification policy appended to a when_relevant job's prompt. Which one is
// used depends on who decides whether the result is delivered, because the
// instruction and the decider have to AGREE: an instruction that tells the agent
// to judge worth makes the agent drop findings before the decider ever sees
// them — invisibly, and with no way to recover them afterwards.
const (
	// markerNotificationPolicy asks the agent to make the call, because under the
	// marker nothing else does. It is shared with the desktop through the cron
	// package: the wording is unchanged from before the gate existed, so an
	// installation without a usable jev block behaves exactly as it did, and a
	// gate that has stopped answering falls back to the behaviour its fallback
	// can actually read.
	markerNotificationPolicy = cron.MarkerNotificationPolicy

	// gateNotificationPolicy asks the agent to REPORT and leaves the judgment to
	// the gate. The difference is not stylistic: a job whose own words say
	// "开市了就总结" loses its summary if the agent is invited to decide that a
	// flat market is not worth mentioning — which is exactly the case the gate
	// reads off the job's words and gets right. The marker survives here only for
	// the run that produced nothing at all, which is the one reading it was ever
	// right about.
	gateNotificationPolicy = "[Notification policy: when_relevant]\n" +
		"Report what this run produced, as it is. Do not judge whether it is worth the user's attention — that is decided " +
		"after you finish, from this report, so anything you leave out cannot be recovered. " +
		"Reply with ONLY the text `[SILENT]` when the run produced nothing at all (no data, no result, nothing to report " +
		"about the task itself). Otherwise say what happened, including when nothing changed: \"没有变化\" is a result, " +
		"and it is what the decision needs."
)

// buildCronPrompt constructs the effective prompt for a cron job execution: the
// job's own words, plus the notification policy the decider needs.
//
// The policy follows the DECIDER rather than the job, so it changes when the
// gate is present (and healthy) and when it is not. That also means it is part
// of what a running thread's history holds: flipping notify_matcher, or a
// judgment failing once, re-reads the prompt prefix from that point on the next
// cron run of that thread — a one-off cost per thread, not a recurring miss.
func (m *Manager) buildCronPrompt(job *cron.Job) string {
	if job.EffectiveNotify() != cron.NotifyWhenRelevant {
		return job.Prompt
	}
	if m.notifyGate != nil && !m.notifyGate.degraded.Load() {
		return job.Prompt + "\n\n" + gateNotificationPolicy
	}
	return job.Prompt + "\n\n" + markerNotificationPolicy
}
