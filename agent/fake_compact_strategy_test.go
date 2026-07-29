package agent

import (
	"context"

	"github.com/monsterxx03/tachi/llm"
)

// fakeCompactStrategy returns a fixed summary (or error) for every Compact call.
// Tests use this to verify auto-compact behaviour without an LLM provider.
type fakeCompactStrategy struct {
	summary string
	err     error
}

func (s *fakeCompactStrategy) Compact(_ context.Context, _ []llm.Message, _ int) (string, error) {
	return s.summary, s.err
}
