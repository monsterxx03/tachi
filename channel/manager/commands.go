package manager

import (
	"context"
	"errors"
	"fmt"
	"strings"

	cmds "github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/pkg/channel"
)

// --- CommandHandler bridge: typed slash command dispatch ---

// buildCommandHandler returns a channel.CommandHandler that dispatches
// typed SlashCommand values to the Manager's slash command methods.
// This allows channels to invoke manager operations programmatically
// without routing through the text-based message handler path.
func (m *Manager) buildCommandHandler() channel.CommandHandler {
	return func(ctx context.Context, cmd channel.SlashCommand) (channel.OutgoingMessage, string, string, string, error) {
		result, err := m.executeSlashCommand(ctx, cmd)
		if err != nil {
			// Carry partial output (e.g. a /review that failed mid-chain) so
			// the channel can show completed work alongside the error (B7).
			reply := result.Reply
			if reply.ThreadID == "" {
				reply.ThreadID = cmd.ThreadID
			}
			return reply, "", "", "", err
		}
		// Read the current workDir from cache for channel topic updates.
		workDir := result.WorkDir
		if workDir == "" {
			workDir = m.getThreadWorkDir(cmd.ThreadID)
		}
		// Resolve the current model + provider name for channel topic display.
		resolved := m.getProviderForThread(cmd.ThreadID)
		model := resolved.Model
		provider := resolved.Name
		// Return the full OutgoingMessage so channels can send attachments
		// (e.g., /transcript HTML file) alongside the text reply.
		reply := result.Reply
		if reply.ThreadID == "" {
			reply.ThreadID = cmd.ThreadID
		}
		return reply, workDir, model, provider, result.Err
	}
}

// executeSlashCommand dispatches a SlashCommand to the appropriate handler.
// Returns a HandlerResult so commands that need file attachments (e.g. /transcript)
// can include them. Text-only commands return HandlerResult with just Content set.
//
// ctx carries the channel's streaming callback (Discord status embeds) for
// long-running LLM commands like /commit and /review, plus cancellation.
func (m *Manager) executeSlashCommand(ctx context.Context, cmd channel.SlashCommand) (channel.HandlerResult, error) {
	switch cmd.Name {
	case "new":
		text, err := m.handleNewCommand(cmd.ThreadID)
		return textHandlerResult(text), err
	case "commit":
		text, err := m.handleCommitCommand(ctx, cmd.ThreadID)
		return textHandlerResult(text), err
	case "review":
		text, err := m.handleReviewCommand(ctx, cmd.ThreadID, cmd.Args)
		return textHandlerResult(text), err
	case "mcp":
		if cmd.Args != "" {
			argParts := strings.Fields(cmd.Args)
			if len(argParts) > 0 && argParts[0] == "auth" {
				serverName := ""
				if len(argParts) > 1 {
					serverName = argParts[1]
				}
				text, err := m.handleMCPAuth(cmd.ThreadID, serverName)
				return textHandlerResult(text), err
			}
			if len(argParts) > 0 && argParts[0] == "profile" {
				// Profile switching is TUI/ACP-only for now: channel mode shares
				// one MCP manager across threads, so switching affects every
				// conversation at once.
				return textHandlerResult("MCP profile switching is not supported in channel mode yet — it is available via `/mcp profile` in the TUI or ACP mode."), nil
			}
		}
		text, err := m.handleMCPList(cmd.ThreadID)
		return textHandlerResult(text), err
	case "usage":
		text, err := m.handleUsageCommand(cmd.ThreadID)
		return textHandlerResult(text), err
	case "cron":
		text, err := m.handleCronCommand(cmd.ThreadID)
		return textHandlerResult(text), err
	case "cd":
		text, err := m.handleCDCommand(cmd.ThreadID, cmd.Args)
		return textHandlerResult(text), err
	case "sh":
		text, err := m.handleShCommand(ctx, cmd.ThreadID, cmd.Args)
		return textHandlerResult(text), err
	case "stop":
		text, err := m.handleStopCommand(cmd.ThreadID)
		return textHandlerResult(text), err
	case "model":
		text, err := m.handleModelCommand(cmd.ThreadID, cmd.Args)
		return textHandlerResult(text), err
	case "thinking":
		text, err := m.handleThinkingCommand(cmd.ThreadID, cmd.Args)
		return textHandlerResult(text), err
	case "skill":
		text, err := m.handleSkillCommand(cmd.Args)
		return textHandlerResult(text), err
	case "transcript":
		return m.handleTranscriptCommand(cmd.ThreadID, cmd.Args), nil
	case "research":
		text, err := m.handleResearchCommand(cmd.ThreadID, cmd.Args)
		return textHandlerResult(text), err
	case "restart":
		text, err := m.handleRestartCommand(cmd.ThreadID)
		return textHandlerResult(text), err
	default:
		m.logger.Info(context.Background(), "channel: unknown slash command", "action", cmd.Name, "thread", cmd.ThreadID)
		// Build available commands list from shared registry.
		var help strings.Builder
		fmt.Fprintf(&help, "Unknown command: /%s\n\nAvailable commands in channel mode:\n", cmd.Name)
		for _, def := range cmds.ForMode(cmds.ModeChannel) {
			switch def.Name {
			case "mcp":
				fmt.Fprintf(&help, "  /%-12s — %s\n", def.Name, def.Description)
				help.WriteString("  /mcp auth <server> — Start OAuth authorization for an MCP server\n")
			default:
				fmt.Fprintf(&help, "  /%-12s — %s\n", def.Name, def.Description)
			}
		}
		return textHandlerResult(help.String()), nil
	}
}

