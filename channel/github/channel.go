package github

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/google/go-github/v69/github"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/channel"
	"github.com/monsterxx03/tachi/pkg/logger"
	"gopkg.in/yaml.v3"
)

func init() {
	channel.Register("github", func(rawCfg map[string]any) (channel.Channel, error) {
		b, err := yaml.Marshal(rawCfg)
		if err != nil {
			return nil, fmt.Errorf("github: marshal config: %w", err)
		}
		cfg := &Config{}
		if err := yaml.Unmarshal(b, cfg); err != nil {
			return nil, fmt.Errorf("github: unmarshal config: %w", err)
		}
		cfg.ApplyDefaults()
		return NewChannel(cfg)
	})
}

// GitHubChannel implements channel.Channel for GitHub issue monitoring.
type GitHubChannel struct {
	cfg    *Config
	client *GitHubClient
	state  *StateManager
	logger *logger.Logger

	// provider for LLM agent calls (resolved from config).
	provider llm.Provider

	// Work queue for async processing.
	workCh chan *WorkItem

	// pollLock prevents overlapping poll cycles.
	pollLock sync.Mutex

	// inFlight tracks issues currently being processed by workers.
	inFlight sync.Map // key: "repo/issueNum"
	wg       sync.WaitGroup

	// ctx/cancel for shutdown.
	ctx    context.Context
	cancel context.CancelFunc

	// botLogin cached from the authenticated user.
	botLogin string
}

// NewChannel creates a new GitHub channel.
func NewChannel(cfg *Config) (*GitHubChannel, error) {
	log := logger.New("channel.github")
	return &GitHubChannel{
		cfg:    cfg,
		logger: log,
		workCh: make(chan *WorkItem, 500),
	}, nil
}

// Name returns the channel type identifier.
func (ch *GitHubChannel) Name() string { return "github" }

// OnStart implements channel.Channel. It validates the token, verifies repos,
// and loads state.
func (ch *GitHubChannel) OnStart(ctx context.Context) error {
	// Create API client (PAT or GitHub App).
	var client *GitHubClient
	var err error

	if ch.cfg.HasGitHubApp() {
		app := ch.cfg.GitHubApp
		client, err = NewGitHubClientFromApp(ctx, app.AppID, app.PrivateKeyPath, app.InstallationID, ch.cfg.Proxy, ch.logger)
	} else {
		token, tokErr := ch.cfg.ResolveToken()
		if tokErr != nil {
			return fmt.Errorf("github: %w", tokErr)
		}
		client, err = NewGitHubClient(ctx, token, ch.cfg.Proxy, ch.logger)
	}
	if err != nil {
		return fmt.Errorf("github: %w", err)
	}
	ch.client = client
	ch.botLogin = client.Login()

	// Load state.
	state, err := NewStateManager(ch.logger)
	if err != nil {
		return fmt.Errorf("github: %w", err)
	}
	ch.state = state

	// Resolve LLM provider for agent calls.
	if err := ch.resolveProvider(ctx); err != nil {
		// Log but don't fail — the channel can still poll without an LLM provider
		// (discussion/PR turns will be skipped until provider is configured).
		ch.logger.Error(ctx, "github: provider resolution failed, agent turns disabled",
			err)
	}

	// Verify each repo has a local clone.
	for _, repo := range ch.cfg.Repos {
		if err := ch.verifyLocalRepo(repo); err != nil {
			// Log error but don't fail — the repo will be skipped during polling.
			ch.logger.Error(ctx, "github: repo verification failed, will be skipped",
				err, "repo", repo.Name, "local_path", repo.LocalPath)
		} else {
			ch.logger.Info(ctx, "github: repo verified",
				"repo", repo.Name, "local_path", repo.LocalPath)
		}
	}

	ch.logger.Info(ctx, "github: channel initialized",
		"repos", len(ch.cfg.Repos), "bot_login", ch.botLogin)
	return nil
}

