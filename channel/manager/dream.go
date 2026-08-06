package manager

import (
	"context"

	"github.com/monsterxx03/tachi/dream"
	"github.com/monsterxx03/tachi/session"
)

// executeDream is the SystemScheduler handler for AutoDream.
// It delegates to the dream.Orchestrator which owns all gate/lock/grouping logic.
func (m *Manager) executeDream(ctx context.Context) error {
	sm := m.newSessionManager()
	sessions, err := sm.List()
	if err != nil {
		return err
	}

	o := dream.NewOrchestrator(dream.Config{
		Logger:        m.logger,
		MaxConcurrent: m.cfg.Dream.MaxConcurrent,
	})

	return o.Run(ctx, sessions, m.runDreamForPlan)
}

// runDreamForPlan executes the dream pipeline for a single domain.
func (m *Manager) runDreamForPlan(ctx context.Context, plan dream.Plan) (dream.State, error) {
	provider := m.defaultResolvedProvider.Provider

	return dream.RunDream(ctx, plan, dream.RunConfig{
		FallbackProvider: provider,
		DreamProvider:    m.cfg.Dream.Provider,
		Config:           m.cfg,
		MaxIter:          m.cfg.Dream.SubagentMaxIter,
		MaxTokens:        m.cfg.MaxTokens,
		MaxMessageChars:  m.cfg.Dream.MaxMessageChars,
		Logger:           m.logger,
	}, m.buildMessageLoader())
}

// buildMessageLoader returns a function that loads messages for a given session ID.
func (m *Manager) buildMessageLoader() func(string) ([]session.Message, error) {
	sm := m.newSessionManager()
	return func(id string) ([]session.Message, error) {
		if _, err := sm.Load(id); err != nil {
			return nil, err
		}
		return sm.LoadMessages()
	}
}
