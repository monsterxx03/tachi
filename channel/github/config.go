package github

import (
	"fmt"
	"os"
	"time"
)

// Defaults
const (
	defaultPollInterval             = 5 * time.Minute
	defaultMaxDiscussionTurns       = 10
	defaultMaxImplementationTurns   = 50
	defaultMaxImplementationRetries = 3
	defaultWaitingAuthorTimeout     = 168 * time.Hour // 7 days
)

// Config holds the GitHub channel configuration.
// Maps to the "github" entry in config.yaml channel.channels.
type Config struct {
	// Token is the GitHub PAT. Can be set directly or loaded from token_env.
	Token string `yaml:"token"`

	// TokenEnv specifies an environment variable name to read the token from.
	// Recommended over Token to avoid plaintext secrets in config.
	TokenEnv string `yaml:"token_env"`

	// GitHubApp configures GitHub App authentication (alternative to PAT).
	// When set, Token and TokenEnv are ignored.
	GitHubApp *AppConfig `yaml:"github_app,omitempty"`

	// Repos is the list of repositories to monitor.
	Repos []RepoConfig `yaml:"repos"`

	// PollInterval is how often to poll GitHub API for new issues.
	PollInterval time.Duration `yaml:"poll_interval" default:"5m"`

	// Provider references a provider name from config's providers list for LLM calls.
	// When empty, falls back to the default provider.
	Provider string `yaml:"provider"`

	// Behavior controls how the bot interacts with issues.
	Behavior BehaviorConfig `yaml:"behavior"`

	// Security controls access and tool restrictions.
	Security SecurityConfig `yaml:"security"`
}

// AppConfig configures GitHub App authentication.
type AppConfig struct {
	// AppID is the GitHub App ID (required).
	AppID int64 `yaml:"app_id"`

	// PrivateKeyPath is the path to the GitHub App's private key PEM file (required).
	PrivateKeyPath string `yaml:"private_key_path"`

	// InstallationID is the installation ID of the app on the target account/org (required).
	InstallationID int64 `yaml:"installation_id"`
}

// RepoConfig configures a single repository to monitor.
type RepoConfig struct {
	// Name is the GitHub repo identifier, e.g. "owner/repo".
	Name string `yaml:"name"`

	// LocalPath is the absolute path to a local clone of this repo.
	// Must exist — the bot does NOT auto-clone.
	LocalPath string `yaml:"local_path"`

	// DefaultBranch is the base branch for PRs. When empty, defaults to "main".
	DefaultBranch string `yaml:"default_branch"`
}

// BehaviorConfig controls bot interaction behavior.
type BehaviorConfig struct {
	// AutoRespond, when true, automatically replies to new issues.
	AutoRespond bool `yaml:"auto_respond" default:"true"`

	// PRAsDraft creates PRs as drafts when true. Defaults to true.
	// Uses *bool so we can distinguish "unset" from "explicitly false".
	PRAsDraft *bool `yaml:"pr_as_draft"`

	// MaxDiscussionTurns limits the iteration budget for discussion agent turns.
	MaxDiscussionTurns int `yaml:"max_discussion_turns" default:"10"`

	// MaxImplementationTurns limits the iteration budget for PR generation agent turns.
	MaxImplementationTurns int `yaml:"max_implementation_turns" default:"50"`

	// MaxImplementationRetries limits retries after implementing state crash recovery.
	MaxImplementationRetries int `yaml:"max_implementation_retries" default:"3"`

	// WaitingAuthorTimeout is how long to wait for the issue author's reply before skipping.
	WaitingAuthorTimeout time.Duration `yaml:"waiting_author_timeout" default:"168h"`
}

// SecurityConfig controls access and tool restrictions.
type SecurityConfig struct {
	// AllowedActions enumerates what the bot may do: "comment", "create_pr".
	AllowedActions []string `yaml:"allowed_actions"`

	// PRGate controls who can trigger PR generation.
	PRGate PRGateConfig `yaml:"pr_gate"`

	// DiscussionTools lists the tools available during discussion phase.
	DiscussionTools []string `yaml:"discussion_tools"`

	// ImplementationTools lists the tools available during PR generation phase.
	ImplementationTools []string `yaml:"implementation_tools"`

	// BashAllow is the whitelist of bash command patterns allowed during implementation.
	BashAllow []string `yaml:"bash_allow"`
}

