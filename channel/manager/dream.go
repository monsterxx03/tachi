package manager

import (
	"context"
	"time"

	"github.com/monsterxx03/tachi/dream"
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
		MinInterval: m.cfg.Dream.MinInterval,
		Logger:      m.logger,
	})

	return o.Run(ctx, sessions, m.runDreamSubAgent)
}

// runDreamSubAgent executes the dream pipeline for a single domain.
// TODO: implement full Orient→Gather→Consolidate→Prune sub-agent pipeline.
func (m *Manager) runDreamSubAgent(ctx context.Context, plan dream.Plan) (dream.State, error) {
	m.logger.Log("dream [%s:%s]: starting sub-agent (memory_root=%s, active_sessions=%d)",
		plan.Group.Domain, plan.Group.Root, plan.Group.MemoryRoot, len(plan.ActiveSessions))

	// TODO: Build dream prompt, start sub-agent with PathPolicy sandbox.
	// For now, return a minimal state marking this run as completed.
	state := dream.State{
		LastDreamAt:     time.Now(),
		SessionsDreamed: len(plan.ActiveSessions),
	}

	m.logger.Log("dream [%s:%s]: completed (placeholder — sub-agent pipeline not yet wired)",
		plan.Group.Domain, plan.Group.Root)
	return state, nil
}
