package agent

import (
	"sync"
	"time"

	"github.com/monsterxx03/tachi/agent/tokenbreakdown"
	"github.com/monsterxx03/tachi/llm"
)

// turnState holds the per-turn mutable state of an AIAgent.
//
// Why this exists: an AIAgent can outlive many turns and be shared across
// goroutines. In channel mode the same agent is cached per thread
// (channel/manager/agent_cache.go) and the turn goroutine writes these fields
// while slash-command handlers read them — handleUsageCommand reaches the
// agent through getAgentEstimate / getAgentBreakdown, which only hold
// agentCacheMu long enough to look up the map and then read agent fields
// without the cachedAgent lock. That was a genuine data race on both
// inputTokens and breakdown (the latter a struct, so readable half-updated).
//
// Every field here is guarded by mu. Long-lived configuration and shared
// resources stay on AIAgent — only state that resets or advances per turn
// belongs in this struct.
type turnState struct {
	mu sync.RWMutex

	// inputTokens is the local (conservative) token estimate for the most
	// recent API call, computed by EstimateAndUpdateTokens.
	inputTokens int64
	// breakdown categorises inputTokens; set alongside it.
	breakdown tokenbreakdown.Breakdown

	// messages is the final LLM message slice after a Run*Stream call
	// completes: history + current user + assistant + tool results.
	messages []llm.Message

	// start is the wall-clock time the current turn's agent loop began;
	// finish handlers use it to compute RunResult.Duration.
	start time.Time
	// traceID is the trace ID for the current turn, used for log correlation.
	traceID string

	// pendingImages holds image content parts to attach to the next user
	// message. Set via SetPendingImages, consumed by the agent loop.
	pendingImages []llm.ContentPart

	// compactEstimate is the token estimate at the time of the most recent
	// auto-compact, driving shouldAutoCompact's cooldown logic.
	compactEstimate int64

	// lastMessageDate is the calendar date (2006-01-02) of the last processed
	// user message; empty initially.
	lastMessageDate string
}

// newTurnState creates a zero-valued turn state. AIAgent is always constructed
// through NewAIAgent, which initialises the turn field; a.turn is therefore
// never nil in production code. Tests that build AIAgent via a struct literal
// must set turn: newTurnState() explicitly — deliberately not lazily
// initialised, since a lazy getter would itself race under concurrent access,
// which is exactly what this type exists to prevent.
func newTurnState() *turnState { return &turnState{} }

// begin records the start time and trace ID for a new turn.
func (s *turnState) begin(traceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.start = time.Now()
	s.traceID = traceID
}

func (s *turnState) startTime() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.start
}

// elapsed returns the duration since the current turn began.
func (s *turnState) elapsed() time.Duration {
	return time.Since(s.startTime())
}

func (s *turnState) trace() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.traceID
}

// setEstimate records a new token estimate and its breakdown together, so
// readers never observe one without the other.
func (s *turnState) setEstimate(total int64, tb tokenbreakdown.Breakdown) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inputTokens = total
	s.breakdown = tb
}

// setTokens sets only the token estimate, leaving the breakdown untouched.
// Used when resuming a session from stored usage (no breakdown available)
// and when restoring the estimate after a one-off run.
func (s *turnState) setTokens(total int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inputTokens = total
}

func (s *turnState) tokens() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.inputTokens
}

// snapshotBreakdown returns a copy of the current breakdown.
func (s *turnState) snapshotBreakdown() tokenbreakdown.Breakdown {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.breakdown
}

// estimateSnapshot returns the token total and its breakdown read under a
// single lock.
//
// Reading tokens() and snapshotBreakdown() separately is memory-safe but
// logically tearable: a concurrent turn can update both between the two calls,
// leaving the caller with a total from one estimate and a breakdown from
// another. Callers that report both together (e.g. /usage) must use this.
func (s *turnState) estimateSnapshot() (int64, tokenbreakdown.Breakdown) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.inputTokens, s.breakdown
}

// setMessages stores the turn's final message slice.
func (s *turnState) setMessages(msgs []llm.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = msgs
}

// snapshotMessages returns a shallow copy of the stored message slice, so a
// concurrent turn appending to its own slice cannot mutate what the caller
// observes. The llm.Message values are shared; they are treated as immutable
// once recorded.
func (s *turnState) snapshotMessages() []llm.Message {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.messages == nil {
		return nil
	}
	out := make([]llm.Message, len(s.messages))
	copy(out, s.messages)
	return out
}

// takePendingImages returns and clears any pending image parts.
func (s *turnState) takePendingImages() []llm.ContentPart {
	s.mu.Lock()
	defer s.mu.Unlock()
	imgs := s.pendingImages
	s.pendingImages = nil
	return imgs
}

func (s *turnState) setPendingImages(imgs []llm.ContentPart) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pendingImages = imgs
}

// setCompactEstimate records the token estimate at compaction time.
func (s *turnState) setCompactEstimate(v int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.compactEstimate = v
}

// compactCooldown reports whether the estimate has grown less than 20% since
// the last compaction. Both values are read under one lock so the ratio is
// computed from a consistent pair.
func (s *turnState) compactCooldown() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.compactEstimate == 0 {
		return false // never compacted
	}
	return float64(s.inputTokens)/float64(s.compactEstimate) < 1.2
}

func (s *turnState) messageDate() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastMessageDate
}

func (s *turnState) setMessageDate(d string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastMessageDate = d
}
