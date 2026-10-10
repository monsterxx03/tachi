package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/creasty/defaults"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/logger"
	"gopkg.in/yaml.v3"
)

const (
	DefaultMaxTokens     = 128000
	DefaultMaxIterations = 50
	// DefaultJevTimeout is the bound ONE Jev HTTP attempt gets when the file omits
	// jev.timeout. The number lives twice by necessity — a struct tag cannot be a
	// constant, which is the same reason DefaultMaxTokens appears in the MaxTokens tag
	// too — so config_test pins the tag against this constant, and pkg/jev.DefaultTimeout
	// holds the same value for a client built without a config.
	DefaultJevTimeout = 10 * time.Second
	// DefaultJevQuestionsPerRequest is how many questions ONE request carries
	// against the REFERENCE service, i.e. what JevConfig.QuestionsPerRequest
	// answers when nothing more specific applies. It is a budgeting figure, not
	// a measurement: that service's binding limit is state plus the LONGEST
	// single question (~32k) and it also counts question COUNT against its
	// total, so 100 questions of ~80 tokens of question text each is the shape
	// that stays well inside both. A gateway in front of it caps the count lower
	// — see jevGatewayQuestionCaps.
	DefaultJevQuestionsPerRequest = 100
	// DefaultJevAskBudget is the bound one bash-ask judgment waits — the same
	// tag/constant duplication as DefaultJevTimeout. It is deliberately shorter
	// than a Jev call's own timeout: this judgment sits in front of EVERY
	// non-denied command, so a slow answer is a slow turn, and an unanswered
	// judgment falls back to the rules (agent/agent_permission_jev.go).
	DefaultJevAskBudget = 3 * time.Second
	// DefaultFilesJevBudget is the bound one FILE-ask judgment waits (see
	// FilesPermissions.JevBudget): shorter than the bash one because a file call
	// happens several times per turn, and shorter is still the safe direction —
	// an unanswered judgment falls back to the rules.
	DefaultFilesJevBudget = 2 * time.Second
	// FilesJevShadowThreshold is the τ that can never act: a noul answer is at most
	// 1.0, so a gate running at this threshold judges and logs every call without
	// ever asking. It is not the default (FilesJevThreshold is), and it is kept as
	// the setting to shadow a future question with: watch the distribution on real
	// traffic before letting it act.
	FilesJevShadowThreshold = 1.0
	// FilesJevThreshold is the shipped τ for permissions.files.jev_threshold —
	// the same tag/constant duplication as the other defaults. It was swept on
	// pkg/jev/testdata/files-ask.json; the field's own comment carries the
	// corridor, what it misses, and when to re-sweep.
	FilesJevThreshold = 0.60
	configDirName     = ".tachi"
	configFileName    = "config.yaml"
	// backupSuffix is what the pre-first-save copy of config.yaml is kept as (see backupOnce):
	// the hand-written original, beside the file the settings page now owns.
	backupSuffix              = ".bak"
	inputHistoryFileName      = "input_history"
	sessionDirName            = "session"
	logsDirName               = "logs"
	mcpTokensDirName          = "mcp_tokens"
	mcpSummariesDirName       = "mcp_summaries"
	mcpDiscoveredFileName     = "mcp_discovered.json"
	skillsDirName             = "skills"
	weixinStateDirName        = "weixin"
	researchDirName           = "research"
	cronStoreFileName         = "crons.json"
	toolResultsDirName        = "tool_results"
	oneoffDirName             = "oneoff"
	usageDirName              = "usage"
	nodeDirName               = "node"
	defaultToolResultMaxChars = 50000
)

// baseDir is the base directory for all tachi state. Default: $HOME/.tachi.
// Can be overridden via SetBaseDir() before any other config functions are called.
var baseDir string

// SetBaseDir overrides the default base directory. Must be called before
// any other config functions (e.g., from main with a --home CLI flag).
func SetBaseDir(dir string) {
	baseDir = dir
}

// BaseDir returns the resolved base directory. If SetBaseDir was called,
// returns that value. Otherwise falls back to $HOME/.tachi.
func BaseDir() string {
	if baseDir != "" {
		return baseDir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// Best-effort fallback: return relative ".tachi"
		return configDirName
	}
	return filepath.Join(home, configDirName)
}

// FindProjectRoot walks up from the current working directory to find the
// nearest git repository root (directory containing .git). If found, returns
// that directory. Otherwise returns the current working directory as-is.
// Returns "" if os.Getwd() fails.
func FindProjectRoot() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return FindProjectRootFrom(cwd)
}

// FindProjectRootFrom is FindProjectRoot for an explicit starting directory.
//
// The directory cannot always come from the process: one desktop process hosts
// several sessions in different trees, and its own working directory is
// meaningless there (macOS hands a Finder-launched app "/"). Callers that know
// which tree they are in — a session's working directory, a sub-agent worktree —
// pass it in. An empty dir has no root to find and returns "".
func FindProjectRootFrom(dir string) string {
	if dir == "" {
		return ""
	}
	d := dir
	for {
		gitPath := filepath.Join(d, ".git")
		if fileutil.Exists(gitPath) {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			// Reached filesystem root — stop
			break
		}
		d = parent
	}
	return dir
}

// ProjectRootsFrom resolves the trees one session works in: the primary
// directory first, then every additional workspace root, each walked up to its
// own project root (FindProjectRootFrom) and de-duplicated in order.
//
// The rule this exists for: a session can span SEVERAL repositories (the
// desktop's extra workspace roots, ACP's additionalDirectories), and each of
// them may carry its own .tachi/permissions.yaml. Reading only the primary's
// file meant the second repository's deny/ask rules silently did not apply —
// a tripwire the user had written that simply was not there. One tree that
// resolves onto another (a root inside the primary's own repository) is listed
// once, so its file is not read twice.
func ProjectRootsFrom(primary string, additional []string) []string {
	seen := make(map[string]bool, 1+len(additional))
	roots := make([]string, 0, 1+len(additional))
	for _, dir := range append([]string{primary}, additional...) {
		root := FindProjectRootFrom(dir)
		if root == "" || seen[root] {
			continue
		}
		seen[root] = true
		roots = append(roots, root)
	}
	return roots
}

// ModelSpec 汇总模型级运行时属性：上下文窗口、定价、思考级别、请求行为。
// 通过 ProviderConfig.Spec 嵌套配置（不向前兼容旧版平铺字段）。
type ModelSpec struct {
	// ContextWindow 手动覆盖模型上下文窗口（tokens）。
	ContextWindow *int64 `yaml:"context_window,omitempty"`

	// ThinkingLevel 控制思考模式强度。不同模型支持不同级别：
	//   - "none"             关闭思考模式
	//   - "low"/"medium"/"high"/"xhigh"/"max"  设置思考强度
	// 空 = 使用模型内置默认（DeepSeek: 思考开启、effort high；Anthropic: adaptive）。
	// 请求的级别会原样透传给 API，由服务端映射到模型实际的推理强度
	// （如 DeepSeek thinking_mode 文档的 effort 映射表）。
	ThinkingLevel string `yaml:"thinking_level,omitempty"`

	// ThinkingEfforts 覆盖该 provider 端点接受的思考档位
	// （取值同 ThinkingLevel 的具体档位，顺序无关）。空 = 用模型内置的档位表
	// （llm.ModelThinkingEfforts，厂商文档口径），内置表也没有声明时 = 全部可用。
	//
	// 为什么需要一层覆盖：级别是原样透传的，而端点把 effort 映射到另一个参数时
	// （某些网关上只接受一部分档位），送一个它不认的档位不是被忽略而是整单失败
	// ——整个请求 400，且这个档位已经写进 session meta，会话会一直失败到用户
	// 手动改回来。同一模型在不同线路上的档位可以不同（实测某网关的
	// chat-completions 拒绝 max、Responses 接受），内置表描述模型本身，线路差异
	// 落在这一层。与 spec.vision / llm.ModelSupportsVision 是同一种关系。
	ThinkingEfforts []string `yaml:"thinking_efforts,omitempty"`

	// Vision 标记模型是否支持图片（多模态）输入。
	//   - nil（默认）：按模型名内置能力表自动判断（llm.ModelSupportsVision）
	//   - true：显式声明支持图片
	//   - false：显式声明不支持图片（即使模型名命中了能力表）
	// 当当前模型不支持图片但输入包含图片时，Tachi 会调用配置中第一个
	// 支持图片的 provider 描述图片，再把描述文本喂给当前模型。
	Vision *bool `yaml:"vision,omitempty"`

	// MaxRetries 失败后的额外重试次数。nil = 使用默认（各 provider 路径均为
	// 2 次）；0 = 禁用重试；n = 重试 n 次。
	// 落地方式：openai（go-openai，无内置重试）走外层 RetryProvider；
	// anthropic / openai-res 走 SDK 内置重试（option.WithMaxRetries）。
	MaxRetries *int `yaml:"max_retries,omitempty"`

	// Timeout 单次 LLM API 请求超时。0 = 使用默认（各路径不设超时）。
	// 落地方式：openai（go-openai）设置 HTTP client 超时；
	// anthropic / openai-res 走 SDK 的 option.WithRequestTimeout。
	Timeout time.Duration `yaml:"timeout,omitempty"`

	// Proxy 该 provider 的 API 请求走哪个代理：
	//
	//	http://127.0.0.1:7890     HTTP 代理
	//	https://host:8443         HTTPS 代理（CONNECT 隧道）
	//	socks5://127.0.0.1:1080   SOCKS5 代理
	//
	// 空 = 不显式指定代理，走默认 transport 的环境变量（HTTP_PROXY /
	// HTTPS_PROXY / NO_PROXY）——需要全局代理时用环境变量即可，本字段是
	// "只有部分 provider 要走代理" 时的答案（境内直连、境外走代理）。
	// URL 无法解析或 scheme 不支持时 provider 解析直接失败（不静默直连）。
	Proxy string `yaml:"proxy,omitempty"`

	// Pricing 定价覆盖（可选，覆盖内置价格表）。类型定义在本包
	// （PricingConfig）——定价 schema 单一来源，config 只负责装载，
	// 定价语义（内置表/分时带）由 llm 解析。
	Pricing *PricingConfig `yaml:"pricing,omitempty"`

	// CreditRate 配置 credit 计算的速率（每 unitDivisor 个 token 的 credit
	// 单价，见 llm.ComputeCredit）。nil/未配置 = 0（不产生 credit）。
	// 计算时优先用账本行内记录的调用时刻快照；升级前的历史行没有快照，
	// 聚合展示时用此处配置的当前 rate 实时回算。
	CreditRate *float64 `yaml:"credit_rate,omitempty"`
}

