package agent

import (
	"context"

	agenttools "github.com/monsterxx03/tachi/agent/tools"
)

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

func (s *stubTool) Name() string                                 { return s.name }
func (s *stubTool) Description() string                          { return s.desc }
func (s *stubTool) Properties() map[string]agenttools.PropertySchema { return s.props }
func (s *stubTool) Required() []string                           { return s.required }
func (s *stubTool) Parallel() bool                               { return s.parallel }
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
