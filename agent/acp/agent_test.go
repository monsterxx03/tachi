package acp

import (
	"context"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/monsterxx03/tachi/config"
)

func TestListSessions_FilterByCwd(t *testing.T) {
	cfg := config.DefaultConfig()
	ta := NewTachiAgent(cfg, "test")

	// Add sessions with different cwds (in-memory only — no disk scan needed)
	ta.sessions.New(context.Background(), "/home/user/project-a", "openai", nil, nil, nil, nil)
	ta.sessions.New(context.Background(), "/home/user/project-b", "openai", nil, nil, nil, nil)
	ta.sessions.New(context.Background(), "/home/user/project-a", "anthropic", nil, nil, nil, nil)

	// Filter by cwd — only checks in-memory sessions first, then disk.
	// Since we can't control disk sessions in unit tests, just verify in-memory filtering works.
	cwd := "/home/user/project-a"
	resp, err := ta.ListSessions(context.Background(), acp.ListSessionsRequest{Cwd: &cwd})
	require.NoError(t, err)
	// Should have at least 2 from in-memory
	found := 0
	for _, s := range resp.Sessions {
		if s.Cwd == cwd {
			found++
		}
	}
	assert.GreaterOrEqual(t, found, 2)

	// Verify all returned sessions with project-a cwd match
	for _, s := range resp.Sessions {
		assert.Equal(t, cwd, s.Cwd)
	}
}
