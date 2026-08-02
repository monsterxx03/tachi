package agent

import (
	"context"
	"errors"
	"fmt"
	"os"

	cmds "github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/session"
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
// (channel / ACP). On success the report is registered as a session artifact
// and the artifact ref is returned — frontends with an in-memory history
// cache (channel's ca.history) splice the reminder block into it, since a
// disk-only write is invisible to the cached history. artifact is nil when
// there is nothing to register (no session, or the report file is missing).
//
// The TUI does not use this — its research is event-driven.
func (a *AIAgent) RunDeepResearch(ctx context.Context, cfg *config.Config, args string, onStart func(topic string, depth, breadth int), emit func(string)) (report string, artifact *session.ArtifactRef, err error) {
	parsed := cmds.ParseResearchArgs(args)
	if parsed.Topic == "" {
		return "", nil, ErrResearchUsage
	}
	if parsed.Depth <= 0 {
		parsed.Depth = cfg.DeepResearch.DefaultDepth
	}
	if parsed.Breadth <= 0 {
		parsed.Breadth = cfg.DeepResearch.DefaultBreadth
	}

	engine, err := a.NewDeepResearch(cfg)
	if err != nil {
		return "", nil, fmt.Errorf("create research engine: %w", err)
	}
	if engine == nil {
		return "", nil, ErrResearchUnavailable
	}

	onStart(parsed.Topic, parsed.Depth, parsed.Breadth)

	researchCtx, cancel := context.WithTimeout(ctx, cfg.DeepResearch.Timeout)
	defer cancel()

	report, err = engine.Run(researchCtx, parsed.Topic, parsed.Depth, parsed.Breadth, func(format string, args ...any) {
		emit(fmt.Sprintf(format, args...))
	})
	if err != nil {
		return "", nil, err
	}

	// Register the report as a session artifact (best-effort — a failure
	// must not fail the research). Only when the file exists on disk; a
	// missing session manager or report is logged, not silently skipped.
	if sm := a.SessionManager(); sm != nil {
		if p := engine.ReportPath(); p != "" {
			if _, statErr := os.Stat(p); statErr != nil {
				a.Config.Logger.Warn(ctx, "research: report missing on disk, artifact not registered", "path", p, "err", statErr)
				return report, nil, nil
			}
			ref := &session.ArtifactRef{
				Kind:  session.ArtifactKindResearch,
				Title: parsed.Topic,
				Path:  p,
			}
			if err := sm.AppendArtifact(*ref); err != nil {
				a.Config.Logger.Error(ctx, "research: failed to register artifact", err)
				return report, nil, nil
			}
			return report, ref, nil
		}
	} else {
		a.Config.Logger.Warn(ctx, "research: no session manager, artifact not registered")
	}
	return report, nil, nil
}
