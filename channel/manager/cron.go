package manager

import (
	"context"
	"fmt"
	"os"
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
// when a job fires. It simulates an incoming message from the cron system:
// builds an agent with the job's prompt as the user message, runs the agent
// turn, and delivers the response to the target thread's channel.
func (m *Manager) OnCronTrigger(ctx context.Context, job *cron.Job) error {
	m.logger.Log("channel: cron trigger job=%s (%s) thread=%s", job.ID, job.Name, job.TargetThreadID)

	prov, resolved := m.getProvider()
	if prov == nil || resolved == nil {
		return fmt.Errorf("channel: provider not initialized for cron trigger")
	}

	aiAgent := agent.NewAIAgent(prov, resolved.Provider.Model, 0)
	aiAgent.SetSkipEditConfirm(true)
	aiAgent.SetContextWindow(resolved.Provider.ContextWindow)
	aiAgent.SetupTitleProvider(m.cfg)
	aiAgent.SetupCommitProvider(m.cfg)

	mcpMgr, err := aiAgent.Configure(ctx, m.cfg)
	if err != nil {
		return fmt.Errorf("cron: configure agent: %w", err)
	}
	if mcpMgr != nil {
		defer mcpMgr.Close()
	}
	aiAgent.UnregisterTool(tools.ToolNameAskUser)

	// Register CronTool so cron jobs can manage themselves.
	aiAgent.RegisterTool(tools.NewCronTool(m.scheduler, func() string {
		return job.TargetThreadID
	}))

	// Load/create session for the target thread.
	sm, priorHistory, err := m.loadThreadSession(job.TargetThreadID)
	if err != nil {
		m.logger.Log("channel: cron session for %s: %v", job.TargetThreadID, err)
		sm = m.newSessionManager()
		priorHistory = nil
	}

	if sm != nil && !sm.HasCurrent() {
		wd, _ := os.Getwd()
		if _, err := sm.New(resolved.Provider.Type, resolved.Provider.Model, wd); err != nil {
			m.logger.Log("channel: cron create session: %v", err)
		} else {
			sm.SetThreadID(job.TargetThreadID)
		}
	}

	if sm != nil {
		aiAgent.SetSessionManager(sm)
	}

	eventCh := aiAgent.RunConversationStream(ctx, priorHistory, job.Prompt, m.systemPrompt, llm.ChatOptions{
		MaxTokens: resolved.MaxTokens,
	})

	isVerbose := func() bool {
		m.verboseMu.RLock()
		defer m.verboseMu.RUnlock()
		return m.verboseState != nil && m.verboseState[job.TargetThreadID]
	}

	// sendProgress for cron: deliver intermediate tool results inline.
	sendProgress := func(text string) {
		m.sendToThread(ctx, job.TargetThreadID, text, fmt.Sprintf("cron_%s_%d", job.ID, time.Now().Unix()))
	}

	result, err := m.drainEvents(eventCh, aiAgent, isVerbose, sendProgress, nil)
	if err != nil {
		m.logger.Log("channel: cron job %s drain error: %v", job.ID, err)
		return err
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
