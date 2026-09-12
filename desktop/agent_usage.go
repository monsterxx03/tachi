package main

import (
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/llm"
)

// rebuildCostCredit recomputes the session's cumulative cost (CNY) and ledger
// credit ("积分") from the usage ledger — the same aggregation the TUI /usage
// report uses (row.Cost + row.CreditValue(rate)). Rebuilt on session switch
// and after each usage event; O(rows), and rows is small per session.
// Callers must NOT hold d.mu.
func (d *desktopApp) rebuildCostCredit(r *sessionRun) {
	if r == nil || r.agent == nil || r.sm == nil || !r.sm.HasCurrent() {
		return
	}
	rec := r.agent.UsageRecorder()
	if rec == nil {
		return
	}
	curr := r.sm.Current()
	rows, err := rec.Rows(curr.ID, curr.CreatedAt)
	if err != nil {
		return
	}
	var cost, credit float64
	var lastInTok, lastCrTok int64
	var have bool
	for _, row := range rows {
		cost += row.Cost()
		credit += row.CreditValue(llm.ResolveCreditRate(d.cfg, row.Provider, row.Model))
		lastInTok = row.InputTokens
		lastCrTok = row.CacheReadInputTokens
		have = true
	}
	var rate float64
	hasCacheHit := false
	// "Recent" cache-hit rate = the LAST call's cache-read / (cache-miss + cache-read).
	// Cache-creation tokens (writing new cache entries) are excluded. Ledger rows are
	// appended in call order, so the last row is the most recent API call.
	if have {
		if total := lastInTok + lastCrTok; total > 0 {
			rate = float64(lastCrTok) / float64(total)
			hasCacheHit = true
		}
	}
	d.mu.Lock()
	r.cost = cost
	r.credit = credit
	r.cacheHitRate = rate
	r.hasCacheHit = hasCacheHit
	d.mu.Unlock()
}

// GetSessionUsage returns the session's cumulative cost (CNY) and ledger
// credit ("积分") for the status bar. Both are 0 when no usage is recorded.
func (s *AgentService) GetSessionUsage(id string) map[string]any {
	d := s.desk
	d.mu.Lock()
	r := d.getRun(id)
	d.mu.Unlock()
	d.rebuildCostCredit(r)
	d.mu.Lock()
	cost, credit := r.cost, r.credit
	rate := r.cacheHitRate
	hasCacheHit := r.hasCacheHit
	d.mu.Unlock()
	return map[string]any{"cost": cost, "credit": credit, "cacheHitRate": rate, "hasCacheHit": hasCacheHit}
}

// tpsAdd accumulates the estimated output-token count for a live tokens/sec
// display and pushes the rate to the frontend ("agent:tps"). It mirrors TUI
// StatusBar's AddStreamedOutput + currentTPS semantics (text + thinking
// deltas, guarded against sub-second spikes).
func (d *desktopApp) tpsAdd(id, text string) {
	if text == "" {
		return
	}
	d.mu.Lock()
	r := d.getRun(id)
	if r.tpsStart.IsZero() {
		r.tpsStart = time.Now()
	}
	r.tpsTokens += agent.EstimateTokenCount(text)
	tps := tpsRate(r.tpsTokens, r.tpsStart)
	d.mu.Unlock()
	if d.app != nil && tps > 0 {
		d.app.Event.Emit("agent:tps", map[string]any{"sessionId": id, "tps": tps})
	}
}

// tpsReset freezes the current live rate into tpsLast (for a dimmed "paused"
// readout) and clears the timing segment. Called at tool boundaries / turn end —
// same as TUI StatusBar.ResetTPS.
func (d *desktopApp) tpsReset(id string) {
	d.mu.Lock()
	r := d.getRun(id)
	if tps := tpsRate(r.tpsTokens, r.tpsStart); tps > 0 {
		r.tpsLast = tps
	}
	r.tpsTokens = 0
	r.tpsStart = time.Time{}
	last := r.tpsLast
	d.mu.Unlock()
	if d.app != nil && last > 0 {
		d.app.Event.Emit("agent:tps", map[string]any{"sessionId": id, "tps": 0, "lastTps": last})
	}
}

// tpsRate returns tokens/sec for a segment, or 0 before a full second elapses
// (avoids wild spikes from the first instants) — the same guard as TUI's
// currentTPS.
func tpsRate(tokens int64, start time.Time) int64 {
	if tokens <= 0 || start.IsZero() {
		return 0
	}
	elapsed := time.Since(start)
	if elapsed < time.Second {
		return 0
	}
	return int64(float64(tokens) / elapsed.Seconds())
}

// estimateFromMessages returns the most recent persisted input-token estimate
// (usage.estimated_input_tokens on a session message, chars/4 heuristic), or 0
// when the session has never carried an estimate. Mirrors TUI's session-restore
// recovery (tui/session_selector.go).
func (d *desktopApp) estimateFromMessages(r *sessionRun) int64 {
	if r == nil || r.sm == nil {
		return 0
	}
	msgs, err := r.sm.LoadMessages()
	if err != nil {
		return 0
	}
	var est int64
	for _, m := range msgs {
		if m.Usage != nil && m.Usage.EstimatedInputTokens > 0 {
			est = m.Usage.EstimatedInputTokens
		}
	}
	return est
}
