package acp

import (
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
)

func TestMapToolKind(t *testing.T) {
	assert.Equal(t, acp.ToolKindRead, mapToolKind("ReadFile"))
	assert.Equal(t, acp.ToolKindEdit, mapToolKind("WriteFile"))
	assert.Equal(t, acp.ToolKindEdit, mapToolKind("EditFile"))
	assert.Equal(t, acp.ToolKindExecute, mapToolKind("Bash"))
	assert.Equal(t, acp.ToolKindSearch, mapToolKind("Glob"))
	assert.Equal(t, acp.ToolKindSearch, mapToolKind("Grep"))
	assert.Equal(t, acp.ToolKindFetch, mapToolKind("WebSearch"))
	assert.Equal(t, acp.ToolKindFetch, mapToolKind("WebFetch"))
	assert.Equal(t, acp.ToolKind(""), mapToolKind("UnknownTool"))
}

func TestMapStopReason(t *testing.T) {
	assert.Equal(t, acp.StopReasonEndTurn, mapStopReason("stop"))
	assert.Equal(t, acp.StopReasonCancelled, mapStopReason("cancelled"))
	assert.Equal(t, acp.StopReasonCancelled, mapStopReason("interrupted"))
	assert.Equal(t, acp.StopReasonEndTurn, mapStopReason("error"))
	assert.Equal(t, acp.StopReasonEndTurn, mapStopReason("budget_exhausted"))
}
