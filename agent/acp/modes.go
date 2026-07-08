package acp

import (
	acp "github.com/coder/acp-go-sdk"

	"github.com/monsterxx03/tachi/agent"
)

// buildModeState returns the ACP SessionModeState for Tachi's supported modes.
func buildModeState(currentMode string) *acp.SessionModeState {
	autoDesc := "Full tool access — edit files, run commands, browse the web, use MCP servers, and more"
	chatDesc := "Read-only conversation — search code, browse the web, ask questions, use skills and MCP tools"
	return &acp.SessionModeState{
		CurrentModeId: acp.SessionModeId(currentMode),
		AvailableModes: []acp.SessionMode{
			{
				Id:          acp.SessionModeId(agent.ModeAuto),
				Name:        "Auto",
				Description: &autoDesc,
			},
			{
				Id:          acp.SessionModeId(agent.ModeChat),
				Name:        "Chat",
				Description: &chatDesc,
			},
		},
	}
}
