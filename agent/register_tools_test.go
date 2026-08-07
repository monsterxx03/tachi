package agent

import (
	"testing"

	agenttools "github.com/monsterxx03/tachi/agent/tools"
	"github.com/stretchr/testify/assert"
)

// SavePlan is only registered when EnablePlanTool is called (ACP sessions).
// TUI/channel agents must not see it — they have no plan card UI.
func TestRegisterTools_SavePlanGating(t *testing.T) {
	t.Run("not registered by default", func(t *testing.T) {
		a := newBareTestAgent(t, nil, 50)
		a.RegisterTools()
		assert.Nil(t, a.GetTool(agenttools.ToolNameSavePlan))
	})

	t.Run("registered after EnablePlanTool", func(t *testing.T) {
		a := newBareTestAgent(t, nil, 50)
		a.EnablePlanTool()
		a.RegisterTools()
		assert.NotNil(t, a.GetTool(agenttools.ToolNameSavePlan))
	})
}