// PRGateConfig controls PR generation gating.
type PRGateConfig struct {
	// AllowedAssociations lists which author_association values are allowed to
	// trigger PR generation (e.g., "OWNER", "MEMBER", "COLLABORATOR").
	AllowedAssociations []string `yaml:"allowed_associations"`

	// Label, if set, is a GitHub label that also grants PR generation access.
	// A maintainer adding this label to an issue bypasses the association check.
	Label string `yaml:"label"`
}

// DefaultDiscussionTools returns the default tools for discussion phase.
// No MemoryRecall, MemoryRecord, or Skill tools.
func DefaultDiscussionTools() []string {
	return []string{
		"WebSearch",
		"WebFetch",
		"ReadFile",
		"Grep",
		"Glob",
	}
}

// DefaultImplementationTools returns the default tools for implementation phase.
func DefaultImplementationTools() []string {
	return []string{
		"ReadFile",
		"WriteFile",
		"EditFile",
		"Bash",
		"Glob",
		"Grep",
		"SubAgent",
	}
}

// DefaultBashAllow returns the default bash command whitelist.
func DefaultBashAllow() []string {
	return []string{
		"git *",
		"rg *",
		"go *",
	}
}

// DefaultPRGateAssociations returns the default allowed associations.
func DefaultPRGateAssociations() []string {
	return []string{"OWNER", "MEMBER", "COLLABORATOR"}
}

// ApplyDefaults fills in zero-value fields with sensible defaults.
func (c *Config) ApplyDefaults() {
	if c.PollInterval <= 0 {
		c.PollInterval = defaultPollInterval
	}
	if c.Behavior.MaxDiscussionTurns <= 0 {
		c.Behavior.MaxDiscussionTurns = defaultMaxDiscussionTurns
	}
	if c.Behavior.MaxImplementationTurns <= 0 {
		c.Behavior.MaxImplementationTurns = defaultMaxImplementationTurns
	}
	if c.Behavior.MaxImplementationRetries <= 0 {
		c.Behavior.MaxImplementationRetries = defaultMaxImplementationRetries
	}
	if c.Behavior.WaitingAuthorTimeout <= 0 {
		c.Behavior.WaitingAuthorTimeout = defaultWaitingAuthorTimeout
	}
	if len(c.Security.DiscussionTools) == 0 {
		c.Security.DiscussionTools = DefaultDiscussionTools()
	}
	if len(c.Security.ImplementationTools) == 0 {
		c.Security.ImplementationTools = DefaultImplementationTools()
	}
	if len(c.Security.BashAllow) == 0 {
		c.Security.BashAllow = DefaultBashAllow()
	}
	if len(c.Security.PRGate.AllowedAssociations) == 0 {
		c.Security.PRGate.AllowedAssociations = DefaultPRGateAssociations()
	}
}

// HasGitHubApp returns true if GitHub App authentication is configured.
func (c *Config) HasGitHubApp() bool {
	return c.GitHubApp != nil && c.GitHubApp.AppID > 0 && c.GitHubApp.PrivateKeyPath != "" && c.GitHubApp.InstallationID > 0
}

// ResolveToken returns the token, preferring token_env over the direct token field.
// token_env, when set, takes priority over the token field. If token_env is set
// but the environment variable is empty, an error is returned — there is no
// fallback to the token field. This is intentional: if a user explicitly sets
// token_env, they expect the token to come from the environment; falling back to
// config would silently bypass the env var and undermine the security benefit.
//
// When GitHub App auth is configured (HasGitHubApp returns true), this method
// returns an error — use ResolveInstallationToken instead.
func (c *Config) ResolveToken() (string, error) {
	if c.HasGitHubApp() {
		return "", fmt.Errorf("github: GitHub App is configured, use ResolveInstallationToken instead")
	}
	if c.TokenEnv != "" {
		tok := os.Getenv(c.TokenEnv)
		if tok != "" {
			return tok, nil
		}
		return "", fmt.Errorf("github: token_env %q is set but environment variable is empty", c.TokenEnv)
	}
	if c.Token != "" {
		return c.Token, nil
	}
	return "", fmt.Errorf("github: no token configured — set token, token_env, or github_app in config")
}
