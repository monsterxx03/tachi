package manager

import (
	"context"
	"fmt"
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/cron"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/channel"
)

// initCron creates the cron store and scheduler with the manager as the
// trigger handler. Must be called before Start() fires channels.
func (m *Manager) initCron(_ context.Context) error {
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
	m.logger.Log("channel: cron initialized (path=%s, max_concurrent=%d, timeout=%v)",
		storePath, m.cfg.Cron.MaxConcurrent, m.cfg.Cron.ExecutionTimeout)
	return nil
}

// OnCronTrigger is the TriggerHandler callback invoked by the cron scheduler
// when a job fires. It simulates an incoming message from the cron system,
// reuses the per-thread cached AIAgent (so MCP discoveredSet, skills, etc.
// stay consistent with regular messages on the same thread), and delivers
// the response to the target thread's channel.
func (m *Manager) OnCronTrigger(ctx context.Context, job *cron.Job) error {
	m.logger.Log("channel: cron trigger job=%s (%s) thread=%s", job.ID, job.Name, job.TargetThreadID)

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

	// Snapshot the registry so the cron-scoped tools we register below
	// (CronTool) don't leak into the next regular message turn.
	snap := aiAgent.SaveToolRegistry()
	defer aiAgent.RestoreToolRegistry(snap)

	// Register CronTool so cron jobs can manage themselves.
	aiAgent.RegisterTool(tools.NewCronTool(m.scheduler, func() string {
		return job.TargetThreadID
	}))

	// Per-thread session.
	sm, diskHistory := m.prepareThreadSession(job.TargetThreadID, resolved)
	if sm != nil {
		aiAgent.SetSessionManager(sm)
		// Notify memory backend when a new session was created
		aiAgent.StartSessionMemory()
	}

	// Use cached in-memory history when available; fall back to disk on first run.
	priorHistory := diskHistory
	if ca.history != nil {
		priorHistory = ca.history
	}

	eventCh := aiAgent.RunConversationStream(ctx, priorHistory, m.buildCronPrompt(job), agent.BuildSystemPrompt(m.cfg.Language, ""), llm.ChatOptions{
		MaxTokens: resolved.MaxTokens,
	})

	// sendProgress for cron: deliver intermediate tool results inline.
	sendProgress := func(text string) {
		m.sendToThread(ctx, job.TargetThreadID, text, fmt.Sprintf("cron_%s_%d", job.ID, time.Now().Unix()))
	}

	result, err := m.drainEvents(eventCh, aiAgent, sendProgress, nil)

	// Update cached history after cron turn.
	if msgs := aiAgent.GetLastMessages(); len(msgs) > 0 {
		ca.history = msgs
	}

	if err != nil {
		m.logger.Log("channel: cron job %s drain error: %v", job.ID, err)
		return err
	}

	// Check suppress policy: if the job uses when_relevant and the agent
	// determined there's nothing actionable, skip delivery.
	if job.ShouldSuppressResult(result) {
		m.logger.Log("channel: cron job %s suppressed (notify=when_relevant, agent replied SILENT)", job.ID)
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
			m.logger.Log("channel: cron send to %s failed: %v", ch.Name(), err)
		} else {
			m.logger.Log("channel: cron response delivered to %s (thread=%s)", ch.Name(), msg.ThreadID)
			return
		}
	}

	m.logger.Log("channel: cron response not delivered — no channel accepted thread %s", msg.ThreadID)
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