// Run implements channel.Channel. It starts the cron poller and worker
// goroutines, then blocks until context cancellation.
//
// Note: The handler parameter is accepted for interface compatibility but
// not used directly — GitHub channel is self-contained: it detects issues via
// polling, processes them internally via worker goroutines, and posts
// replies directly through the GitHub API without routing through the
// Manager's MessageHandler. This is intentional: the standard channel
// message path (channel → handler → manager → agent → reply) is designed
// for interactive IM platforms, while GitHub is an async issue tracker.
// Phase 2/3 worker logic will interact with the GitHub API directly.
func (ch *GitHubChannel) Run(ctx context.Context, handler channel.MessageHandler) error {
	ch.ctx, ch.cancel = context.WithCancel(ctx)
	defer ch.cancel()

	// Start worker goroutines (2 workers for now).
	for i := 0; i < 2; i++ {
		ch.wg.Add(1)
		go ch.workerLoop(i)
	}

	// Start polling via a ticker (simpler than SystemScheduler for now).
	// SystemScheduler requires lifecycle alignment with the manager.
	// We use a simple time.Ticker instead.
	pollInterval := ch.cfg.PollInterval
	if pollInterval <= 0 {
		pollInterval = 5 * time.Minute
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	// Run first poll immediately.
	ch.pollOnce(ctx)

	for {
		select {
		case <-ticker.C:
			ch.pollOnce(ctx)
		case <-ctx.Done():
			ch.logger.Info(ctx, "github: channel shutting down")
			return nil
		}
	}
}

// pollOnce runs one poll cycle.
func (ch *GitHubChannel) pollOnce(ctx context.Context) {
	if !ch.pollLock.TryLock() {
		ch.logger.Warn(ctx, "github: previous poll still running, skipping")
		return
	}
	defer ch.pollLock.Unlock()

	ch.logger.Info(ctx, "github: polling started")
	pollStart := time.Now()

	for _, repo := range ch.cfg.Repos {
		// Skip repos with invalid local paths.
		if !ch.isRepoValid(repo) {
			continue
		}

		owner, name, err := ParseRepo(repo.Name)
		if err != nil {
			ch.logger.Error(ctx, "github: invalid repo name",
				fmt.Errorf("invalid repo: %s", repo.Name))
			continue
		}

		rs := ch.state.Repo(repo.Name)

		// First run seed: mark all existing issues as skipped.
		if !rs.Seeded {
			ch.seedRepo(ctx, owner, name, repo.Name)
			// seedRepo internally calls SeedExistingIssues which sets Seeded=true.
			ch.state.SetLastPolledAt(repo.Name, pollStart)
			continue
		}

		// Fetch issues updated since last poll.
		issues, err := ch.client.ListIssues(ctx, owner, name, rs.LastPolledAt)
		if err != nil {
			ch.logger.Error(ctx, "github: list issues failed",
				err, "repo", repo.Name)
			continue
		}

		ch.logger.Info(ctx, "github: found issues", "repo", repo.Name, "count", len(issues))

		for _, issue := range issues {
			issueNum := fmt.Sprintf("%d", issue.GetNumber())
			ir := ch.state.GetIssueRecord(repo.Name, issueNum)

			// Fetch comments to check for new activity.
			comments, err := ch.client.ListComments(ctx, owner, name, issue.GetNumber())
			if err != nil {
				ch.logger.Error(ctx, "github: list comments failed",
					err, "repo", repo.Name, "issue", issueNum)
				continue
			}

			// Skip if issue is already being processed by a worker.
			issueKey := repo.Name + "/" + issueNum
			if _, loaded := ch.inFlight.LoadOrStore(issueKey, true); loaded {
				ch.logger.Info(ctx, "github: issue already in flight, skipping", "repo", repo.Name, "issue", issueNum)
				continue
			}

			// Find the latest non-bot comment.
			lastCommentID, lastCommentAt := ch.findLatestHumanComment(comments)

			// Skip if no new activity since last processed.
			if lastCommentID > 0 && lastCommentID <= ir.LastProcessedCommentID {
				ch.inFlight.Delete(issueKey)
				continue
			}

			// Skip if no human comments and issue was already seen (re-enqueue guard).
			if lastCommentID == 0 && ir.State != "" && ir.State != IssueStateNew {
				ch.inFlight.Delete(issueKey)
				continue
			}

			// If issue was already processed and has no new human comments, skip.
			if ir.State == IssueStateSkipped || ir.State == IssueStatePRCreated {
				ch.inFlight.Delete(issueKey)
				continue
			}

			// Enqueue for processing (watermark NOT advanced here — it's updated
			// only after successful processing in runDiscussionTurn/runPRGeneration).
			// This ensures that if the worker fails, the issue is re-enqueued on
			// the next poll rather than silently dropped.
			ch.enqueueWork(&WorkItem{
				RepoName:      repo.Name,
				RepoPath:      repo.LocalPath,
				Issue:         issue,
				IssueNum:      issueNum,
				IssueRec:      ir,
				Comments:      comments,
				BotLogin:      ch.botLogin,
				LastCommentID: lastCommentID,
				LastCommentAt: lastCommentAt,
				DefaultBranch: repo.DefaultBranch,
			})
		}

		ch.state.SetLastPolledAt(repo.Name, pollStart)

		// Check for new review comments on PRs we've created.
		ch.pollPRReviewComments(ctx, owner, name, repo.Name)
	}

	// Save state atomically.
	if err := ch.state.SaveAtomic(); err != nil {
		ch.logger.Error(ctx, "github: save state failed", err)
	}

	ch.logger.Info(ctx, "github: poll completed",
		"duration", time.Since(pollStart).Round(time.Millisecond))
}

// enqueueWork sends a work item to the worker channel, non-blocking.
func (ch *GitHubChannel) enqueueWork(item *WorkItem) {
	select {
	case ch.workCh <- item:
	default:
		ch.logger.Warn(context.Background(), "github: work queue full, dropping item",
			"repo", item.RepoName, "issue", item.IssueNum)
	}
}

// findLatestHumanComment finds the latest non-bot comment.
// De-duplication is based on comment ID, not timestamp — edited comments have
// updated UpdatedAt but unchanged ID, so they won't trigger re-processing.
// Returns (commentID, createdAt). Returns (0, zero) if no human comments.
func (ch *GitHubChannel) findLatestHumanComment(comments []*github.IssueComment) (int64, time.Time) {
	var maxID int64
	var maxTime time.Time
	for _, c := range comments {
		// Skip all bot comments (including our own and other bots).
		if c.User != nil && c.User.GetType() == "Bot" {
			continue
		}
		if c.GetID() > maxID {
			maxID = c.GetID()
			maxTime = c.GetCreatedAt().Time
		}
	}
	return maxID, maxTime
}

// isRepoValid checks if the repo has a valid local path.
func (ch *GitHubChannel) isRepoValid(repo RepoConfig) bool {
	return ch.verifyLocalRepo(repo) == nil
}

// resolveProvider resolves the LLM provider from the full config.
// Uses the configured provider name, falling back to the default provider.
func (ch *GitHubChannel) resolveProvider(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	providerName := ch.cfg.Provider
	if providerName == "" {
		providerName = cfg.Provider // use default provider
	}

	pCfg := cfg.FindProvider(providerName)
	if pCfg == nil {
		return fmt.Errorf("provider %q not found in config", providerName)
	}

	resolved, err := config.ResolveProviderConfig(pCfg)
	if err != nil {
		return fmt.Errorf("resolve provider %q: %w", providerName, err)
	}

	provider, err := llm.NewProvider(resolved.Type, resolved.APIKey, resolved.BaseURL, resolved.Model)
	if err != nil {
		return fmt.Errorf("create provider %q: %w", providerName, err)
	}

	ch.provider = provider
	ch.logger.Info(ctx, "github: provider resolved", "provider", providerName, "model", resolved.Model)
	return nil
}

// verifyLocalRepo checks that the local path is a valid git repository.
func (ch *GitHubChannel) verifyLocalRepo(repo RepoConfig) error {
	if repo.LocalPath == "" {
		return fmt.Errorf("local_path is empty")
	}
	// Check path exists.
	if _, err := os.Stat(repo.LocalPath); err != nil {
		return fmt.Errorf("local_path %q does not exist: %w", repo.LocalPath, err)
	}
	// Check it's a git repo.
	cmd := exec.Command("git", "rev-parse", "--git-dir")
	cmd.Dir = repo.LocalPath
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("local_path %q is not a git repo: %s", repo.LocalPath, string(out))
	}
	return nil
}

