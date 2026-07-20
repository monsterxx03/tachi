package acp

import (
	"context"
	"encoding/json"

	acp "github.com/coder/acp-go-sdk"

	"github.com/monsterxx03/tachi/agent"
)

// editArgs mirrors the EditTool argument struct for parsing diff content from args JSON.
type editArgs struct {
	FilePath   string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

// buildPermissionHandler creates a PermissionHandler that delegates to the ACP
// client's RequestPermission flow. It returns whether the user approved the action.
// If the user selects "allow_all", the agent's permission mode is switched to Skip
// for the remainder of the session.
func buildPermissionHandler(conn *acp.AgentSideConnection, sessionID string, aiAgent *agent.AIAgent) agent.PermissionHandler {
	return func(ctx context.Context, toolName, toolID, diff, args string) (bool, error) {

		// Build content to show in the permission dialog
		var content []acp.ToolCallContent
		if diff != "" {
			// Try to send a structured diff (oldText/newText) so clients like
			// agentic.nvim can show proper diff previews (split view or inline
			// virtual text) in the actual file buffer, not just plain text in chat.
			var ea editArgs
			if err := json.Unmarshal([]byte(args), &ea); err == nil && ea.FilePath != "" {
				content = append(content, acp.ToolDiffContent(ea.FilePath, ea.NewString, ea.OldString))
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
