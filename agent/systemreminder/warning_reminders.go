package systemreminder

import (
	"fmt"

	"github.com/monsterxx03/tachi/pkg/debuglog"
)

// IterationWarningReminder warns when the agent loop is running low on
// iterations so the model knows to finish its work efficiently.
// Threshold is the remaining-iteration count at or below which the warning fires.
type IterationWarningReminder struct {
	Threshold int
}

func (r IterationWarningReminder) Generate(ctx Context) []string {
	if ctx.MaxIterations <= 0 || ctx.IterationsLeft <= 0 {
		return nil
	}
	if r.Threshold <= 0 {
		return nil
	}
	if ctx.IterationsLeft > r.Threshold {
		return nil
	}
	line := fmt.Sprintf(
		"Iteration budget: %d of %d iterations remaining. Complete your work as efficiently as possible.",
		ctx.IterationsLeft, ctx.MaxIterations,
	)
	debuglog.DefaultLogger.Log("systemreminder: IterationWarningReminder firing (threshold=%d): %q", r.Threshold, line)
	return []string{line}
}

// TokenWarningReminder warns when the input token count exceeds a percentage
// of the context window, so the model can adjust its output verbosity.
// ThresholdPct is the usage percentage at or above which the warning fires.
type TokenWarningReminder struct {
	ThresholdPct int
}

func (r TokenWarningReminder) Generate(ctx Context) []string {
	if ctx.ContextWindow <= 0 || ctx.InputTokens <= 0 {
		return nil
	}
	if r.ThresholdPct <= 0 {
		return nil
	}
	pct := float64(ctx.InputTokens) / float64(ctx.ContextWindow) * 100
	if pct < float64(r.ThresholdPct) {
		return nil
	}
	line := fmt.Sprintf(
		"Context window usage: %.0f%% (%d / %d input tokens). Be concise and minimize unnecessary output.",
		pct, ctx.InputTokens, ctx.ContextWindow,
	)
	debuglog.DefaultLogger.Log("systemreminder: TokenWarningReminder firing (threshold=%d%%): %q", r.ThresholdPct, line)
	return []string{line}
}