// seedRepo marks all existing open issues as skipped (first run).
func (ch *GitHubChannel) seedRepo(ctx context.Context, owner, name, repoName string) {
	// Fetch all open issues (no since filter = all).
	issues, err := ch.client.ListIssues(ctx, owner, name, time.Time{})
	if err != nil {
		ch.logger.Error(ctx, "github: seed repo failed",
			err, "repo", repoName)
		return
	}
	numbers := make([]string, len(issues))
	for i, issue := range issues {
		numbers[i] = fmt.Sprintf("%d", issue.GetNumber())
	}
	ch.state.SeedExistingIssues(repoName, numbers)
	ch.logger.Info(ctx, "github: repo seeded", "repo", repoName, "issues", len(numbers))
}

// --- workerLoop ---

// workerLoop processes work items from the queue.
func (ch *GitHubChannel) workerLoop(id int) {
	defer ch.wg.Done()
	ch.logger.Info(ch.ctx, "github: worker started", "id", id)

	for {
		select {
		case item := <-ch.workCh:
			ch.processWorkItem(item)
		case <-ch.ctx.Done():
			return
		}
	}
}

// processWorkItem handles a single work item.
// Routes to discussion agent or PR generation agent based on issue state.
func (ch *GitHubChannel) processWorkItem(item *WorkItem) {
	ctx := item.Ctx
	if ctx == nil {
		ctx = ch.ctx
	}

	// Release in-flight lock on exit.
	defer ch.inFlight.Delete(item.RepoName + "/" + item.IssueNum)

	ch.logger.Info(ctx, "github: processing work item",
		"repo", item.RepoName, "issue", item.IssueNum,
		"state", item.IssueRec.State)

	// Parse repo name for API calls.
	owner, name, err := ParseRepo(item.RepoName)
	if err != nil {
		ch.logger.Error(ctx, "github: parse repo name", err, "repo", item.RepoName)
		return
	}

	switch item.IssueRec.State {
	case IssueStateNew, IssueStateDiscussing, IssueStateWaitingAuthor:
		// Discussion phase.
		ch.runDiscussionTurn(ctx, item, owner, name)

	case IssueStateReadyForPR:
		// PR generation phase.
		ch.runPRGeneration(ctx, item, owner, name)

	default:
		ch.logger.Info(ctx, "github: no action for state",
			"repo", item.RepoName, "issue", item.IssueNum, "state", item.IssueRec.State)
	}
}

