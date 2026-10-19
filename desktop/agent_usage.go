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
		credit += row.CreditValue(llm.ResolveCreditRate(d.config(), row.Provider, row.Model))
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

// trayTodayCost sums today's ledger rows across EVERY session — the menu bar's spend figure.
//
// The ledger is one file per calendar day (llm.UsageRecorder.fileFor), so "today" is exactly the
// file RowsAll picks up from the start of today: no timestamp filtering needed, and no scan of
// older days. Takes the recorder and the clock as arguments so a test can point it at a temp dir.
func trayTodayCost(rec *llm.UsageRecorder, now time.Time) float64 {
	if rec == nil {
		return 0
	}
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	rows, err := rec.RowsAll(start)
	if err != nil {
		return 0 // best-effort: the menu bar shows nothing rather than a wrong number
	}
	total := 0.0
	for i := range rows {
		total += rows[i].Cost()
	}
	return total
}

// trayCostTTL bounds how often the menu bar re-reads the ledger. refreshTray runs on every state
// change (a turn moves through several), while today's file can hold thousands of rows by evening —
// and ten seconds is far below what a person can notice on a spend figure.
const trayCostTTL = 10 * time.Second

// trayCostCache is todayCost's memo. day is part of it so a midnight rollover invalidates the
// entry even while the TTL is still running.
type trayCostCache struct {
	amount float64
	at     time.Time
	day    string
}

// todayCost is the memoized trayTodayCost for the menu bar.
func (d *desktopApp) todayCost() float64 {
	now := time.Now()
	day := now.Format("2006-01-02")

	d.mu.Lock()
	cached := d.trayCost
	d.mu.Unlock()
	if cached.day == day && now.Sub(cached.at) < trayCostTTL {
		return cached.amount
	}

	amount := trayTodayCost(agent.GlobalUsageRecorder(), now)

	d.mu.Lock()
	d.trayCost = trayCostCache{amount: amount, at: now, day: day}
	d.mu.Unlock()
	return amount
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
	last := r.tpsLast
	d.mu.Unlock()
	if d.app != nil && tps > 0 {
		d.app.Event.Emit("agent:tps", map[string]any{"sessionId": id, "tps": tps})
	}
	if tps > 0 {
		// The phone's strip dims to lastTps once the rate stops moving; it needs both numbers.
		d.mobileNotify(id, map[string]any{"type": mobileEvTps, "tps": tps, "lastTps": last})
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
	if last > 0 {
		d.mobileNotify(id, map[string]any{"type": mobileEvTps, "tps": 0, "lastTps": last})
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

// estimateFromMessages returns the context size to report for a session with no call in this
// process: the most recent call's RECORDED prompt size (`llm.PromptTokens` over the persisted usage
// counts) — the real number, since nothing has been appended since that call — falling back to
// `usage.estimated_input_tokens` (the chars/4 heuristic) for records too old to carry the counts,
// or 0 when the session has never carried either. Mirrors TUI's session-restore recovery
// (tui/session_selector.go).
func (d *desktopApp) estimateFromMessages(r *sessionRun) int64 {
	if r == nil || r.sm == nil {
		return 0
	}
	msgs, err := r.sm.LoadMessages()
	if err != nil {
		return 0
	}
	providerType := ""
	if r.agent != nil && r.agent.Provider() != nil {
		providerType = r.agent.Provider().Name()
	}
	var est, real int64
	for _, m := range msgs {
		u := m.Usage
		if u == nil || u.EstimatedInputTokens <= 0 {
			continue
		}
		est = u.EstimatedInputTokens
		// The same call's REAL prompt size, in the provider's own terms (llm.PromptTokens: the
		// cache counts are inside the input for OpenAI-family, beside it for Anthropic).
		//
		// A session restored from disk has had nothing appended since that call, so this IS the
		// context size right now — and reporting the character estimate instead would under-report
		// mixed content by ~18% until this process makes a call of its own (which then anchors it
		// in convState). `est` stays as the fallback for records old enough to lack the counts.
		real = llm.PromptTokens(&llm.Usage{
			InputTokens:              u.InputTokens,
			CacheReadInputTokens:     u.CacheReadInputTokens,
			CacheCreationInputTokens: u.CacheCreationInputTokens,
		}, providerType)
	}
	if real > 0 {
		return real
	}
	return est
}
