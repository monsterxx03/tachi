package acp

import (
	"context"

	acp "github.com/coder/acp-go-sdk"

	"github.com/monsterxx03/tachi/agent"
)

// buildPermissionHandler creates a PermissionHandler that delegates to the ACP
// client's RequestPermission flow. It returns whether the user approved the action.
func buildPermissionHandler(conn *acp.AgentSideConnection, sessionID string) agent.PermissionHandler {
	return func(ctx context.Context, toolName, toolID, diff, args string) (bool, error) {
		// Build content to show in the permission dialog
		var content []acp.ToolCallContent
		if diff != "" {
			content = append(content, acp.ToolContent(acp.TextBlock(diff)))
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

		// "allow_all" could be used to switch to PermissionModeSkip,
		// but that requires access to the AIAgent — handled externally if needed.
		return resp.Outcome.Selected.OptionId == "allow" || resp.Outcome.Selected.OptionId == "allow_all", nil
	}
}