// runDiscussionTurn runs the discussion agent and posts the reply.
func (ch *GitHubChannel) runDiscussionTurn(ctx context.Context, item *WorkItem, owner, name string) {
	// Check if provider is available.
	if ch.provider == nil {
		ch.logger.Warn(ctx, "github: no LLM provider configured, skipping discussion turn",
			"repo", item.RepoName, "issue", item.IssueNum)
		return
	}

	// Run the discussion agent.
	result := RunDiscussionTurn(
		ctx,
		ch.provider,
		item.Issue,
		item.Comments,
		ch.botLogin,
		item.RepoName,
		item.RepoPath,
		&ch.cfg.Behavior,
		ch.cfg.Security.DiscussionTools,
		ch.logger,
	)

	if result.Err != nil {
		ch.logger.Error(ctx, "github: discussion turn failed",
			result.Err, "repo", item.RepoName, "issue", item.IssueNum)
		return
	}

	// Post reply if the agent chose to respond.
	if shouldPostReply(result.Reply) {
		reply := formatReplyForGitHub(result.Reply)
		comment, err := ch.client.CreateComment(ctx, owner, name, item.Issue.GetNumber(), reply)
		if err != nil {
			ch.logger.Error(ctx, "github: failed to post comment",
				err, "repo", item.RepoName, "issue", item.IssueNum)
			return
		}
		ch.logger.Info(ctx, "github: posted comment",
			"repo", item.RepoName, "issue", item.IssueNum,
			"comment_id", comment.GetID())
	} else {
		ch.logger.Info(ctx, "github: agent chose not to reply",
			"repo", item.RepoName, "issue", item.IssueNum)
	}

	// Update issue state.
	ch.state.UpdateIssueState(item.RepoName, item.IssueNum, result.NewState)

	// Advance watermark only after successful processing.
	if item.LastCommentID > 0 {
		ch.state.MarkIssueProcessed(item.RepoName, item.IssueNum, item.LastCommentID, item.LastCommentAt)
	}

	if err := ch.state.SaveAtomic(); err != nil {
		ch.logger.Error(ctx, "github: save state after discussion", err)
	}

	ch.logger.Info(ctx, "github: discussion turn completed",
		"repo", item.RepoName, "issue", item.IssueNum,
		"new_state", result.NewState)
}