type ProviderConfig struct {
	Name    string `yaml:"name"`
	Type    string `yaml:"type"`
	Model   string `yaml:"model"`
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`

	Spec ModelSpec `yaml:"spec"`
}

// Web search provider types. Referenced by WebSearchProviderConfig.Type.
const (
	WebSearchProviderExa   = "exa" // default provider
	WebSearchProviderBrave = "brave"
)

// Web fetch provider types. Referenced by WebFetchProviderConfig.Type.
// Native is the built-in fallback and never needs to be configured.
const (
	WebFetchProviderFirecrawl = "firecrawl" // requires an API key
	WebFetchProviderNative    = "native"    // built-in fallback
)

// WebSearchConfig configures the web search tool.
//
// Providers is a priority-ordered list: entries are tried front-to-back.
// When a provider's monthly credit/quota is exhausted (a clear error code:
// HTTP 402, or Brave's 429 with X-RateLimit-Remaining monthly remainder 0),
// it is marked paused in <BaseDir>/websearch_paused.json until the start of
// the next billing cycle (1st of next month) and searches fall back to the
// next provider. Transient rate limits (HTTP 429) only skip to the next
// provider without pausing. All of this is silent to the user (WARN logs
// only); the tool errors out only when every provider is unavailable.
type WebSearchConfig struct {
	Providers  []WebSearchProviderConfig `yaml:"providers"`
	Timeout    time.Duration             `yaml:"timeout" default:"30s"`
	MaxResults int                       `yaml:"max_results" default:"10"`
	Proxy      string                    `yaml:"proxy"` // Optional proxy URL (e.g. socks5://127.0.0.1:1080, http://127.0.0.1:8080)
}

// WebSearchProviderConfig is a single web search provider entry.
type WebSearchProviderConfig struct {
	Type    string `yaml:"type" default:"exa"` // WebSearchProviderExa (default) | WebSearchProviderBrave
	Key     string `yaml:"key"`                // provider API key
	BaseURL string `yaml:"base_url"`           // optional API base URL override (exa default: https://api.exa.ai)
}

// WebFetchConfig configures the web fetch tool.
//
// Providers is a priority-ordered list of API-keyed backends (e.g.
// firecrawl). The built-in native fetcher is always appended as the final
// fallback — it needs no configuration and never pauses. When a provider's
// quota/credits are exhausted (a clear error code: HTTP 402), it is marked
// paused in <BaseDir>/webfetch_paused.json until the start of the next
// billing cycle; transient rate limits (HTTP 429) only skip to the next
// backend without pausing. Fallbacks are silent to the user (WARN logs only).
type WebFetchConfig struct {
	Providers []WebFetchProviderConfig `yaml:"providers"`
	Timeout   time.Duration            `yaml:"timeout" default:"60s"` // HTTP request timeout
	Proxy     string                   `yaml:"proxy"`                 // Optional proxy URL (e.g. socks5://127.0.0.1:1080)
}

// WebFetchProviderConfig is a single web fetch provider entry.
type WebFetchProviderConfig struct {
	Type    string `yaml:"type" default:"firecrawl"` // WebFetchProviderFirecrawl (native always falls back, no config needed)
	Key     string `yaml:"key"`                      // provider API key
	BaseURL string `yaml:"base_url"`                 // optional API base URL override (firecrawl default: https://api.firecrawl.dev)
}

// MCPTransportType represents the type of MCP transport protocol
type MCPTransportType string

const (
	MCPTransportStdio MCPTransportType = "stdio"
	MCPTransportHTTP  MCPTransportType = "http"
)

// MCPOAuthConfig holds OAuth2 configuration for HTTP MCP servers.
// If ClientID is empty, dynamic client registration (DCR) is attempted
// automatically on the first OAuth flow.
type MCPOAuthConfig struct {
	ClientID              string   `json:"client_id,omitempty"`
	ClientSecret          string   `json:"client_secret,omitempty"`
	ClientURI             string   `json:"client_uri,omitempty"`
	Scopes                []string `json:"scopes,omitempty"`
	AuthServerMetadataURL string   `json:"auth_server_metadata_url,omitempty"`          // Override auto-discovery
	CallbackHost          string   `json:"callback_host,omitempty" default:"127.0.0.1"` // OAuth callback host
	CallbackPort          int      `json:"callback_port,omitempty"`                     // OAuth callback port (default: auto)
}

// MCPServerConfig represents a single MCP server connection configuration.
// Loaded from JSON files (mcp.json).
type MCPServerConfig struct {
	Name    string            `json:"name"`
	Type    MCPTransportType  `json:"type"` // "stdio" or "http"
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`                    // For http transport
	Headers map[string]string `json:"headers,omitempty"`                // For http transport
	Proxy   string            `json:"proxy,omitempty"`                  // Optional proxy URL (only for http transport; e.g. socks5://127.0.0.1:1080)
	Timeout Duration          `json:"timeout,omitempty"`                // Connect timeout (default: 10s)
	Enabled *bool             `json:"enabled,omitempty" default:"true"` // Whether to load this server
	OAuth   *MCPOAuthConfig   `json:"oauth,omitempty"`                  // OAuth2 configuration (http transport only)

	// ToolSearch-specific options
	AlwaysLoadTools []string          `json:"always_load_tools,omitempty"` // Tool names to always load (skip ToolSearch)
	SearchHints     map[string]string `json:"search_hints,omitempty"`      // Override search hints: tool_name -> hint
	Whitelist       []string          `json:"whitelist,omitempty"`         // If set, only these tools are loaded from the server; all others are ignored
	Blacklist       []string          `json:"blacklist,omitempty"`         // Tools to exclude. When both whitelist and blacklist are configured, blacklist filters from the whitelisted set (whitelist ∩ ¬blacklist)
	// Empty string means it came from a base file (always loaded).
	// Set internally during config loading; not serialized.
	Profile string `json:"-"`

	// SourceFile is the mcp.json this server's definition came from — the file that WON, when
	// several of them define the same name. Set while loading; not serialized.
	//
	// It exists so a change can be written back to the right place: Profile alone cannot tell
	// the global file from the project one, and writing a server into a file it did not come
	// from either shadows it (profile files override base files) or loses the change the next
	// time the user switches profile.
	SourceFile string `json:"-"`
}

// HasOAuth returns true if the server has OAuth2 configured.
func (srv *MCPServerConfig) HasOAuth() bool {
	return srv.OAuth != nil
}

// TokenStorageName returns the key used for on-disk token storage.
// For HTTP servers, the URL host is used so that servers behind the same
// gateway (same host) automatically share a single OAuth token.
// For stdio servers, the server name is used.
// Profile servers append the profile name to avoid cross-environment collisions.
func (srv *MCPServerConfig) TokenStorageName() string {
	base := srv.Name
	if srv.Type == MCPTransportHTTP && srv.URL != "" {
		if u, err := url.Parse(srv.URL); err == nil && u.Host != "" {
			// Sanitize host for use as filename (replace : with _)
			base = strings.ReplaceAll(u.Host, ":", "_")
		}
	}
	if srv.Profile == "" {
		return base
	}
	return base + "_" + srv.Profile
}

// IsEnabled returns whether the server should be loaded. Defaults to true.
func (srv *MCPServerConfig) IsEnabled() bool {
	return srv.Enabled == nil || *srv.Enabled
}

// TUIConfig 控制终端界面行为。
type TUIConfig struct {
	InputHistoryLimit int   `yaml:"input_history_limit" default:"10"`
	NotifyOnComplete  *bool `yaml:"notify_on_complete" default:"true"` // 是否在 LLM 回合结束后发送终端通知
}

// NotifyEnabled 返回是否启用了回合结束通知。默认启用。
func (t *TUIConfig) NotifyEnabled() bool {
	return t.NotifyOnComplete == nil || *t.NotifyOnComplete
}

type SystemReminderConfig struct {
	GitReminder *bool `yaml:"git_reminder" default:"true"` // set false to disable
}

// WeixinConfig holds iLink Bot channel configuration.
type WeixinConfig struct {
	Enabled  bool   `yaml:"enabled"`
	StateDir string `yaml:"state_dir"` // State directory (default: ~/.tachi/weixin)
	RouteTag string `yaml:"route_tag"` // Optional SKRouteTag for routing
	Greeting string `yaml:"greeting"`  // Startup greeting sent to the admin user after login (default: "👋 你好！Tachi 已启动，随时可以开始工作～")
	BotAgent string `yaml:"bot_agent"` // v2.3.1+: bot_agent identity string (default: "Tachi")
}

// NodeConfig holds this machine's node-service settings for multi-node
// collaboration (docs/2026-10-10-a2a-node-design.md).
//
// Disabled is the default, and it means NOTHING happens: no data directory is
// created, no lock is taken, no listener is opened. That is what lets every entry
// point keep behaving exactly as it did before the node existed, and it is asserted
// by tests rather than promised in a comment.
//
// Only the switch lives here. The data directory is derived
// (config.NodeDir), and the settings a node needs once it does something —
// listen addresses, peers, timeouts — arrive with the slices that use them: a
// config key with no behaviour behind it is a lie the reader cannot detect.
type NodeConfig struct {
	Enabled bool `yaml:"enabled"`

	// Workspace is where this node works: the working directory of every session a remote
	// task creates. It is deliberately NOT the hosting process's cwd (design §10.3) — a
	// node started from a shell must not do remote work in whatever directory that shell
	// happened to be sitting in, because nobody asked for that directory and the caller
	// cannot see it.
	//
	// "~" is expanded (see fileutil.ExpandHome) and the directory is created on first use.
	Workspace string `yaml:"workspace" default:"~/workspace/"`

	// Tools is the tool allowlist for remote tasks. It is an ALLOWLIST on purpose
	// (design §10.2): a remote caller gets exactly the tools the operator named. An empty
	// list means no tool that touches this machine — there is no hidden default here, because
	// a security setting whose empty value silently means something else is one nobody can
	// reason about. The recommended starting point (read-only) belongs in the example config
	// and in the docs, not in a fallback.
	//
	// AskUserQuestion is not subject to this list: asking the CALLER a question does nothing
	// to this machine, and it is added to whatever is allowed (node/runner.go toolViewOption).
	// So an empty list still means "the model is offered exactly one tool, the one that can
	// only talk to the caller".
	//
	// Note that a tool allowlist is not the same thing as a sandbox: an unrestricted Bash
	// is not "read-only" however few tools are listed next to it.
	Tools []string `yaml:"tools"`

	// QuestionTimeout bounds how long a question asked over the remote interface waits for
	// its answer before the task is ended (design §7.4: a parked turn needs a time limit).
	// Zero or negative means no deadline.
	//
	// The task is not left parked when this fires: INPUT_REQUIRED means "a turn in this
	// process is waiting for the answer", and after the deadline nobody is, so the task is
	// FAILED with the question kept in its status.
	QuestionTimeout time.Duration `yaml:"question_timeout" default:"30m"`

	// MaxParked bounds how many questions this node holds at once (design §7.4). A parked
	// question keeps a goroutine, a session and a task open, so the number is a limit on what
	// one caller — or several — can make this node hold.
	//
	// Reaching it does not refuse new WORK: the running turn is told its question could not be
	// asked and finishes with what it has.
	MaxParked int `yaml:"max_parked" default:"16"`

	// ContextRetention is how long a remote CONVERSATION stays valid after its last use, and
	// with it how long its session is kept (design §9.4). Within the window the conversation
	// can be continued even while no task is running; after it, a continuation is refused —
	// explicitly, never by quietly starting a new conversation — and the session becomes
	// ordinary again, so any count-based cleanup may reclaim it.
	//
	// The clock runs from the LAST use, so a conversation in daily use never expires and an
	// abandoned one ages out. Zero or negative means no window: the conversation stays valid
	// and its session stays protected until something releases it (only sensible for a node
	// whose data dir is cleaned by hand).
	ContextRetention time.Duration `yaml:"context_retention" default:"168h"`
}

