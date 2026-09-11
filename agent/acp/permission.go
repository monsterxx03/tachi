package acp

import (
	"context"

	acp "github.com/coder/acp-go-sdk"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/tools"
)

// buildPermissionHandler creates a PermissionHandler that delegates to the ACP
// client's RequestPermission flow. It returns whether the user approved the action.
// If the user selects "allow_all", the agent's permission mode is switched to Skip
// for the remainder of the session.
func buildPermissionHandler(conn *acp.AgentSideConnection, sessionID string, aiAgent *agent.AIAgent) agent.PermissionHandler {
	return func(ctx context.Context, toolName, toolID, diff, args string) (bool, error) {

		// Build content to show in the permission dialog.
		//
		// The structured form comes from the SAME derivation the tool-call stream uses
		// (tools.FileChangeForTool → fileChangeContent): two independent parsers of the
		// same arguments would drift, and this preview is what clients like
		// agentic.nvim render in the actual file buffer (split view or inline virtual
		// text) rather than as plain text in chat.
		var content []acp.ToolCallContent
		if diff != "" {
			if fc, ok := tools.FileChangeForTool(toolName, args); ok {
				content = append(content, *fileChangeContent(fc))
			} else {
				// Fallback: send diff as plain text
				content = append(content, acp.ToolContent(acp.TextBlock(diff)))
			}
		}

		resp, err := conn.RequestPermission(ctx, acp.RequestPermissionRequest{
			SessionId: acp.SessionId(sessionID),
			ToolCall: acp.ToolCallUpdate{
				ToolCallId: acp.ToolCallId(toolID),
				Title:      &toolName,
				Kind:       acp.Ptr(acp.ToolKindEdit),
				Status:     acp.Ptr(acp.ToolCallStatusPending),
				Content:    content,
			},
			Options: []acp.PermissionOption{
				{
					Kind:     acp.PermissionOptionKindAllowOnce,
					Name:     "Allow",
					OptionId: "allow",
				},
				{
					Kind:     acp.PermissionOptionKindRejectOnce,
					Name:     "Reject",
					OptionId: "reject",
				},
				{
					Kind:     acp.PermissionOptionKindAllowAlways,
					Name:     "Allow all edits",
					OptionId: "allow_all",
				},
			},
		})
		if err != nil {
			return false, err
		}

		// Check outcome
		if resp.Outcome.Cancelled != nil {
			return false, nil
		}
		if resp.Outcome.Selected == nil {
			return false, nil
		}

		optionID := resp.Outcome.Selected.OptionId

		// "allow_all" → switch to PermissionModeSkip for the rest of this session
		if optionID == "allow_all" {
			aiAgent.SetPermissionMode(agent.PermissionModeSkip)
			aiAgent.SetAutoApprovePolicyAsks(true) // bash policy asks: user chose allow-all
			return true, nil
		}

		return optionID == "allow", nil
	}
}