// runPRGeneration runs the PR generation agent and creates the PR.
func (ch *GitHubChannel) runPRGeneration(ctx context.Context, item *WorkItem, owner, name string) {
	if ch.provider == nil {
		ch.logger.Warn(ctx, "github: no LLM provider configured, skipping PR generation",
			"repo", item.RepoName, "issue", item.IssueNum)
		return
	}

	// Resolve token for git push (PAT or GitHub App installation token).
	var token string
	var tokErr error
	if ch.cfg.HasGitHubApp() {
		token, tokErr = ch.cfg.ResolveInstallationToken(ctx)
	} else {
		token, tokErr = ch.cfg.ResolveToken()
	}
	if tokErr != nil {
		ch.logger.Error(ctx, "github: resolve token for PR generation",
			tokErr, "repo", item.RepoName, "issue", item.IssueNum)
		return
	}

	// Update state to implementing (for crash recovery).
	ch.state.UpdateIssueState(item.RepoName, item.IssueNum, IssueStateImplementing)
	if err := ch.state.SaveAtomic(); err != nil {
		ch.logger.Error(ctx, "github: save state before PR generation", err)
	}

	// Run the PR generation agent.
	// Fetch labels for PR gate checking.
	labels, _ := ch.client.GetIssueLabels(ctx, owner, name, item.Issue.GetNumber())

	prResult := RunPRGeneration(ctx, &PRGenerationConfig{
		Provider:      ch.provider,
		Token:         token,
		Issue:         item.Issue,
		Comments:      item.Comments,
		Labels:        labels,
		RepoName:      item.RepoName,
		RepoLocalPath: item.RepoPath,
		BotLogin:      ch.botLogin,
		Owner:         owner,
		Repo:          name,
		DefaultBranch: item.DefaultBranch,
		Behavior:      &ch.cfg.Behavior,
		Security:      &ch.cfg.Security,
		ToolNames:     ch.cfg.Security.ImplementationTools,
		BashAllow:     ch.cfg.Security.BashAllow,
		Logger:        ch.logger,
	})

	if prResult.Err != nil {
		ch.logger.Error(ctx, "github: PR generation failed",
			prResult.Err, "repo", item.RepoName, "issue", item.IssueNum)

		// Retry logic: if under max retries, go back to ready_for_pr.
		retryCount := ch.state.IncrementRetryCount(item.RepoName, item.IssueNum)
		if retryCount < ch.cfg.Behavior.MaxImplementationRetries {
			ch.state.UpdateIssueState(item.RepoName, item.IssueNum, IssueStateReadyForPR)
		} else {
			ch.state.UpdateIssueState(item.RepoName, item.IssueNum, IssueStateSkipped)
		}
		if err := ch.state.SaveAtomic(); err != nil {
			ch.logger.Error(ctx, "github: save state after PR failure", err)
		}
		return
	}

	// Create the PR via GitHub API.
	prTitle := buildPRTitle(item.Issue)
	prBody := buildPRBody(item.Issue, "")
	prDraft := true
	if ch.cfg.Behavior.PRAsDraft != nil {
		prDraft = *ch.cfg.Behavior.PRAsDraft
	}

	baseBranch := item.DefaultBranch
	if baseBranch == "" {
		baseBranch = "main"
	}

	pr, err := ch.client.CreatePR(ctx, owner, name, prTitle, prBody, prResult.Branch, baseBranch, prDraft)
	if err != nil {
		ch.logger.Error(ctx, "github: create PR failed",
			err, "repo", item.RepoName, "issue", item.IssueNum, "branch", prResult.Branch)

		// State stays as implementing — next poll will retry.
		return
	}

	// Post a comment on the issue with the PR link.
	comment := fmt.Sprintf("I've created a pull request for this issue:\n\n%s", pr.GetHTMLURL())
	if _, err := ch.client.CreateComment(ctx, owner, name, item.Issue.GetNumber(), comment); err != nil {
		ch.logger.Error(ctx, "github: post PR comment failed",
			err, "repo", item.RepoName, "issue", item.IssueNum)
		// Non-fatal — PR was created successfully.
	}

	// Update state to pr_created.
	ch.state.SetPRNumber(item.RepoName, item.IssueNum, pr.GetNumber())
	ch.state.UpdateIssueState(item.RepoName, item.IssueNum, IssueStatePRCreated)

	// Advance watermark only after successful processing.
	if item.LastCommentID > 0 {
		ch.state.MarkIssueProcessed(item.RepoName, item.IssueNum, item.LastCommentID, item.LastCommentAt)
	}

	if err := ch.state.SaveAtomic(); err != nil {
		ch.logger.Error(ctx, "github: save state after PR creation", err)
	}

	ch.logger.Info(ctx, "github: PR created successfully",
		"repo", item.RepoName, "issue", item.IssueNum,
		"pr", pr.GetNumber(), "url", pr.GetHTMLURL())
}