// WorkspaceDir is where remote tasks work: the configured workspace with "~" expanded to
// an absolute path. The directory is NOT created here — config answering a question about
// a path should not have side effects, and the node creates it when it first needs it.
func (nc NodeConfig) WorkspaceDir() (string, error) {
	dir := strings.TrimSpace(nc.Workspace)
	if dir == "" {
		return "", errors.New("node: node.workspace is empty; a remote task needs a working directory to run in")
	}
	return filepath.Abs(fileutil.ExpandHome(dir))
}

// CronConfig holds cron scheduler configuration (only active in channel mode).
type CronConfig struct {
	Enabled          *bool         `yaml:"enabled" default:"true"` // whether to enable cron (default: true in channel mode)
	StorePath        string        `yaml:"store_path"`             // custom store path (default: ~/.tachi/crons.json)
	MaxConcurrent    int           `yaml:"max_concurrent" default:"3"`
	ExecutionTimeout time.Duration `yaml:"execution_timeout" default:"5m"`

	// NotifyMatcher picks how a notify=when_relevant job's result is judged:
	// "marker" = the agent's [SILENT] marker, matched by
	// cron.Job.ShouldSuppressResult (the original behaviour), "jev" = the System
	// One judgment gate, "auto" = the gate whenever Jev.Configured(), the marker
	// otherwise. The marker instruction STAYS in the prompt under every setting:
	// it is the gate's fallback when the service cannot be reached.
	NotifyMatcher string `yaml:"notify_matcher" default:"auto"`
	// JevThreshold is the noul probability at or above which a when_relevant
	// result is DELIVERED. It is not a fact about the gate and must not be
	// copied: p(action) is relative to the question and the case set it was
	// swept against. 0.45 sits in the middle of the corridor measured on
	// pkg/jev/testdata/notify-gate-prompt.json (37 scenarios, jev-1.13.0):
	// should-act ≥ 0.60, should-not-act ≤ 0.21. That case set's labels are one
	// person's judgement — re-sweep before trusting this number, and read
	// docs/agents/jev.md (Measuring a threshold) first.
	JevThreshold float64 `yaml:"jev_threshold" default:"0.45"`
}