// textHandlerResult wraps a text string into a HandlerResult with just Content set.
// The caller (handleSlashCommand) fills in ThreadID/ReplyTo for the channel reply.
func textHandlerResult(text string) channel.HandlerResult {
	return channel.HandlerResult{
		Reply: channel.OutgoingMessage{
			Content: text,
		},
	}
}

// handleSlashCommand parses a text-based slash command from an IncomingMessage
// into a typed SlashCommand, then delegates to executeSlashCommand.
// Returns a fully populated HandlerResult with ThreadID and ReplyTo set.
func (m *Manager) handleSlashCommand(ctx context.Context, msg channel.IncomingMessage) channel.HandlerResult {
	parts := strings.Fields(msg.Content)
	if len(parts) == 0 {
		return channel.HandlerResult{}
	}
	name := strings.TrimPrefix(parts[0], "/")
	args := ""
	if len(parts) > 1 {
		args = strings.Join(parts[1:], " ")
	}
	result, err := m.executeSlashCommand(ctx, channel.SlashCommand{
		Name:     name,
		ThreadID: msg.ThreadID,
		Args:     args,
	})
	if err != nil {
		// Errors from handlers: wrap in a reply with ThreadID/ReplyTo.
		// The handler's partial output (single-round review text, or the
		// multi-round status message) is kept when non-empty — completed
		// work must not be silently discarded (B7) — and the error is
		// appended exactly once (handlers deliberately don't embed it).
		content := fmt.Sprintf("❌ %v", err)
		if errors.Is(err, context.Canceled) {
			// User-initiated /stop — the stop reply already acknowledged it.
			// Keep the handler's status summary (e.g. report dir) if any.
			content = "⏹️ 已取消。"
			if result.Reply.Content != "" {
				content = result.Reply.Content + "\n\n" + content
			}
		} else if result.Reply.Content != "" {
			content = result.Reply.Content + "\n\n❌ " + err.Error()
		}
		result = channel.HandlerResult{
			Reply: channel.OutgoingMessage{
				ThreadID: msg.ThreadID,
				Content:  content,
				ReplyTo:  msg.MessageID,
			},
			Err: err,
		}
	}
	// Fill in ThreadID/ReplyTo for text-only commands that don't set them.
	// Commands like /transcript already set these themselves.
	if result.Reply.ThreadID == "" {
		result.Reply.ThreadID = msg.ThreadID
	}
	if result.Reply.ReplyTo == "" {
		result.Reply.ReplyTo = msg.MessageID
	}

	// Propagate the thread's current working directory, model, and provider
	// so channel implementations can update platform-specific UI (e.g.,
	// Discord channel topic, Wave group announcement). Commands like /cd
	// have just updated the workdir in the cache; model/provider come from
	// the thread's resolved provider (session override wins). Filling them
	// here keeps the HandlerResult complete even for state-less commands
	// like /model (list mode), so channels never observe empty values.
	if result.WorkDir == "" {
		result.WorkDir = m.getThreadWorkDir(msg.ThreadID)
	}
	if result.Model == "" || result.Provider == "" {
		resolved := m.getProviderForThread(msg.ThreadID)
		if result.Model == "" {
			result.Model = resolved.Model
		}
		if result.Provider == "" {
			result.Provider = resolved.Name
		}
	}

	return result
}
