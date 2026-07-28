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
	})

	m.scheduler = scheduler
	m.logger.Info(ctx, "channel: cron initialized", "path", storePath, "max_concurrent", m.cfg.Cron.MaxConcurrent, "timeout", m.cfg.Cron.ExecutionTimeout)
	return nil
}

// OnCronTrigger is the TriggerHandler callback invoked by the cron scheduler
// when a job fires. It simulates an incoming message from the cron system,
// reuses the per-thread cached AIAgent (so MCP discoveredSet, skills, etc.
// stay consistent with regular messages on the same thread), and delivers
// the response to the target thread's channel.
func (m *Manager) OnCronTrigger(ctx context.Context, job *cron.Job) error {
	m.logger.Info(ctx, "channel: cron trigger", "job", job.ID, "job_name", job.Name, "thread", job.TargetThreadID)

	_, resolved, _ := m.getProviderForThread(job.TargetThreadID)
	if resolved == nil {
		return fmt.Errorf("channel: provider not initialized for cron trigger")
	}

	ca, err := m.acquireAgent(ctx, job.TargetThreadID)
	if err != nil {
		return fmt.Errorf("cron: acquire agent: %w", err)
	}
	defer m.releaseAgent(ca)
	aiAgent := ca.agent

	// The CronTool is scoped to this run rather than registered on the cached
	// agent, so it can't leak into the next regular message turn on this
	// thread (its closure is bound to this job's target thread anyway).
	cronTool := tools.NewCronTool(m.scheduler, func() string {
		return job.TargetThreadID
	})

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
	if ca.workDir != "" {
		ctx = wdctx.WithDir(ctx, ca.workDir)
	}

	sid := ""
	if cur := aiAgent.SessionManager(); cur != nil {
		if s := cur.Current(); s != nil {
			sid = s.ID
		}
	}
	eventCh := aiAgent.RunConversationStream(ctx, priorHistory, m.buildCronPrompt(job), agent.BuildSystemPrompt(m.cfg.Language, ca.workDir, sid, m.cfg.Debug.PPROF), llm.ChatOptions{
		MaxTokens: resolved.MaxTokens,
	}, agent.WithExtraTools(cronTool))

	// sendProgress for cron: deliver intermediate tool results inline.
	sendProgress := func(text string) {
		m.sendToThread(ctx, job.TargetThreadID, text, fmt.Sprintf("cron_%s_%d", job.ID, time.Now().Unix()))
	}

	result, err := m.drainEvents(ctx, eventCh, aiAgent, sendProgress, nil, nil)

	// Update cached history after cron turn.
	if msgs := aiAgent.GetLastMessages(); len(msgs) > 0 {
		ca.history = msgs
	}

	if err != nil {
		m.logger.Error(ctx, "channel: cron job drain error", err, "job", job.ID)
		return err
	}

	// Check suppress policy: if the job uses when_relevant and the agent
	// determined there's nothing actionable, skip delivery.
	if job.ShouldSuppressResult(result) {
		m.logger.Info(ctx, "channel: cron job suppressed (notify=when_relevant)", "job", job.ID)
		return nil
	}

	// Deliver the response to the target thread's channel.
	if result != "" {
		m.deliverCronResponse(ctx, channel.OutgoingMessage{
			ThreadID: job.TargetThreadID,
			Content:  result,
			ReplyTo:  fmt.Sprintf("cron_%s_%d", job.ID, time.Now().Unix()),
		})
	}

	return nil
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

// buildCronPrompt constructs the effective prompt for a cron job execution.
// If the job uses notify=when_relevant, a suppression instruction is appended
// so the agent can reply with [SILENT] when there's nothing meaningful.
func (m *Manager) buildCronPrompt(job *cron.Job) string {
	if job.EffectiveNotify() != cron.NotifyWhenRelevant {
		return job.Prompt
	}

	return job.Prompt + "\n\n" +
		"[Notification policy: when_relevant]\n" +
		"After completing the task, evaluate whether the result contains meaningful or actionable information for the user. " +
		"If there is nothing new, no changes detected, or nothing worth reporting, respond with ONLY the text `[SILENT]` — " +
		"do not include any other content. The notification will be suppressed and the user will not be disturbed. " +
		"Only send actual content when there IS something noteworthy to report."
}