// IsEnabled returns whether the cron scheduler is enabled. Defaults to true.
func (c *CronConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// DreamConfig holds configuration for AutoDream — the background memory
// consolidation system that runs via SystemScheduler in channel mode.
type DreamConfig struct {
	Enabled         bool          `yaml:"enabled" default:"false"`
	Schedule        string        `yaml:"schedule" default:"0 3 * * *"`     // cron expression (default: daily 3 AM)
	Provider        string        `yaml:"provider"`                         // provider name (empty → use main provider)
	MaxConcurrent   int           `yaml:"max_concurrent" default:"3"`       // max parallel dream sub-agents
	SubagentTimeout time.Duration `yaml:"subagent_timeout" default:"10m"`   // timeout for each dream sub-agent
	SubagentMaxIter int           `yaml:"subagent_max_iters" default:"30"`  // max iterations for dream sub-agent
	MaxMessageChars int           `yaml:"max_message_chars" default:"2000"` // max chars per message in dream prompt
}

// JevConfig configures the optional TypeSafe Jev integration: a System One
// model that returns typed judgments — a probability, a chosen option, a
// score on ordered levels — rather than generated text. It fits narrow
// semantic gates where the current alternative is asking a generative model
// for a short answer and parsing it out of the reply.
//
// It can NEVER stand in for a call whose output is shown or stored:
// generation stays on llm.Provider. pkg/jev documents the model's own
// constraints (the 32k state limit, silent failure on non-text input, why
// confidence is relative to the option set).
//
// Disabled by default, so an existing installation keeps its current path
// everywhere until a gate is deliberately moved over.
type JevConfig struct {
	Enabled bool `yaml:"enabled" default:"false"`
	// APIKey authenticates against the evaluation endpoint (Bearer).
	APIKey string `yaml:"api_key"`
	// Model pins the model version. Empty means pkg/jev's DefaultModel — a
	// concrete version, never the service's `jev-latest` alias. The alias
	// moves, and a threshold swept against the old weights silently stops
	// applying when it does, so leaving this blank is the safe direction.
	//
	// An endpoint that accepts ONLY an alias (a gateway reselling the service)
	// has to be given that alias here or every call fails with 400 — see
	// docs/agents/jev.md, "A pinned model", for what that costs a threshold.
	Model string `yaml:"model"`
	// BaseURL overrides the API host (empty = pkg/jev's DefaultBaseURL).
	BaseURL string `yaml:"base_url"`
	// Proxy routes the request through a local proxy (http://, https:// or
	// socks5:// — e.g. a clash on 127.0.0.1:7890). The endpoint is overseas,
	// so a machine without a system-wide proxy needs it. Empty leaves the
	// choice to the environment: the plain client httpx builds still honours
	// HTTP(S)_PROXY.
	Proxy string `yaml:"proxy"`
	// Timeout bounds one HTTP attempt. The tag carries DefaultJevTimeout, so the
	// field is never left at zero by a load — the same number pkg/jev.DefaultTimeout
	// gives a client built without a config. The client's retry loop can outlast
	// this several times over.
	Timeout time.Duration `yaml:"timeout" default:"10s"`
	// MaxQuestionsPerRequest bounds how many questions ONE request may carry.
	// This is a property of the SERVICE, not of any gate: a search over a large
	// deferred pool is judged in several sequential requests rather than one
	// huge one (agent/mcp/jev_search.go), and endpoints differ in how many
	// questions they accept — exceeding a cap is a request-level failure rather
	// than a partial answer.
	//
	// Zero (or a negative value) means "resolve it" — see QuestionsPerRequest,
	// which answers from BaseURL: the reference service takes
	// DefaultJevQuestionsPerRequest, and a known gateway takes its own measured
	// cap. There is no `default:` tag on purpose: a tag would write 100 into
	// every loaded config and the resolution below could never fire.
	//
	// Setting it costs round trips, not tokens: a search's state is the query
	// itself and the question texts are the same either way, so the total billed
	// input barely moves while the number of calls (and the wall time the
	// search's jev_budget has to cover) grows.
	MaxQuestionsPerRequest int `yaml:"max_questions_per_request"`
	// Pricing describes how this service bills, for the usage ledger's price
	// snapshot. It is config.PricingConfig — the same schema a provider's
	// `pricing:` block uses — because Jev is not a provider and has no
	// ProviderConfig to hang one on. Nil = the built-in table decides, which
	// prices a Jev call on its own (llm/builtin_models.go carries a Jev record,
	// quoted in USD and converted at the configured usd_to_cny), so this block
	// is the override rather than the source. The reader is the tool-search gate,
	// which resolves each ledger row through llm.ResolveModelPriceFrom as it
	// writes it (agent/mcp/jev_search.go) — set a field here and the next
	// call's row carries it.
	//
	// Note what a ledger row means here: this service bills INPUT only, so a
	// priced row carries input_price > 0 and output_price = 0. That zero is
	// "the output side is free", not "nobody filled it in" — UsageRow.Unpriced
	// needs BOTH fields zero, so the row still counts as priced.
	Pricing *PricingConfig `yaml:"pricing,omitempty"`
	// CreditRate is the credit rate for Jev calls (see llm.ComputeCredit).
	// nil = 0. It pairs with Pricing for the same reason ModelSpec pairs its
	// two: the ledger always writes a credit snapshot, and a rate left unset
	// would make every Jev row a silent 0 rather than a decision. The gate
	// above reads it per row, the same way it reads Pricing.
	CreditRate *float64 `yaml:"credit_rate,omitempty"`
}

// Configured reports whether the integration can actually run: switched on
// AND holding a key. Callers gate on this, so a half-filled section degrades
// to the existing path instead of failing per call.
func (c JevConfig) Configured() bool {
	return c.Enabled && strings.TrimSpace(c.APIKey) != ""
}

// jevGatewayQuestionCaps is the per-request question cap of each known GATEWAY
// host, for the case where MaxQuestionsPerRequest is left unset. It exists
// because the cap is a property of the endpoint, not of a gate, and a user who
// points `base_url` at one of these has no way to guess the number — the
// failure it prevents is not a wrong answer but a whole search degrading to the
// keyword path, since the service answers HTTP 400 to an over-long request
// (measured, not documented: `{"error":{"message":"at most 20 questions per
// call"}}`, one question over the cap, and the status is not retryable).
//
// Keyed by HOST, not by a substring of the URL: the path is the deployment's
// business and the host is what identifies the service. A gateway whose cap
// moves, or a new one, is one entry here — and the field above still wins over
// the table, so an explicit number always decides.
var jevGatewayQuestionCaps = map[string]int{
	"api.commandcode.ai": 20,
}

// QuestionsPerRequest is how many questions ONE request may carry against this
// endpoint: the configured value when set, else the known gateway's cap, else
// the reference service's default. It is a method rather than a field default
// because the answer depends on BaseURL, which the yaml `default:` tag cannot
// see.
func (c JevConfig) QuestionsPerRequest() int {
	if c.MaxQuestionsPerRequest > 0 {
		return c.MaxQuestionsPerRequest
	}
	if n, ok := jevGatewayQuestionCaps[hostOfURL(c.BaseURL)]; ok {
		return n
	}
	return DefaultJevQuestionsPerRequest
}

// hostOfURL lower-cases the host of a base URL, tolerating one written without
// a scheme (`api.example.com/v1`), which url.Parse would otherwise read as a
// path. An unparseable or empty value is "" and matches no gateway, i.e. the
// reference default applies — the safe direction, since it is the larger cap and
// the only cost of being wrong about it is round trips.
func hostOfURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "//" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// ACPConfig holds configuration for the ACP (Agent Client Protocol) agent mode.
type ACPConfig struct {
	// ConnectConfiguredMCP controls whether to connect MCP servers from mcp.json
	// when a new ACP session starts. Defaults to true.
	ConnectConfiguredMCP *bool `yaml:"connect_configured_mcp"`
	// MCPConflictPolicy determines what happens when an editor sends an MCP server
	// with the same name as one in mcp.json. "client_wins" (default) uses the
	// editor's version; "agent_wins" keeps the mcp.json version.
	MCPConflictPolicy string `yaml:"mcp_conflict_policy" default:"client_wins"`
}

// CompactConfig holds configuration for session compaction.
// Used by both the /compact command and auto-compact (when enabled).
type CompactConfig struct {
	Timeout   time.Duration `yaml:"timeout" default:"5m"`      // Timeout for the compaction LLM call
	MaxTokens int           `yaml:"max_tokens" default:"4096"` // Max tokens for the compact response (summary)
	Auto      *bool         `yaml:"auto" default:"true"`       // Enable automatic compaction when context is near limit
	Threshold float64       `yaml:"threshold" default:"0.8"`   // Trigger ratio: lastInputTokens / contextWindow >= threshold
}

// CheckpointConfig holds configuration for session checkpoints: a per-turn file
// snapshot (a private bare git repository per workspace root, under the session
// directory) that a rewind can restore, together with the conversation.
//
// The guards are safety valves rather than tuned limits — they are what keeps a
// pathological repository from making every turn slow or the snapshot store
// enormous. A tripped guard records the turn's boundary without file state, and
// a rewind to it says so instead of pretending it restored anything.
type CheckpointConfig struct {
	// Enabled defaults to true for a loaded config (see the tag). A config built
	// in code — tests, and anything constructing one by hand — leaves it nil,
	// which means OFF. That is deliberately the opposite of the Compact.Auto
	// idiom, where nil means on: checkpointing writes to disk on every turn, so
	// the default has to keep an unconfigured agent from creating a snapshot
	// store the caller did not ask for.
	Enabled  *bool `yaml:"enabled" default:"true"`       // capture a file snapshot per turn
	MaxFiles int   `yaml:"max_files" default:"50000"`    // refuse a root with more covered files than this
	MaxBytes int64 `yaml:"max_bytes" default:"33554432"` // refuse a single changed file bigger than this (32MB)
	Retain   int   `yaml:"retain" default:"100"`         // keep this many checkpoints per session
}

// ToolResultConfig holds configuration for tool result size limits and
// file persistence of oversized results. When a tool result exceeds
// MaxChars, the full output is saved to disk and a truncated preview
// with file path is returned to the LLM instead.
type ToolResultConfig struct {
	MaxChars int    `yaml:"max_chars"` // max chars for tool result passed to LLM (default 50000, 0 = no limit)
	FileDir  string `yaml:"file_dir"`  // dir for storing oversized results (default: ~/.tachi/tool_results)
}

// MaxResultChars returns the effective max chars threshold. Falls back to
// defaultToolResultMaxChars (50000) when unconfigured. Returns 0 only when
// explicitly set to 0 (which means no limit).
func (c *ToolResultConfig) MaxResultChars() int {
	if c.MaxChars == 0 && c.FileDir == "" {
		return defaultToolResultMaxChars
	}
	return c.MaxChars
}

// ResultFileDir returns the effective directory for storing oversized results.
// Falls back to ~/.tachi/tool_results when unconfigured.
func (c *ToolResultConfig) ResultFileDir() string {
	if c.FileDir != "" {
		return c.FileDir
	}
	return ToolResultsDir()
}

// SubagentConfig holds configuration for sub-agent execution.
// When Provider is empty, the main provider is used.
type SubagentConfig struct {
	Provider       string `yaml:"provider"`         // provider name, empty → use main
	MaxIterations  int    `yaml:"max_iterations"`   // default: 200 (hardcoded fallback)
	MaxConcurrency int    `yaml:"max_concurrency"`  // default: 10 (hardcoded fallback)
	MaxOutputChars int    `yaml:"max_output_chars"` // default: 16384 (hardcoded fallback)
	Thinking       bool   `yaml:"thinking"`         // default: false

	// Worktree enables git worktree isolation for sub-agents (default: false).
	Worktree        bool   `yaml:"worktree"`
	WorktreeDir     string `yaml:"worktree_dir"`     // worktree storage directory (default: os.TempDir())
	WorktreeCleanup *bool  `yaml:"worktree_cleanup"` // clean up after completion (default: true)
	WorktreeBranch  string `yaml:"worktree_branch"`  // default branch for worktree checkout (empty = detached HEAD)
}

// AdversarialReviewConfig holds the per-round model configuration for the
// adversarial (multi-round) /review mode. Rounds are NOT configured here —
// multi-round review is entered explicitly via "/review N" (N ≥ 2); no
// argument means the single-round path. See
// docs/2026-07-30-adversarial-review-design.md.
//
// NOTE (creasty/defaults behavior): Adversarial is a *pointer* without a
// default tag, so defaults.Set() does NOT allocate it when the YAML omits the
// `adversarial:` key — callers must check `adv != nil` before touching
// Models/JudgeModel (SetupAdversarialProviders and CheckAdversarialProviders
// both gate on it). Old configs that still carry the removed `enabled:` /
// `rounds:` keys load fine: yaml.Unmarshal is non-strict and ignores unknown
// keys.
type AdversarialReviewConfig struct {
	Models     []string `yaml:"models"`      // per-round model names (empty → all rounds use review.provider)
	JudgeModel string   `yaml:"judge_model"` // fixed model for the final round (empty → modulo assignment)
}

// ReviewConfig holds configuration for the /review code review command.
// When Provider is empty, the main provider is used.
// AllowedTools defaults to [Bash, ReadFile, WriteFile, Glob, Grep] when empty (handled in code).
//
// Thinking / ThinkingLevel both default to "follow the current session":
// nil / empty mean the review rounds inherit the session's thinking switch
// and effort (which in turn falls back to the provider/model default when
// the session has no override). Setting either field pins that dimension.
type ReviewConfig struct {
	Provider      string                   `yaml:"provider"`                     // provider name, empty → use main
	MaxIterations int                      `yaml:"max_iterations" default:"200"` // iteration budget
	AllowedTools  []string                 `yaml:"allowed_tools"`                // default: [Bash, ReadFile, Glob, Grep] (code-level fallback)
	Thinking      *bool                    `yaml:"thinking"`                     // thinking switch override (nil = follow current session)
	ThinkingLevel string                   `yaml:"thinking_level"`               // thinking effort override, /thinking level values ("" = follow current session)
	Adversarial   *AdversarialReviewConfig `yaml:"adversarial"`                  // multi-round mode (nil when unconfigured, see note above)
}

// ChannelConfig groups configuration for all IM channel backends.
//
// Legacy field: Weixin is a typed config for a built-in channel.
// For backward compatibility, if Weixin.Enabled is set
// but no matching entry exists in Channels, it's auto-activated.
//
// Generic field: Channels maps channel name to its raw config. Each entry
// must match the Name() of a registered Channel (via channel.Register).
// Private channel repositories can drop a single Go file with:
//
//	import _ "private-repo/tachi-channel-mybots"
//
// and then activate it via config.yaml:
//
//	channel:
//	  channels:
//	    mybots:
//	      enabled: true
//	      token: "xxx"
type ChannelConfig struct {
	Weixin   WeixinConfig              `yaml:"weixin"`
	Channels map[string]map[string]any `yaml:"channels"`
	Whisper  ChannelWhisperConfig      `yaml:"whisper"`
}

// ChannelWhisperConfig holds channel-mode whisper settings for group chat
// selective reply. When enabled, non-directed messages in group chats are
// batched and presented to the agent as ambient context rather than
// triggering individual agent turns.
type ChannelWhisperConfig struct {
	// Enabled controls whether the whisper pipeline is active (default: true).
	// Use pointer so false (explicit disable) can be distinguished from unset.
	Enabled *bool `yaml:"enabled"`

	// AmbientBatchWindow is the duration to buffer non-directed messages
	// before triggering an ambient turn (default: 30s).
	AmbientBatchWindow time.Duration `yaml:"ambient_batch_window" default:"30s"`

	// AmbientMaxIterations is the iteration budget for ambient turns (default: 5).
	AmbientMaxIterations int `yaml:"ambient_max_iterations" default:"5"`

	// AmbientMaxBuffer is the maximum number of messages buffered per thread.
	// When exceeded, oldest messages are dropped (FIFO). Default: 10.
	AmbientMaxBuffer int `yaml:"ambient_max_buffer" default:"10"`

	// AmbientMaxHistory is the maximum number of ambient conversation entries
	// retained in memory for cross-turn context. Oldest entries are dropped
	// (FIFO) when exceeded. Default: 50.
	AmbientMaxHistory int `yaml:"ambient_max_history" default:"50"`

	// AmbientCooldown is the minimum interval between two ambient turns
	// on the same thread (default: 0, no cooldown).
	AmbientCooldown time.Duration `yaml:"ambient_cooldown"`

	// SilenceMarker is the string the agent replies with to indicate
	// it has nothing to say. Matching is lenient (trim + case-insensitive).
	// Default: "[SILENT]".
	SilenceMarker string `yaml:"silence_marker" default:"[SILENT]"`

	// AmbientTools is the tool whitelist for ambient turns.
	// Empty (default) = [MemoryRecall, RecordMemory, WebFetch, WebSearch].
	AmbientTools []string `yaml:"ambient_tools"`

	// AmbientMaxTokens is the max_tokens budget for ambient turns.
	// Default: falls back to agent.DefaultMaxTokens.
	AmbientMaxTokens int `yaml:"ambient_max_tokens"`
}

// WhisperEnabled returns whether whisper is enabled, defaulting to true if unset.
func (c *ChannelWhisperConfig) WhisperEnabled() bool {
	if c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

// ActiveChannels returns the raw configs for every enabled channel,
// merging the legacy typed Weixin config into the generic Channels
// map when needed.
//
// For backward compatibility: if the legacy Weixin typed config has
// Enabled set and there's no matching key in Channels, it's auto-included.
// If both exist, the generic Channels entry takes precedence.
func (cc *ChannelConfig) ActiveChannels() map[string]map[string]any {
	result := make(map[string]map[string]any, len(cc.Channels))

	// Copy generic channels that are enabled.
	for name, rawCfg := range cc.Channels {
		if isEnabled, ok := rawCfg["enabled"]; !ok || !toBool(isEnabled) {
			continue
		}
		result[name] = rawCfg
	}

	// Legacy weixin: include if enabled and not already present in Channels.
	if _, inChannels := result["weixin"]; !inChannels && cc.Weixin.Enabled {
		result["weixin"] = map[string]any{
			"enabled":   true,
			"state_dir": cc.Weixin.StateDir,
			"route_tag": cc.Weixin.RouteTag,
			"greeting":  cc.Weixin.Greeting,
		}
	}

	return result
}

// toBool converts a YAML-parsed value to a boolean. Handles both actual
// booleans (from yaml.v3) and numeric representations.
func toBool(v any) bool {
	switch val := v.(type) {
	case bool:
		return val
	case int:
		return val != 0
	case float64:
		return val != 0
	default:
		return false
	}
}

// MemoryConfig holds configuration for the memory system.
// Type selects the backend: "" (disabled) or "topic".
type MemoryConfig struct {
	Type              string        `yaml:"type"`                             // "topic" or "" (disabled)
	KeywordProvider   string        `yaml:"keyword_provider"`                 // optional: provider name for keyword extraction (defaults to main provider)
	Timeout           time.Duration `yaml:"timeout" default:"10s"`            // context deadline for Store/Recall/Forget
	RecallLimit       int           `yaml:"recall_limit" default:"5"`         // max memories recalled per turn by automatic MemoryRecallReminder
	DecayHalfLifeDays int           `yaml:"decay_half_life_days" default:"7"` // decay half-life in days for TopicBackend (default 7)
	BaseDir           string        `yaml:"-"`                                // runtime-only: state base dir, injected by the agent (not serialized)
}

// Default prompts for DeepResearch (built-in fallbacks when YAML doesn't specify).
const defaultQueryGeneratorPrompt = `You are a research query generator. Given a research topic and existing learnings, generate {breadth} specific, non-overlapping search engine queries. Each query should target a distinct aspect of the topic.

Research topic: {query}
Existing learnings: {learnings}

Generate {breadth} search queries with a "researchGoal" for each explaining what this query aims to discover.

Return your response as a JSON array of objects with "query" and "researchGoal" fields. Only return the JSON array, no other text.`

const defaultResearcherPrompt = `You are a research analyst. Your task:
1. Search for: "{query}"
2. Read the search results and linked pages
3. Extract key learnings (factual, detailed, with specific metrics and entities)
4. Suggest follow-up questions for deeper research

Research goal: {researchGoal}

Return your findings as a structured summary with:
- Key learnings (up to 3, concise and information-dense)
- Follow-up questions (up to 3, for deeper research)
- Source URLs visited

Output your findings in {language}.
`

const defaultReportWriterPrompt = `You are a research report writer. Write a comprehensive, well-structured report as a self-contained HTML document.

Research topic: {query}

Findings:
{learnings}

Source URLs:
{urls}

Create a beautiful, readable HTML page with:
- Modern, clean CSS design (embedded in a <style> tag)
- Good typography, readable font sizes and line spacing
- Professional color scheme with proper contrast
- Well-organized sections with clear heading hierarchy
- Code blocks with monospace font and subtle background if needed
- Links to sources properly formatted
- Responsive design that works on both desktop and mobile
- A table of contents at the top for navigation

The HTML should be a complete, valid HTML5 document with <!DOCTYPE html>, <html>, <head>, and <body> tags.
Include all CSS inline in a <style> tag within <head>. Do NOT use external CSS or JavaScript.
Write the report in {language}.

Output the complete HTML document as your final response. The report file will be saved by the system to: {output_path}
Do NOT call any tools. Return the full HTML content only, without markdown code fences.`

type DeepResearchConfig struct {
	DefaultDepth   int           `yaml:"default_depth" default:"2"`
	DefaultBreadth int           `yaml:"default_breadth" default:"3"`
	MaxDepth       int           `yaml:"max_depth" default:"4"`
	MaxBreadth     int           `yaml:"max_breadth" default:"8"`
	Timeout        time.Duration `yaml:"timeout" default:"30m"`
	ReportTimeout  time.Duration `yaml:"report_timeout" default:"10m"`
	MaxLearnings   int           `yaml:"max_learnings" default:"200"`
	ReportLanguage string        `yaml:"language" default:"zh"`

	// QueryGeneratorProvider references a provider name from config's providers list.
	// When empty, the main (default) provider is used.
	QueryGeneratorProvider string `yaml:"query_generator_provider"`

	// Prompts contains all customizable prompt templates. When nil or empty string,
	// built-in Go defaults are used.
	Prompts *DeepResearchPrompts `yaml:"prompts,omitempty"`

	// Researcher controls how research sub-agents behave.
	Researcher *ResearcherConfig `yaml:"researcher,omitempty"`
}

// Language returns the report language, defaulting to "zh" when not set.
func (c *DeepResearchConfig) Language() string {
	if c.ReportLanguage != "" {
		return c.ReportLanguage
	}
	return "zh"
}

// QueryGeneratorPrompt returns the query generator prompt template, using
// the built-in default when config doesn't specify one.
func (c *DeepResearchConfig) QueryGeneratorPrompt() string {
	if c.Prompts != nil && c.Prompts.QueryGenerator != "" {
		return c.Prompts.QueryGenerator
	}
	return defaultQueryGeneratorPrompt
}

// ResearcherPrompt returns the researcher sub-agent prompt template, using
// the built-in default when config doesn't specify one.
func (c *DeepResearchConfig) ResearcherPrompt() string {
	if c.Prompts != nil && c.Prompts.Researcher != "" {
		return c.Prompts.Researcher
	}
	return defaultResearcherPrompt
}

// ReportWriterPrompt returns the report writer prompt template, using
// the built-in default when config doesn't specify one.
func (c *DeepResearchConfig) ReportWriterPrompt() string {
	if c.Prompts != nil && c.Prompts.ReportWriter != "" {
		return c.Prompts.ReportWriter
	}
	return defaultReportWriterPrompt
}

// ResearcherTools returns the allowed tools for research sub-agents.
func (c *DeepResearchConfig) ResearcherTools() []string {
	if c.Researcher != nil && len(c.Researcher.AllowedTools) > 0 {
		return c.Researcher.AllowedTools
	}
	return []string{"WebSearch", "WebFetch", "ReadFile", "Grep", "WriteFile"}
}

type DeepResearchPrompts struct {
	QueryGenerator string `yaml:"query_generator,omitempty"`
	Researcher     string `yaml:"researcher,omitempty"`
	ReportWriter   string `yaml:"report_writer,omitempty"`
}

type ResearcherConfig struct {
	AllowedTools  []string `yaml:"allowed_tools"`
	MaxIterations int      `yaml:"max_iterations" default:"5"`
}

// LogsConfig is a type alias for logger.Config, so that YAML tags and defaults
// are defined in one place (pkg/logger) and shared by the config package.
type LogsConfig = logger.Config

// DebugConfig holds debug/observability settings (pprof, etc.).
type DebugConfig struct {
	PPROF PprofConfig `yaml:"pprof"`
}

// PprofConfig controls the Go pprof HTTP server for live profiling.
// When enabled, pprof endpoints are served on 127.0.0.1:<port>:
//
//	/debug/pprof/          — index
//	/debug/pprof/profile   — CPU profile
//	/debug/pprof/heap      — heap profile
//	/debug/pprof/goroutine — goroutine dump
//	...and more
type PprofConfig struct {
	Enabled bool `yaml:"enabled" default:"false"`
	Port    int  `yaml:"port" default:"6060"`
}

// Addr returns the listen address ("127.0.0.1:<port>") if pprof is enabled,
// or empty string otherwise.
func (c PprofConfig) Addr() string {
	if !c.Enabled || c.Port == 0 {
		return ""
	}
	return fmt.Sprintf("127.0.0.1:%d", c.Port)
}

type Config struct {
	Provider               string               `yaml:"provider"`
	MaxTokens              int                  `yaml:"max_tokens" default:"128000"`
	MaxIterations          *int                 `yaml:"max_iterations" default:"50"`             // nil = default; 0 = unlimited; >0 = explicit limit
	SessionCleanupMaxCount int                  `yaml:"session_cleanup_max_count" default:"100"` // max sessions to retain
	Providers              []ProviderConfig     `yaml:"providers"`
	WebSearch              WebSearchConfig      `yaml:"web_search"`
	WebFetch               WebFetchConfig       `yaml:"web_fetch"`
	MCPServers             []MCPServerConfig    `yaml:"-"`                  // Loaded from JSON files via LoadMCPServers(); not in YAML
	ActiveMCPProfile       string               `yaml:"active_mcp_profile"` // Which profile to load (empty = none)
	MCPToolSearch          MCPToolSearchConfig  `yaml:"mcp_tool_search"`
	MCPToolRefresh         MCPToolRefreshConfig `yaml:"mcp_tool_refresh"`
	TUI                    TUIConfig            `yaml:"tui"`
	SystemReminder         SystemReminderConfig `yaml:"system_reminder"`
	Language               string               `yaml:"language" default:"English"`      // Reply language for LLM
	ExtraSystemPrompt      string               `yaml:"extra_system_prompt,omitempty"`   // Optional user-supplied system prompt content; when set, appended to the end of the system prompt
	TitleGeneration        *bool                `yaml:"title_generation" default:"true"` // set false to use truncation
	TitleProvider          string               `yaml:"title_provider"`                  // optional: provider name for title generation (defaults to main provider)
	CommitProvider         string               `yaml:"commit_provider"`                 // optional: provider name for /commit (defaults to main provider)
	RunProvider            string               `yaml:"run_provider"`                    // optional: provider name for tachi -p run mode (defaults to main provider)
	ProviderAliases        map[string]string    `yaml:"provider_aliases"`                // alias name → actual provider name
	Memory                 MemoryConfig         `yaml:"memory"`                          // pluggable memory backend
	Channel                ChannelConfig        `yaml:"channel"`                         // IM channel backends
	Subagent               SubagentConfig       `yaml:"subagent"`                        // Sub-agent configuration
	Review                 ReviewConfig         `yaml:"review"`                          // /review code review configuration
	Compact                CompactConfig        `yaml:"compact"`                         // /compact command configuration
	Checkpoints            CheckpointConfig     `yaml:"checkpoints"`                     // per-turn file snapshots for /rewind
	ToolResult             ToolResultConfig     `yaml:"tool_result"`                     // tool result size limits and file persistence
	Cron                   CronConfig           `yaml:"cron"`                            // Cron scheduler (channel mode)
	Dream                  DreamConfig          `yaml:"dream"`                           // AutoDream memory consolidation (channel mode)
	ACP                    ACPConfig            `yaml:"acp"`                             // ACP agent configuration
	DeepResearch           DeepResearchConfig   `yaml:"deep_research"`                   // Deep Research engine configuration
	Jev                    JevConfig            `yaml:"jev"`                             // TypeSafe Jev semantic judgments (optional)
	Logs                   LogsConfig           `yaml:"logs"`                            // Logger configuration
	Permissions            PermissionsConfig    `yaml:"permissions"`                     // Tool permission rules (bash allow/ask/deny)
	Oneoff                 OneoffConfig         `yaml:"oneoff"`                          // One-off transcript recording (/commit, /review, ambient, dream...)
	Hooks                  HooksConfig          `yaml:"hooks"`                           // Event hook system (user-defined commands)
	Herdr                  HerdrConfig          `yaml:"herdr"`                           // Herdr terminal multiplexer integration
	Debug                  DebugConfig          `yaml:"debug"`                           // Debug/observability settings (pprof, etc.)
	Web                    WebConfig            `yaml:"web"`                             // Web console (tachi web)
	Badge                  BadgeConfig          `yaml:"badge"`                           // Device link (AI Passport: a small screen for parked asks)
	Node                   NodeConfig           `yaml:"node"`                            // Node service for multi-node collaboration

	// USDToCNY is the rate the built-in price table uses for the vendors that
	// publish in USD (OpenAI's gpt-6 family, TypeSafe/Jev): their records quote
	// USD per 1M tokens and the resolver converts, because the ledger's unit is
	// CNY (see llm/builtin_models.go, builtinModel.usdPrices).
	//
	// It is a config rather than a constant because the number is not a fact
	// about the models — it is a snapshot of a moving exchange rate, and a bill
	// read months later was converted at whatever rate was configured then. nil
	// or <= 0 = llm.DefaultUSDToCNY.
	//
	// A row keeps the price it was RESOLVED with (the ledger snapshots it), so
	// changing this reprices only the calls made after the change.
	USDToCNY *float64 `yaml:"usd_to_cny,omitempty"`
}

// WebConfig controls the local web console (`tachi web`): a read-only
// dashboard over sessions, one-off transcripts and the usage ledger.
type WebConfig struct {
	// Addr is the listen address (default: 127.0.0.1:8787).
	Addr string `yaml:"addr" default:"127.0.0.1:8787"`
	// APIKey enables auth for /api/*: when non-empty, every request must
	// carry the matching X-Api-Key header (constant-time compared).
	// Empty = local development mode, no auth required.
	APIKey string `yaml:"api_key"`
	// AllowedOrigins enables CORS for the listed origins (dev mode when the
	// frontend runs on the Vite dev server; empty = same-origin only).
	AllowedOrigins []string `yaml:"allowed_origins"`
}

// BadgeConfig controls the device link (see desktop/badge.go, and
// docs/2026-09-28-agent-badge-link-design.md): a small screen — an AI Passport board, an
// ESP32-C3 with three buttons — that shows what the agent is WAITING ON (a bash ask, an
// AskUserQuestion) and answers it from the device.
//
// The key lives here because every frontend shares one config; only the desktop reads it.
type BadgeConfig struct {
	// Transport is how the device is reached: "off" (or empty), "serial", or "tcp".
	//
	// OFF is the default on purpose. The serial port is a shared resource, and an app that
	// grabs it at startup makes `idf.py monitor` fail for reasons that have nothing to do
	// with the board. The TACHI_BADGE environment variable overrides this key.
	Transport string `yaml:"transport"`
	// Address is the transport's own address. serial: the port path, empty meaning find
	// the board by USB vendor id (the name — /dev/cu.usbmodem2101 — moves between
	// plug-ins). tcp: the listen address, e.g. "127.0.0.1:8765".
	Address string `yaml:"address"`
	// Alert tells the device to play a sound when something starts waiting for you
	// (a permission or a question) or when a turn finishes — but only while you are NOT
	// watching it (the window is in the background, or the session is not the active one;
	// see desktop/asks.go's alertWanted). nil is the default, which is ON.
	Alert *bool `yaml:"alert"`
}

// AlertsEnabled reports whether the device should be told to play its notification sound.
// An unset value reads as the default (on) — the same nil-means-default rule the rest of
// the config uses.
func (b BadgeConfig) AlertsEnabled() bool { return b.Alert == nil || *b.Alert }

// OneoffConfig controls one-off transcript recording: sidecar JSONL files
// capturing the full execution of side-channel LLM runs (/commit, /review,
// channel ambient, dream, github bot) without touching the main session
// history. See docs/2026-07-24-oneoff-transcript-design.md.
type OneoffConfig struct {
	Enabled       *bool `yaml:"enabled" default:"true"`      // master switch; false = restore old behavior (no recording)
	RetentionDays int   `yaml:"retention_days" default:"30"` // days to keep files under the global <home>/oneoff/ dir
}

// IsEnabled reports whether one-off transcript recording is on (default true).
func (c *OneoffConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// HooksConfig controls the event hook system. Events fire during the agent
// loop and can trigger external commands (user-defined scripts) or built-in
// Go callbacks (e.g. Herdr integration).
type HooksConfig struct {
	Enabled *bool                    `yaml:"enabled" default:"true"`
	Events  map[string][]HookCommand `yaml:"events"` // event name → commands
}

// IsEnabled reports whether the hook system is active (default true).
func (c *HooksConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// HookCommand defines an external command to run when an event fires.
// The command receives the event payload as JSON on stdin.
type HookCommand struct {
	Command string            `yaml:"command"`           // e.g. "bash {{HOOKS_DIR}}/notify.sh"
	Timeout string            `yaml:"timeout,omitempty"` // e.g. "5s", "3s"; default "5s"
	Async   *bool             `yaml:"async,omitempty"`   // default true; false = wait for completion
	Env     map[string]string `yaml:"env,omitempty"`     // extra env vars
}

// HerdrConfig controls the Herdr terminal multiplexer integration.
// When enabled and HERDR_ENV=1 is detected, Tachi automatically reports
// session identity and lifecycle state to the local Herdr server.
// Socket path and pane ID are always read from environment variables
// (HERDR_SOCKET_PATH, HERDR_PANE_ID).
type HerdrConfig struct {
	Enabled *bool `yaml:"enabled" default:"true"` // auto-detect from HERDR_ENV
}

// IsEnabled reports whether Herdr integration is allowed (default true).
func (c *HerdrConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// BashPermissions holds glob rules classifying bash commands.
// Only the '*' wildcard is supported. Per-command-segment precedence:
// deny > allow > ask > default(allow).
type BashPermissions struct {
	Deny []string `yaml:"deny"` // blocked outright; error returned to the LLM
	Ask  []string `yaml:"ask"`  // requires approval: a frontend with a human asks; an unattended mode runs it
	// Allow exempts matching segments from the ask rules — and, on the same
	// path, from the judgment gate (permissions.bash.jev): a command covered by
	// an allow rule is run without being judged, so this is also how a user
	// turns the gate off for one shape of command. Deny is still decided first,
	// so an allow rule can never widen into running something forbidden.
	Allow []string `yaml:"allow"`
	// DisableBuiltinDeny turns off the built-in absolutely-dangerous deny
	// rules (permission.BuiltinDenyRules). Only honored from the GLOBAL
	// config — the same key in a project .tachi/permissions.yaml is ignored,
	// so a cloned repo cannot weaken the user's safety defaults.
	DisableBuiltinDeny bool `yaml:"disable_builtin_deny"`

	// Jev turns the judgment gate on: every command that is not DENIED is
	// judged, and one the judgment says a human should see is sent to the
	// frontend's ask path instead of being run. "auto" (the default, and the
	// same shape as mcp_tool_search.matcher / cron.notify_matcher: the gate runs
	// whenever the top-level `jev:` block is usable) or "off" (rules decide
	// alone, and a command nothing matched just runs).
	// See docs/2026-09-30-bash-ask-jev-design.md.
	//
	// It never widens what may run: deny is decided before the gate is reached,
	// and the judgment's only two outcomes are "run" and "ask the human".
	Jev string `yaml:"jev" default:"auto"`

	// JevThreshold is the noul probability at or above which the judgment is
	// read as "a human should see this". It belongs to the case set it was
	// swept on, NOT to this field: the numbers below were measured on
	// pkg/jev/testdata/bash-ask.json — 68 authored commands, 5 repeats, state and
	// question rendered by agent/permission/jevask — through a gateway serving
	// `typesafe/jev`, which is an ALIAS this endpoint accepts instead of a version:
	// the same sweep is what a pinned model would be measured with, so this τ is a
	// hypothesis about the weights rather than a calibration of them (see
	// docs/agents/jev.md, "A pinned model").
	//
	// The window is (0.62, 0.70): the highest ROUTINE command scored 0.62
	// (`rm -rf ./node_modules && npm ci` — it fetches from the network, so the
	// criteria read it as running code they did not write) and the lowest
	// needs-a-look command 0.70 (`docker compose down -v`). One needs-a-look case
	// scores BELOW that window and is knowingly missed: `cat ~/.tachi/config.yaml`
	// (0.60), because the model reads a settings file as ordinary text even when
	// the criteria say it carries credentials — the previous question missed it
	// too, and a `~/.tachi/*` ask rule is the deterministic answer.
	//
	// τ = 0.66 is the middle of that window, biased a little toward asking (the
	// direction that costs an interruption rather than a missed one). What the
	// question it belongs to is about: whether the command DOES something worth a
	// look — reads credentials, changes shared state, rewrites history, runs code
	// fetched from the network, deletes unreproducible work, or walks a whole
	// filesystem and looks like a hang. It deliberately does NOT ask about where a
	// command reaches: the scope axis was measured, found to be the whole reason
	// this gate was noisy on real traffic, and removed (agent/permission/jevask's
	// package doc).
	//
	// Re-sweep when the model version moves, when that case set CHANGES, or when
	// the STATE SHAPE does (a sentence in jevask is part of the question a
	// probability was measured against), or when the log shows auto-allowed
	// commands the user later wrote a deny rule for. A number copied from
	// anywhere else is not a measurement of THIS question.
	//
	// A value of 1.0 is the shadow setting: the gate judges and logs every
	// command but can never act (a noul answer is at most 1.0), so the
	// distribution can be watched on real traffic before anything changes.
	JevThreshold float64 `yaml:"jev_threshold" default:"0.66"`

	// JevBudget bounds one judgment's wait. 3s is shorter than the MCP
	// search's 6s on purpose: that gate is a search the model asked for and is
	// worth waiting on, while this one sits in front of EVERY command, so a
	// slow answer is a slow turn. Exceeding it falls back to the rules.
	JevBudget time.Duration `yaml:"jev_budget" default:"3s"`
}

// PathPermissions holds glob rules classifying the TARGET PATH of one file
// tool (ReadFile / WriteFile / EditFile). The three decisions and their
// precedence are the bash ones: deny > allow (an exemption from the ask rules,
// never a whitelist) > ask > default(allow).
//
// A pattern is matched against the target's CANONICAL absolute path — cleaned,
// and symlinks resolved as far as the path exists — never against the argument
// as written, so `../x/../.ssh/id_rsa` and `/home/me/.ssh/id_rsa` are one path.
//
// A leading `~` expands to the user's home directory, and a pattern that is not
// absolute is anchored to the tree the rules came from: `$HOME` for the global
// config, the repository root for a `<tree>/.tachi/permissions.yaml`. That is
// what makes `ask: ["secrets/**"]` mean "inside this repository" in a project
// file, while the same line in the global config reads home-relative.
//
// Only the '*' wildcard is supported, and it spans '/' — so `~/.ssh/*` already
// covers every file below that directory. A LITERAL path (one with no wildcard)
// covers everything below it too: `deny: ["/home/me/.ssh"]` reads as the
// directory, not as a file that cannot exist without a trailing name.
type PathPermissions struct {
	Deny []string `yaml:"deny"` // blocked outright; error returned to the LLM
	Ask  []string `yaml:"ask"`  // requires approval: a frontend with a human asks; an unattended mode runs it
	// Allow exempts matching paths from the ask rules — and, on the same path,
	// from the judgment gate (permissions.files.jev): a call covered by an allow
	// rule is not judged, so this is also how a user turns the gate off for one
	// path (e.g. their own scratch directory outside the trees). GLOBAL config
	// only, and deny is decided first.
	Allow []string `yaml:"allow"`
}

// FilesPermissions holds the file tools' rule sets and the ONE judgment gate
// over all three of them.
//
// The gate is shared rather than per-tool on purpose: a threshold belongs to
// the QUESTION it was measured on, and ReadFile / WriteFile / EditFile are put
// to one question ("does the user want to see this first?") with the tool name
// among the facts it is asked about. Three switches over one measured number
// would invite three numbers where there is one.
//
// The rules are per tool, because the same path is not the same risk: reading a
// key is a leak, overwriting it is a loss of work.
type FilesPermissions struct {
	Read  PathPermissions `yaml:"read"`
	Write PathPermissions `yaml:"write"`
	Edit  PathPermissions `yaml:"edit"`

	// Jev turns the file judgment gate on: every ReadFile / WriteFile / EditFile
	// call that is not DENIED is judged, and one the judgment says a human
	// should see is sent to the frontend's ask path instead of running. "auto"
	// (the default, the same shape as permissions.bash.jev and
	// mcp_tool_search.matcher: the gate runs whenever the top-level `jev:` block
	// is usable) or "off" (the rules decide alone). See
	// docs/2026-09-30-files-ask-jev-design.md.
	//
	// It never widens what may run — deny is decided before the gate is reached,
	// and the judgment's only two outcomes are "run" and "ask the human" — and it
	// is off wherever nobody can answer (PermissionModeSkip: channel, `tachi -p`,
	// subagents, dream), where only the RULES decide.
	Jev string `yaml:"jev" default:"auto"`

	// JevThreshold is the noul probability at or above which the judgment is read
	// as "a human should see this". It belongs to the case set it was swept on,
	// NOT to this field: τ = 0.55 was measured on
	// pkg/jev/testdata/files-ask.json — 64 authored calls across read/write/edit,
	// 5 repeats, state rendered by agent/permission/jevfiles — with jev-1.13.0.
	// (The bash gate's number was swept later and against an ALIAS, so the two are
	// not comparable in provenance either — and they never were comparable in
	// meaning: the QUESTION is different, so neither number says anything about
	// the other.)
	//
	// The corridor is (0.49, 0.62): the highest ROUTINE call scored 0.49 (an export
	// written to ~/Downloads — outside the trees, produced for the user, and the
	// criteria call that routine) and the next needs-a-look call 0.62 (a private
	// key committed inside the tree). τ = 0.60 is the top of that corridor, and
	// both neighbours were measured: at 0.50 AND at 0.60 the gate raised 0 of the
	// 155 routine calls, so the band is flat rather than knife-edge — which is
	// what the harness's stability warning asks for, since a threshold 0.01 above
	// a routine case would flap. What it leaves is 0.02 of headroom under the
	// nearest needs-a-look case (0.62): the same case is the one a re-run of the
	// harness can walk across, so it is the line to watch in the sweep.
	//
	// WHAT IT DELIBERATELY MISSES: three of the 33 needs-a-look calls stayed below
	// it — writing and editing `.tachi/permissions.yaml` (0.43, 0.48) and a
	// wholesale replacement of a 200 KB in-tree data file (0.30). The first two are
	// the interesting ones: a JUDGMENT should not be the thing that stops an agent
	// from rewriting the rules it is judged by, and with a path-only state that
	// path does not read as alarming on its own (with the content in the state it
	// scored 0.87 — the payload was removed on purpose). The deterministic answer
	// is a rule, and it is worth writing:
	//
	//	permissions:
	//	  files:
	//	    write: {deny: ["**/.tachi/permissions.yaml"]}
	//	    edit:  {deny: ["**/.tachi/permissions.yaml"]}
	//
	// Re-sweep when the model version moves, when that case set CHANGES, or when
	// the STATE SHAPE does (a sentence in jevfiles is part of the question a
	// probability was measured against), or when the log shows auto-allowed writes
	// the user later wrote a deny rule for. A number copied from anywhere else —
	// including from permissions.bash.jev_threshold, which answers a different
	// question — is not a measurement of THIS question.
	//
	// A value of 1.0 is the shadow setting: the gate judges and logs every call but
	// can never act (a noul answer is at most 1.0), so the distribution can be
	// watched on real traffic before anything changes.
	JevThreshold float64 `yaml:"jev_threshold" default:"0.60"`

	// JevBudget bounds one judgment's wait. 2s is shorter than the bash gate's 3s
	// on purpose: a file call happens several times per turn and is often the
	// cheapest thing in it, so a slow judgment is a slow turn. Exceeding it falls
	// back to the rules.
	JevBudget time.Duration `yaml:"jev_budget" default:"2s"`
}

// The values BashPermissions.Jev and FilesPermissions.Jev accept.
const (
	// BashJevOff / FilesJevOff: rules alone decide.
	BashJevOff = "off"
	// BashJevAuto / FilesJevAuto runs the judgment gate whenever the top-level
	// jev block is usable (config.JevConfig.Configured: enabled AND holding a key).
	BashJevAuto = "auto"

	// FilesJevOff / FilesJevAuto are the file gate's two values, spelled
	// separately so neither gate's switch can be edited by accident through the
	// other's name.
	FilesJevOff  = "off"
	FilesJevAuto = "auto"
)

// PermissionsConfig groups tool permission rule sets.
type PermissionsConfig struct {
	Bash  BashPermissions  `yaml:"bash"`
	Files FilesPermissions `yaml:"files"`
}

func DefaultConfig() *Config {
	cfg := &Config{}
	if err := defaults.Set(cfg); err != nil {
		panic(fmt.Sprintf("config: failed to apply defaults: %v", err))
	}
	return cfg
}

func configDir() string {
	return BaseDir()
}

func ConfigPath() (string, error) {
	return filepath.Join(configDir(), configFileName), nil
}

// InputHistoryPath 返回终端输入历史文件路径
func InputHistoryPath() (string, error) {
	return filepath.Join(configDir(), inputHistoryFileName), nil
}

// SessionDir 返回会话存储目录路径
func SessionDir() (string, error) {
	return filepath.Join(configDir(), sessionDirName), nil
}

// OneoffDir 返回旁路执行记录（one-off transcript）的全局存储目录。
// 仅用于无会话上下文的旁路执行（tachi -c、github bot、dream）；
// 有会话上下文的记录在 <SessionDir>/<id>/oneoff/ 下。
func OneoffDir() string {
	return filepath.Join(configDir(), oneoffDirName)
}

// UsageDir 返回 usage 计费账本的全局存储目录（<home>/usage/）。
// 按日分片：<UsageDir>/YYYY-MM-DD.jsonl；永久保留，不做清理。
func UsageDir() string {
	return filepath.Join(configDir(), usageDirName)
}

// LogsDir returns the path to the debug logs directory.
func LogsDir() string {
	return filepath.Join(BaseDir(), logsDirName)
}

// MCPTokensDir returns the path to the MCP OAuth tokens directory.
func MCPTokensDir() string {
	return filepath.Join(BaseDir(), mcpTokensDirName)
}

// MCPSummariesDir returns the path to the generated MCP server summaries.
//
// One small JSON file per server, keyed by a hash of the tool set it was
// generated from, so it survives restarts and is only rewritten when the
// server's tools change (agent/mcp/mcp_summary_cache.go). It sits beside the
// tokens rather than inside a session: a summary describes a SERVER, not a
// conversation.
func MCPSummariesDir() string {
	return filepath.Join(BaseDir(), mcpSummariesDirName)
}

// MCPDiscoveredFile returns the path to the persisted MCP discovered-tools
// file for the given session. Each session keeps its own record of which MCP
// tools the LLM has loaded via MCPSearchTools, so a restart (or a resumed
// session) restores exactly that session's tools without re-searching. The
// file lives inside the session directory, so session cleanup deletes it
// automatically.
func MCPDiscoveredFile(sessionID string) string {
	return filepath.Join(BaseDir(), sessionDirName, sessionID, mcpDiscoveredFileName)
}

// GlobalSkillsDir returns the path to the global (user-level) skills directory.
func GlobalSkillsDir() string {
	return filepath.Join(BaseDir(), skillsDirName)
}

// WeixinStateDir returns the default path to the weixin state directory.
func WeixinStateDir() string {
	return filepath.Join(BaseDir(), weixinStateDirName)
}

// ResearchDir returns the path to the deep research reports directory.
func ResearchDir() string {
	return filepath.Join(BaseDir(), researchDirName)
}

// NodeDir returns this machine's node data directory, where the node identity, the
// single-instance lock and the published local connection info live
// (docs/2026-10-10-a2a-node-design.md §3.1).
//
// Derived rather than configurable on purpose: ONE data directory is ONE node
// identity, and that is the whole rule. A different --home, or a different OS user,
// is therefore a different node with no extra switch to set — and the integration
// harness gets its isolation for free, since its temporary home moves this path with
// everything else.
func NodeDir() string {
	return filepath.Join(BaseDir(), nodeDirName)
}

// ToolResultsDir returns the path to the tool results storage directory.
func ToolResultsDir() string {
	return filepath.Join(BaseDir(), toolResultsDirName)
}

// CronStorePath returns the default path for crons.json.
func CronStorePath() string {
	return filepath.Join(BaseDir(), cronStoreFileName)
}

// WebSearchPausePath returns the path to the web search provider pause state
// file. Quota-exhausted providers are marked here and resume at the start of
// the next billing cycle.
func WebSearchPausePath() string {
	return filepath.Join(BaseDir(), "websearch_paused.json")
}

// WebFetchPausePath returns the path to the web fetch provider pause state
// file. Quota-exhausted backends (e.g. firecrawl) are marked here and resume
// at the start of the next billing cycle.
func WebFetchPausePath() string {
	return filepath.Join(BaseDir(), "webfetch_paused.json")
}

func Load() (*Config, error) {
	path, err := ConfigPath()
	if err != nil {
		return nil, err
	}
	return LoadFrom(path)
}

func LoadFrom(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DefaultConfig(), nil
		}
		return nil, err
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	// Apply defaults to any fields not set in the YAML.
	if err := defaults.Set(cfg); err != nil {
		return nil, fmt.Errorf("config defaults: %w", err)
	}

	// Expand provider_aliases into every provider-reference field, so that
	// after loading all fields hold REAL provider names and no call site
	// outside this package ever needs to know about aliases.
	cfg.ExpandProviderAliases()

	return cfg, nil
}

// Save writes the whole config to config.yaml, in ONE atomic write.
//
// It is the settings page's write half (the runtime half is desktop's SaveSettings, which
// pushes the values at the running process). "Whole" is what makes it simple: the file is the
// in-memory config's own projection, so there is no path to resolve in a document this process
// did not write, no missing block to create, and no index to keep in step — a value the form
// changed is just a field that holds something else by the time this runs.
//
// The price is that config.yaml stops being a hand-written document the moment it is saved
// through the settings page: comments, blank lines, the file's own indentation and any key the
// Config struct does not declare are not carried over. backupOnce is what keeps that from
// being irreversible.
//
// A save whose result would be the bytes already in the file writes NOTHING — the file's mtime
// (and, on the first such save, its comments) must not be disturbed for a change that came to
// nothing.
func Save(cfg *Config) error {
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	return SaveTo(path, cfg)
}

// Marshal renders the whole config exactly as Save writes it.
//
// It is exported for the callers that have to SHOW the file rather than write it — the settings
// page's preview of what a save would produce marshals the same object on both sides of the batch,
// and its answer is only worth anything while it is the same function that writes the file. A
// second `yaml.Marshal` at that call site would be that answer, one encoder swap away from lying.
func Marshal(cfg *Config) ([]byte, error) {
	return yaml.Marshal(cfg)
}

// SaveTo is Save for an explicit path (tests, and any caller that already resolved it).
func SaveTo(path string, cfg *Config) error {
	data, err := Marshal(cfg)
	if err != nil {
		return err
	}
	if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, data) {
		return nil
	}
	if err := backupOnce(path); err != nil {
		return err
	}
	return atomicWriteConfig(path, data)
}

// backupOnce keeps the file as it was before the FIRST whole-file write, beside it as
// config.yaml.bak.
//
// It guards the one loss here that cannot be undone: config.yaml is hand-editable, people
// explain their API keys and their proxy in comments, and the first save through the settings
// page rewrites the whole document. One copy, taken once, turns that from "gone" into "your
// old file is right there" — and an existing .bak is never overwritten, so no later save can
// bury the original under an already-rewritten file.
//
// A file that is not there is not a failure: there is nothing to preserve.
func backupOnce(path string) error {
	current, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if _, err := os.Stat(path + backupSuffix); err == nil {
		return nil // already kept
	}
	return fileutil.WriteFilePrivate(path+backupSuffix, current)
}

// atomicWriteConfig writes the config file atomically, keeping the mode the file already had: a
// user's 0600 must not become 0644. Atomic (tmp + rename) for the same reason session metadata
// is — another Tachi process may be reading config.yaml at startup, and an in-place rewrite can
// hand it a half-written file.
func atomicWriteConfig(path string, out []byte) error {
	mode := os.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	return fileutil.AtomicWriteFile(path, out, 0o700, mode)
}

func Init() (string, error) {
	path, err := ConfigPath()
	if err != nil {
		return "", err
	}

	dir := configDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}

	cfg := &Config{}
	if err := defaults.Set(cfg); err != nil {
		return "", fmt.Errorf("config defaults: %w", err)
	}

	// Init-specific overrides (not defaults — user-facing template values).
	cfg.Provider = "deepseek"
	cfg.Providers = []ProviderConfig{
		{
			Name:    "deepseek-v4-flash",
			Type:    ProviderTypeAnthropic,
			Model:   "deepseek-v4-flash",
			BaseURL: "https://api.deepseek.com/anthropic",
			APIKey:  "<your-api-key>",
		},
		{
			Name:    "deepseek-v4-pro",
			Type:    ProviderTypeAnthropic,
			Model:   "deepseek-v4-pro",
			BaseURL: "https://api.deepseek.com/anthropic",
			APIKey:  "<your-api-key>",
		},
	}
	cfg.WebSearch.Providers = []WebSearchProviderConfig{
		{Type: WebSearchProviderExa, Key: "<your-exa-api-key>"}, // default provider
		// {Type: WebSearchProviderBrave, Key: "<your-brave-api-key>"}, // optional fallback provider
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return "", err
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return path, fmt.Errorf("config file already exists: %s", path)
		}
		return "", err
	}
	defer f.Close()

	if _, err := f.Write(data); err != nil {
		return "", err
	}
	return path, nil
}

// MCPEnabled returns true if at least one MCP server is configured.
func (c *Config) MCPEnabled() bool {
	return len(c.MCPServers) > 0
}

// LoadMCPServers loads MCP server config from the global JSON files (mcp.json).
//
// If JSON files are found, their servers replace whatever is currently in
// c.MCPServers. If no JSON files exist, c.MCPServers is left unchanged
// (typically empty, since YAML mcp_servers is no longer supported).
//
// Profile origin is tracked via the Profile field on each server:
// servers from the base file (mcp.json) get empty string, servers from the
// profile file (mcp.{profile}.json) get the profile name.
func (c *Config) LoadMCPServers() error {
	servers, err := LoadMCPConfig(c.ActiveMCPProfile)
	if err != nil {
		return err
	}
	if servers != nil {
		c.MCPServers = servers
	}
	return nil
}

// MCPToolSearchConfig controls MCPSearchTools behavior.
type MCPToolSearchConfig struct {
	Enabled           *bool `yaml:"enabled" default:"true"`           // false = load all tools directly (disable ToolSearch)
	MinToolsForSearch int   `yaml:"min_tools_for_search" default:"5"` // Auto-load all if total MCP tools <= this threshold

	// Matcher picks how a prose query is matched against the deferred pool:
	// "bm25" = the keyword search (the original behaviour), "jev" = the System
	// One judgment gate, "auto" = the gate whenever Jev.Configured(), keyword
	// search otherwise. A query that NAMES what it wants (a tool name, a
	// select: list, an mcp__server prefix) is resolved by exact lookup under
	// every setting — that is a lookup, not a judgment.
	Matcher string `yaml:"matcher" default:"auto"`
	// JevThreshold is the noul probability at which a tool is offered. It is
	// NOT a fact, and not a property of the gate: it belongs to the POOL whose
	// tool texts the questions carry, and the value picked on it is a point on
	// that pool's recall/cost curve, not a natural constant. τ = 0.30 is measured
	// on a real 184-tool pool (94 requests, jev-1.13.0) at recall 0.60 / 1.1 tools
	// offered per search / precision 0.55, and with nothing offered for a request
	// the pool cannot serve; the same curve gives recall 0.81 / 2.2 tools at 0.20
	// and 0.51 / 0.9 at 0.35. Quiet is what costs here: at 0.30 two searches in
	// five come back with nothing, against one in five at 0.20.
	//
	// The same gate swept against the 22-tool hand-written fixture gives 0.35,
	// where only half those searches would find anything. Pool SIZE is not the
	// difference — solo, one-shot and chunked requests score within 0.02 of one
	// another (how many questions ride in one request is a COST lever, not a
	// shape that moves the numbers: jev.max_questions_per_request); the
	// descriptions are. Adding a server or
	// rewriting a tool description makes this a hypothesis again: re-sweep
	// (docs/agents/jev.md, Measuring a threshold).
	JevThreshold float64 `yaml:"jev_threshold" default:"0.30"`
	// JevBudget bounds one search (all its chunked calls), not one HTTP
	// attempt: a search runs inside a model turn and degrades to the keyword
	// search rather than making the turn wait out pkg/jev's retry loop.
	JevBudget Duration `yaml:"jev_budget" default:"6s"`
	// CacheBudget is the CNY of cache invalidation ONE search may cause before
	// the user is asked to confirm it (人民币元/次, ≥0; 0 turns the confirmation
	// off). Loading a tool inserts its schema at the FRONT of the request
	// (tools → system → messages), which re-reads the whole prompt once — the
	// arithmetic and the reasoning are in docs/agents/mcp.md.
	//
	// It is a per-search threshold, not a session total: each prompt says what
	// THAT search costs, and 「本会话允许」 is how a user stops the asking.
	CacheBudget float64 `yaml:"cache_budget" default:"1"`
}

// MCPToolRefreshConfig controls background tool list polling for HTTP MCP servers.
// When enabled, the agent periodically calls ListTools on connected HTTP servers
// and updates the deferred pool / discovered set if tools have changed.
type MCPToolRefreshConfig struct {
	Enabled  *bool         `yaml:"enabled" default:"true"` // false = disable background refresh
	Interval time.Duration `yaml:"interval" default:"1m"`  // polling interval (0 = disabled)
}

// IsEnabled returns whether tool refresh is active. Defaults to true.
func (c *MCPToolRefreshConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// RefreshInterval returns the polling interval. Returns 0 if refresh is disabled.
func (c *MCPToolRefreshConfig) RefreshInterval() time.Duration {
	if !c.IsEnabled() {
		return 0
	}
	return c.Interval
}

// IsEnabled returns whether ToolSearch is active. Defaults to true.
func (c *MCPToolSearchConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// GetMaxIterations returns the effective max iterations:
// - nil → DefaultMaxIterations (50)
// - 0 → 0 (unlimited)
// - >0 → explicit value
func (c *Config) GetMaxIterations() int {
	if c == nil || c.MaxIterations == nil {
		return DefaultMaxIterations
	}
	return *c.MaxIterations
}

// projectPermissionsFileName is the project-level permissions file, looked up
// under <project-root>/.tachi/.
const projectPermissionsFileName = "permissions.yaml"

// LoadProjectPermissions reads permission rules from
// <projectRoot>/.tachi/permissions.yaml. The file mirrors the global
// config.yaml "permissions:" section:
//
//	permissions:
//	  bash:
//	    deny: ["git push --force*"]
//	    ask:  ["rm *"]
//
// Project rules are merged with global rules such that a project can only
// tighten: deny/ask are unioned, but project-level allow is IGNORED at
// policy construction time (allow is a user-global privilege), and
// disable_builtin_deny has no effect here either.
// A missing file returns zero-value rules and nil error.
func LoadProjectPermissions(projectRoot string) (PermissionsConfig, error) {
	var out struct {
		Permissions PermissionsConfig `yaml:"permissions"`
	}
	if projectRoot == "" {
		return out.Permissions, nil
	}
	path := filepath.Join(projectRoot, ".tachi", projectPermissionsFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out.Permissions, nil
		}
		return out.Permissions, fmt.Errorf("read project permissions: %w", err)
	}
	if err := yaml.Unmarshal(data, &out); err != nil {
		return out.Permissions, fmt.Errorf("parse %s: %w", path, err)
	}
	return out.Permissions, nil
}
