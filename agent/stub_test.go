package agent

import (
	"context"

	agenttools "github.com/monsterxx03/tachi/agent/tools"
)

// testAgentOpt configures the AIAgent returned by newTestAgent.
type testAgentOpt func(*AIAgent)

// withFakeSession creates an in-memory fake session manager and sets it on
// the agent. No file I/O, no temp directories. When appendErr is non-nil,
// every AppendMessage call returns that error — use this to test session
// write failure paths.
func withFakeSession(appendErr ...error) testAgentOpt {
	return withFakeSessionIn("", appendErr...)
}

// withFakeSessionIn is withFakeSession with the directory the fake's sessions live in.
//
// It is how a test that exercises checkpoints gets them at all: the checkpoint store records
// its manifest BESIDE a session, so an agent whose manager names no directory has nowhere to
// put one and records nothing (see sessionBaseDir). The directory is a real one — the fake
// keeps its records in memory either way, but the snapshots are files.
func withFakeSessionIn(dir string, appendErr ...error) testAgentOpt {
	return func(a *AIAgent) {
		fake := &fakeSessionManager{baseDir: dir}
		if len(appendErr) > 0 && appendErr[0] != nil {
			fake.appendErr = appendErr[0]
		}
		a.SetSessionManager(fake)
	}
}

// withCompactStrategy sets a custom CompactStrategy, allowing tests to
// inject a fake that returns a fixed summary without an LLM provider.
func withCompactStrategy(s CompactStrategy) testAgentOpt {
	return func(a *AIAgent) {
		a.SetCompactStrategy(s)
	}
}

// stubTool is a reusable test stub for the tools.Tool interface.
// Only override the fields you need; all methods have sensible zero-value defaults.
type stubTool struct {
	name         string
	desc         string
	props        map[string]agenttools.PropertySchema
	required     []string
	parallel     bool
	executeFn    func(ctx context.Context, args string) (string, error)
	needsConfirm bool
	diffFn       func(ctx context.Context, args string) (string, error)
}

func (s *stubTool) Name() string                                     { return s.name }
func (s *stubTool) Description() string                              { return s.desc }
func (s *stubTool) Properties() map[string]agenttools.PropertySchema { return s.props }
func (s *stubTool) Required() []string                               { return s.required }
func (s *stubTool) Parallel() bool                                   { return s.parallel }
func (s *stubTool) ExecuteContext(ctx context.Context, args string) (string, error) {
	if s.executeFn != nil {
		return s.executeFn(ctx, args)
	}
	return "", nil
}

func (s *stubTool) NeedsConfirmation() bool { return s.needsConfirm }
func (s *stubTool) GetDiff(ctx context.Context, args string) (string, error) {
	if s.diffFn != nil {
		return s.diffFn(ctx, args)
	}
	return "", nil
}
