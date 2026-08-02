package agent

import (
	"context"
	"errors"
	"fmt"

	cmds "github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/config"
)

// Research errors surfaced to frontends by RunDeepResearch.
var (
	// ErrResearchUsage is returned when the topic argument is missing —
	// frontends render their own usage line.
	ErrResearchUsage = errors.New("research topic required")
	// ErrResearchUnavailable is returned when the engine could not be built.
	ErrResearchUnavailable = errors.New("deep research engine unavailable")
)

// RunDeepResearch executes /research end-to-end for a synchronous frontend
// (channel / ACP): parse args (applying config defaults), build the engine,
// invoke onStart so the frontend can show its "research started" banner,
// then run with the configured research timeout, streaming progress via emit.
//
// The TUI does not use this — its research is event-driven (goroutine +
// channel into the chatview) with a slightly longer timeout.
func (a *AIAgent) RunDeepResearch(ctx context.Context, cfg *config.Config, args string, onStart func(topic string, depth, breadth int), emit func(string)) (string, error) {
	parsed := cmds.ParseResearchArgs(args)
	if parsed.Topic == "" {
		return "", ErrResearchUsage
	}
	if parsed.Depth <= 0 {
		parsed.Depth = cfg.DeepResearch.DefaultDepth
	}
	if parsed.Breadth <= 0 {
		parsed.Breadth = cfg.DeepResearch.DefaultBreadth
	}

	engine, err := a.NewDeepResearch(cfg)
	if err != nil {
		return "", fmt.Errorf("create research engine: %w", err)
	}
	if engine == nil {
		return "", ErrResearchUnavailable
	}

	onStart(parsed.Topic, parsed.Depth, parsed.Breadth)

	researchCtx, cancel := context.WithTimeout(ctx, cfg.DeepResearch.Timeout)
	defer cancel()

	return engine.Run(researchCtx, parsed.Topic, parsed.Depth, parsed.Breadth, func(format string, args ...any) {
		emit(fmt.Sprintf(format, args...))
	})
}