// pollPRReviewComments checks for new review comments on PRs we've created.
// TODO: Detected comments are logged but not yet processed by an agent turn.
// This is a placeholder — the watermark is updated to avoid re-fetching, but
// no reply is posted. Implement agent-based reply in a future phase.
func (ch *GitHubChannel) pollPRReviewComments(ctx context.Context, owner, name, repoName string) {
	ch.state.ForEachPRCreatedIssue(repoName, func(issueNum string, ir IssueRecord) {
		comments, err := ch.client.ListPullRequestReviewComments(ctx, owner, name, ir.PRNumber, time.Time{})
		if err != nil {
			ch.logger.Error(ctx, "github: list PR review comments failed",
				err, "repo", repoName, "pr", ir.PRNumber)
			return
		}

		var newComments []*github.PullRequestComment
		for _, c := range comments {
			if c.User != nil && c.User.GetType() == "Bot" {
				continue
			}
			if c.GetID() > ir.LastProcessedReviewCommentID {
				newComments = append(newComments, c)
			}
		}

		if len(newComments) == 0 {
			return
		}

		ch.logger.Info(ctx, "github: new PR review comments detected (not yet processed)",
			"repo", repoName, "pr", ir.PRNumber, "count", len(newComments))

		latestID := ir.LastProcessedReviewCommentID
		for _, c := range newComments {
			if c.GetID() > latestID {
				latestID = c.GetID()
			}
		}
		ch.state.UpdateReviewCommentID(repoName, issueNum, latestID)
	})
}
