package github

import (
	"context"
	"fmt"

	"github.com/google/go-github/v69/github"
	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/agent/wdctx"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// DiscussionResult describes the outcome of a discussion agent turn.
type DiscussionResult struct {
	// Reply is the text to post as an issue comment (empty means no reply).
	Reply string

	// NewState is the state to transition the issue to.
	NewState IssueState

	// Err is non-nil when the agent turn failed unrecoverably.
	Err error
}

// RunDiscussionTurn runs one discussion agent turn for the given issue.
// It creates a one-shot agent with read-only tools, builds the conversation
// context from the issue and comments, and returns the agent's reply.
//
// The agent is created using the dream runner pattern (NewAIAgent + explicit
// tool registration, no Fork). Memory and skill tools are intentionally
// excluded — this channel is focused on issue discussion only.
func RunDiscussionTurn(
	ctx context.Context,
	provider llm.Provider,
	issue *github.Issue,
	comments []*github.IssueComment,
	botLogin string,
	repoName string,
	repoLocalPath string,
	cfg *BehaviorConfig,
	toolNames []string,
	log *logger.Logger,
) *DiscussionResult {
	// Create a new agent (dream pattern: stateless, one-shot, explicit tool whitelist).
	maxIter := cfg.MaxDiscussionTurns
	if maxIter <= 0 {
		maxIter = 10
	}
	discussionAgent := agent.NewAIAgent(provider, maxIter)
	discussionAgent.SetPermissionMode(agent.PermissionModeSkip)
	if log != nil {
		discussionAgent.SetLogger(log)
	}

	// Register only the allowed discussion tools.
	registerDiscussionTools(discussionAgent, toolNames)

	// Set working directory to the local repo clone so tools resolve relative paths.
	ctx = wdctx.WithDir(ctx, repoLocalPath)

	// Build prompts.
	systemPrompt, userMessage := BuildDiscussionPrompt(issue, comments, botLogin, repoName)

	// Run the agent (one-off, no session recording; sidecar transcript kept
	// in the global oneoff dir for troubleshooting).
	eventCh := discussionAgent.RunOneOffStream(ctx, provider, systemPrompt, userMessage, llm.ChatOptions{
		MaxTokens: agent.DefaultMaxTokens,
	}, agent.WithOneOffMeta(&agent.OneOffMeta{
		Kind:  "github-discussion",
		Extra: map[string]string{"repo": repoName},
	}))

	// Drain events.
	var result *agent.RunResult
	for ev := range eventCh {
		switch ev.Type {
		case agent.AgentEventTurnComplete:
			result = ev.Result
		case agent.AgentEventError:
			if ev.Result != nil && ev.Result.Error != nil {
				return &DiscussionResult{
					Err: fmt.Errorf("discussion agent error: %w", ev.Result.Error),
				}
			}
		case agent.AgentEventToolConfirmation:
			// Dead code under PermissionModeSkip + read-only tools:
			// PermissionModeSkip auto-approves all tool confirmations, and
			// discussion tools are all read-only (no ConfirmationTool impl).
			// Kept as defensive code for future config changes.
			discussionAgent.ConfirmTool(agent.ConfirmAllowOnce)
		default:
			// Consume all other event types (e.g. AgentEventTextDelta) to
			// prevent event channel backpressure in future versions.
		}
	}

	if result == nil {
		return &DiscussionResult{
			Err: fmt.Errorf("discussion agent returned no result"),
		}
	}

	if result.Error != nil {
		return &DiscussionResult{
			Err: fmt.Errorf("discussion agent failed: %w", result.Error),
		}
	}

	// Parse the agent's output to determine the next state.
	reply := result.Response
	newState := determineNextState(reply)

	return &DiscussionResult{
		Reply:    reply,
		NewState: newState,
	}
}

// registerDiscussionTools registers the allowed tools for the discussion phase.
// Only read-only tools are registered — no write tools, no memory, no skills.
func registerDiscussionTools(a *agent.AIAgent, toolNames []string) {
	// Map of tool name to constructor.
	constructors := map[string]func() tools.Tool{
		"ReadFile":  func() tools.Tool { return tools.NewReadTool() },
		"Grep":      func() tools.Tool { return tools.GrepTool{} },
		"Glob":      func() tools.Tool { return tools.GlobTool{} },
		"WebSearch": func() tools.Tool { return &tools.WebSearchTool{} },
		"WebFetch":  func() tools.Tool { return &tools.WebFetchTool{} },
	}

	if len(toolNames) == 0 {
		// Default: register all read-only tools.
		for _, constructor := range constructors {
			a.RegisterTool(constructor())
		}
		return
	}

	for _, name := range toolNames {
		if constructor, ok := constructors[name]; ok {
			a.RegisterTool(constructor())
		}
	}
}

// determineNextState parses the agent's reply to determine the next issue state.
func determineNextState(reply string) IssueState {
	if IsNoReply(reply) {
		return IssueStateWaitingAuthor
	}
	if HasControlMarker(reply, "READY_FOR_PR") || HasControlMarker(reply, "IMPLEMENT") {
		return IssueStateReadyForPR
	}
	return IssueStateDiscussing
}

// formatReplyForGitHub prepares the agent's reply for posting to GitHub.
// It strips control markers and trims whitespace.
func formatReplyForGitHub(reply string) string {
	return StripControlMarkers(reply)
}

// shouldPostReply checks whether the agent's reply should be posted as a comment.
func shouldPostReply(reply string) bool {
	return !IsNoReply(reply) && reply != ""
}
