package main

// The settings table: one row per config key, saying what it takes for a saved value to
// reach the RUNNING process.
//
// It exists because the alternative drifts: a schema the frontend hard-codes plus a
// "needs restart" flag somebody has to remember to set, and the first config field added
// after that is wrong in one of the two places. Here the classification sits with the code
// that consumes the value, and two tests in settings_test.go hold it to the whole config —
// not to a hand-picked list:
//
//   - every yaml leaf of config.Config resolves to a row, so a new config field cannot
//     arrive without a decision (a missing disposition fails the build);
//   - every row matches at least one real leaf, so a typo'd key cannot sit there looking
//     authoritative.
//
// A leaf takes the MOST SPECIFIC row that covers it (see settingFor). A block-wide row is
// right when every leaf under it behaves the same way (`compact` is read per turn, all of
// it); `checkpoints` needs two rows because `enabled` is read per call while the three
// limits are baked into a manager.
//
// Rows live HERE, in desktop, not in agent — the decision the design doc left open
// (§八之三 ①) is settled by what the rows actually need: most of the runtime half is
// desktop-owned (the prompt cache, the shared MCP manager, the app-wide fan-out over
// sessions), and the per-agent half is written as a closure over *agent.AIAgent, which can
// move down to agent later without touching this file's shape.
//
// Design: docs/2026-09-14-desktop-settings-design.md §七、§八之三.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/linediff"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// applyMode says what it takes for a saved value to reach the running process.
type applyMode int

const (
	// applyLazy is the good case: nothing to push. Every reader of the key reads it off
	// the live *config.Config at the moment it matters (a turn start, a command, a new
	// session), so mutating the object in place IS the change.
	applyLazy applyMode = iota

	// applyPush means the value has already been consumed by something that keeps its own
	// copy — a built system prompt, a per-session policy, a constructed manager. The
	// running process sees it only when `Apply` carries it.
	applyPush

	// applyRestart means the value was consumed once at startup by something this process
	// cannot rebuild. A saved value lands after a restart, and the form must say so.
	applyRestart

	// applyUnused means this process never reads the key at all: it belongs to another
	// entry mode (tui / channel / acp / the web console). The form must hide it or mark it
	// 仅命令行模式 — offering a switch implies an effect it cannot have.
	//
	// It is deliberately NOT folded into applyRestart: "restart to apply" is false for a
	// key nothing here reads, and the two ask different things of the reader (restart the
	// app vs. go edit the file the other process reads).
	applyUnused
)

// String names the mode, for failure messages and anything that logs a row.
func (m applyMode) String() string {
	switch m {
	case applyLazy:
		return "applyLazy"
	case applyPush:
		return "applyPush"
	case applyRestart:
		return "applyRestart"
	case applyUnused:
		return "applyUnused"
	}
	return fmt.Sprintf("applyMode(%d)", int(m))
}

// The settings page's groups, in the order its left column lists them.
//
// They are declared HERE, next to the rows rather than in the frontend, for the same reason
// Mode and Note are: "where does this key belong" is a claim about the key, and the key's
// owner is this file. A frontend-side mapping would be a second list of config keys to keep
// in step — the drift this table exists to prevent — and no guard test could see it.
//
// The order is written out rather than derived from settingsTable's row order because the
// rows are ordered for READING THE CLASSIFICATION (hot first, then restart-only, then other
// entry modes' keys), which is not the order a person walks a settings page in.
var settingsCategoryOrder = []string{
	catGeneral, catModel, catTools, catMCP, catSession, catExtend, catAdvanced,
}

const (
	// catGeneral is the landing group: what a session says, and who writes the text that
	// surrounds it (titles, commit messages, `run`).
	catGeneral = "通用"
	// catModel is the provider DEFINITIONS and the default pick — not the per-command
	// bindings, which are in 通用 because the reader looks for them with the feature.
	catModel = "模型"
	// catTools is what the agent may do to the world, and the limits on what comes back.
	catTools = "工具与权限"
	// catMCP is the shared MCP layer: which profile is loaded, what auto-loads, how often
	// the tool list is re-read. A separate group because these three are one subsystem, and
	// one of them (the profile) is the only key in the table that is its own writer.
	catMCP = "MCP"
	// catSession is the machinery a turn runs through: compaction, review, the side-channel
	// runs, checkpoints, and the reminders injected into the prompt.
	catSession = "会话与上下文"
	// catExtend is the subsystems an agent is BUILT with — memory, subagents, hooks.
	// They behave alike (a per-session object, rebuilt on the live sessions) and a reader
	// looking for one looks for the others.
	catExtend = "扩展能力"
	// catAdvanced is what is consumed once at startup, plus the keys whose only route to the
	// running process is a restart. The ⟳ badge is not the group's definition — it is where
	// they happen to end up.
	catAdvanced = "高级"
)

// settingApply is one row.
type settingApply struct {
	// Key is a dot path covering itself and everything under it, on SEGMENT boundaries:
	// "compact" covers "compact.auto", never "compaction"; "language" covers only itself.
	// It is also the path config's own reader and writer take, which is why it is dotted
	// rather than slash- or index-based.
	Key string
	// Mode is what the key needs, per the block above.
	Mode applyMode
	// Apply is the runtime half of a push. It must do ONLY that half: the caller mutates
	// the config object in place first (see SaveSettings's contract in the design doc),
	// so a row that wrote the value itself would be a second writer of the same leaf.
	//
	// nil on a push means the code is not written yet — the row is still the right
	// classification, and needsRestart() reports the truth for today.
	Apply func(d *desktopApp, cfg *config.Config) error
	// Note is why the row is what it is, in the user's words where it can be: the reason a
	// restart is needed, which mode a key belongs to instead. Required for restart and
	// unused rows, and for a push whose Apply is missing — those are exactly the cases a
	// settings page has to tell the reader about, so they must not be silent.
	Note string
	// OwnsWrite says this row applies its OWN value, so SaveSettings must not pre-apply it in the
	// batch: there would be two writers of one leaf, and the batch's value would survive a push
	// that failed — a field describing a change the runtime refused (the panel would show the new
	// value while the connections were still the old one).
	//
	// The push reads its new value from d.savingUpdates (see SaveSettings), which is why a row
	// that owns its write must be applyPush WITH an Apply: with no code there would be no writer
	// at all.
	OwnsWrite bool
	// Category is which group of the form the field shows up under, from settingsCategoryOrder.
	//
	// Required exactly when the row is offered, and forbidden otherwise: an unoffered row is
	// another entry mode's key, and a group for a field no form draws is a claim about a page
	// that does not exist. The two states therefore mean one thing each, which is what lets the
	// guard test check both directions.
	Category string

	// ListExample is one example VALUE PER LINE for the `string_list` fields under this row,
	// which the form shows as the box's hint (SettingFieldVO.ListExample).
	//
	// It belongs to the row because the page cannot derive it: a list of shell globs and a list
	// of paths have the same Kind, and only the row knows which one it is. A list that teaches
	// the wrong shape of value is worse than one that teaches nothing — `git *` in an empty path
	// box reads as "type a command here".
	//
	// Empty means the form shows its plain hint, with no example.
	ListExample string
}

// needsRestart reports whether a value saved now can only take effect after a restart.
//
// It is derived, never declared: a restart row, or a push whose runtime half is not
// written yet, is a restart TODAY. That is what makes "forgot to mark it" impossible —
// there is no mark to forget, only an absence of code.
func (s settingApply) needsRestart() bool {
	return s.Apply == nil && (s.Mode == applyPush || s.Mode == applyRestart)
}

// offered reports whether the settings form should present the key at all.
func (s settingApply) offered() bool { return s.Mode != applyUnused }

// covers reports whether this row speaks for a dotted key.
func (s settingApply) covers(key string) bool {
	return key == s.Key || strings.HasPrefix(key, s.Key+".")
}

// settingFor resolves a dotted key to the row that speaks for it: the most specific one,
// so a leaf beats the block it sits in. The second result is false only for a key no row
// covers at all, which settings_test.go fails the build over.
func settingFor(key string) (settingApply, bool) {
	var best settingApply
	found := false
	for _, row := range settingsTable {
		if !row.covers(key) {
			continue
		}
		if !found || len(row.Key) > len(best.Key) {
			best, found = row, true
		}
	}
	return best, found
}

// settingsTable is the whole classification. Order is for reading, not for lookup
// (settingFor picks by specificity).
var settingsTable = []settingApply{
	// --- prompts and titles (what a session SAYS) ---
	{
		Key:      "language",
		Category: catGeneral,
		Mode:     applyPush,
		// The prompt is built once per (cwd, session, roots, mode) and memoized, and the
		// language is not part of that key — so without this the old prompt is served
		// forever (that is why the design doc calls the clear a PREREQUISITE, not a
		// nicety).
		Apply: func(d *desktopApp, _ *config.Config) error {
			d.invalidateSystemPrompts()
			return nil
		},
	},
	{
		Key:      "extra_system_prompt",
		Category: catGeneral,
		Mode:     applyPush,
		Apply:    func(d *desktopApp, _ *config.Config) error { d.invalidateSystemPrompts(); return nil },
	},
	{
		Key:      "title_generation",
		Category: catGeneral,
		Mode:     applyPush,
		Apply: func(d *desktopApp, cfg *config.Config) error {
			// nil = the default (on), the same reading config uses everywhere.
			enabled := cfg.TitleGeneration == nil || *cfg.TitleGeneration
			return pushError("标题生成", d.applyToLiveAgents(func(a *agent.AIAgent) error {
				a.SetTitleGenEnabled(enabled)
				return nil
			}))
		},
	},
	{
		Key:      "title_provider",
		Category: catGeneral,
		Mode:     applyPush,
		Apply: func(d *desktopApp, cfg *config.Config) error {
			return pushError("标题 provider", d.applyToLiveAgents(func(a *agent.AIAgent) error {
				a.SetupTitleProvider(cfg)
				return nil
			}))
		},
	},
	{
		Key:      "commit_provider",
		Category: catGeneral,
		Mode:     applyPush,
		Apply: func(d *desktopApp, cfg *config.Config) error {
			return pushError("提交信息 provider", d.applyToLiveAgents(func(a *agent.AIAgent) error {
				a.SetupCommitProvider(cfg)
				return nil
			}))
		},
	},
	{
		Key:      "run_provider",
		Category: catGeneral,
		Mode:     applyPush,
		Apply: func(d *desktopApp, cfg *config.Config) error {
			return pushError("run provider", d.applyToLiveAgents(func(a *agent.AIAgent) error {
				a.SetupRunProvider(cfg)
				return nil
			}))
		},
	},

	// --- the device link (a screen for what the agent is waiting on) ---
	{
		Key:      "badge",
		Category: catGeneral,
		Mode:     applyPush,
		Apply:    applyBadgeLink,
	},

	// --- the phone interface (the same conversations, a third screen) ---
	// enabled/listen are consumed once, when the listener is built: opening a socket is a
	// process-level act, and a restart is the honest way to apply it.
	{
		Key:      "mobile.enabled",
		Category: catExtend,
		Mode:     applyRestart,
		Note:     "局域网里的明文 HTTP：拿到令牌的人就能让这台电脑执行命令 —— 只在自己家的网络里打开。",
	},
	{
		Key:      "mobile.listen",
		Category: catExtend,
		Mode:     applyRestart,
		Note:     "手机要能从局域网连进来，所以默认监听 0.0.0.0:8787；监听失败只影响这一项，不影响桌面启动。",
	},
	{
		Key:      "mobile.max_attachment_mb",
		Category: catExtend,
		Mode:     applyLazy,
		Note:     "手机能下载的单个附件上限（只算 agent 用 SendFile 交付过的文件）；下一次下载读新值。",
	},

	// --- model and providers ---
	// Prices and credit rates are read lazily by the usage ledger, but resolving a
	// provider is a per-session fact (SetResolvedProvider), so the row is a push.
	// The two are NOT the same kind of fact. `provider` is the DEFAULT for sessions that do not
	// exist yet: every running session has its own provider recorded (the /model switch), so a
	// new default reaches them only by being used — which is lazy, not a rebuild.
	{Key: "provider", Category: catModel, Mode: applyLazy, Note: "这是新会话的默认值；已开的会话各自记录着自己的 provider，要用 /model 切换"},
	// `providers` is the provider DEFINITIONS (model, base_url, api_key, thinking_level): an
	// edit has to reach the agents that were resolved before it.
	{
		Key:      "providers",
		Category: catModel,
		Mode:     applyPush,
		Apply: func(d *desktopApp, _ *config.Config) error {
			return pushError("provider 定义", d.applyToLiveRuns(func(r liveRun) error {
				if r.Agent.Config.Resolved == nil {
					return nil // not resolved yet: it reads the new config when it is built
				}
				// Re-resolve under the name this session already uses, so the CHOICE is left
				// alone and only the definition behind it moves.
				name := r.Agent.Config.Resolved.Name
				if _, err := r.Agent.SetResolvedProvider(name); err != nil {
					return err
				}
				// The provider's own thinking_level came in with the resolve; a session's
				// explicit override has to win again — the order SwitchProvider uses.
				applyThinking(r.Agent, d.sessionThinkingLevel(r.ID))
				return nil
			}))
		},
	},
	{Key: "provider_aliases", Category: catModel, Mode: applyRestart, Note: "ExpandProviderAliases 只在 LoadFrom 末尾跑一次，别名早已展开成真名"},
	{Key: "max_tokens", Category: catModel, Mode: applyLazy},
	{Key: "max_iterations", Mode: applyUnused, Note: "desktop 不传 MaxIterations（0 = 无限），只有 tachi -p 读它"},
	// Jev is a judgment endpoint rather than a provider, but it is still "which model answers
	// this", so it sits with the model keys. One row covers the whole block: a row per leaf would
	// be five rows saying the same thing, and nothing reads the leaves one at a time.
	//
	// It is a PUSH because the gate's client is built once per agent (agent/agent_jev.go's
	// newJevJudge, called from mcpSearcher) and keeps key, model, base_url, proxy and timeout from
	// the block as it stood then — a saved value would otherwise wait for a rebuilt agent. The
	// ledger half is the exception: a row's price and credit rate are resolved where the row is
	// written (agent/mcp/jev_search.go reads the live block to price the call it just made), so
	// pricing edits ride along without needing the push.
	{
		Key:      "jev",
		Category: catModel,
		Mode:     applyPush,
		Apply: func(d *desktopApp, _ *config.Config) error {
			return pushError("Jev", d.applyToLiveAgents(func(a *agent.AIAgent) error {
				a.RefreshMCPSearch()
				return nil
			}))
		},
	},

	// --- sessions ---
	{Key: "session_cleanup_max_count", Category: catGeneral, Mode: applyLazy, Note: "建新会话时读；已在运行的会话 manager 保留旧上限"},
	{Key: "usd_to_cny", Category: catModel, Mode: applyLazy, Note: "每个读取方都在用时现取（llm.USDToCNYRate(cfg) 由 usage 记账 / jev 调用传入），下次算价即生效"},

	// --- tools ---
	{
		Key:      "web_search",
		Category: catTools,
		Mode:     applyPush,
		Apply: func(d *desktopApp, _ *config.Config) error {
			// ReloadTools rebuilds every built-in from the live config (the sync.Once provider
			// caches live on the tool instance, so a fresh instance re-reads) and drops
			// web_search when it is no longer configured.
			return pushError("搜索工具", d.applyToLiveAgents(func(a *agent.AIAgent) error {
				return a.ReloadTools()
			}))
		},
	},
	{
		Key:      "web_fetch",
		Category: catTools,
		Mode:     applyPush,
		Apply: func(d *desktopApp, _ *config.Config) error {
			return pushError("抓取工具", d.applyToLiveAgents(func(a *agent.AIAgent) error {
				return a.ReloadTools()
			}))
		},
	},
	{
		Key:      "tool_result",
		Category: catTools,
		Mode:     applyPush,
		Apply: func(d *desktopApp, cfg *config.Config) error {
			// Both halves or neither: the built-in tools AND the shared MCP manager, which
			// snapshots these two values at construction. Rebuilding only the tools would leave
			// MCP results truncated by the old limits while the page says the change is live.
			if d.mcp != nil {
				d.mcp.SetToolResultLimits(cfg.ToolResult.ResultFileDir(), cfg.ToolResult.MaxResultChars())
			}
			return pushError("工具结果上限", d.applyToLiveAgents(func(a *agent.AIAgent) error {
				return a.ReloadTools()
			}))
		},
	},
	{
		Key:      "permissions.bash",
		Category: catTools,
		Mode:     applyPush,
		// The examples are COMMANDS: this block classifies shell commands, and the list boxes
		// under it are where a rule is written.
		ListExample: "git *\nnpm *",
		Apply: func(d *desktopApp, _ *config.Config) error {
			return pushError("权限策略", d.applyToLiveRuns(func(r liveRun) error {
				// SetPermissionTrees is the same rebuild a tree change already goes through: it
				// compiles a fresh policy from the LIVE config against every tree this session
				// works in (each one's project-level rules live there) and inherits the
				// session's approvals. The engine needs no new path — only the trigger was
				// missing.
				primary, additional := d.sessionRoots(r.ID)
				r.Agent.SetPermissionTrees(context.Background(), primary, additional)
				return nil
			}))
		},
	},
	{
		// The file tools' rules are on the SAME policy object as the command rules
		// (permission.Policy carries both halves), so the push is the same rebuild —
		// and the gate's own threshold/budget need no push at all: they are read
		// live on every call.
		Key:      "permissions.files",
		Category: catTools,
		Mode:     applyPush,
		// PATHS, not commands: these lists classify a call's target file. The seed examples
		// are the two shapes worth knowing — `~` for the user's home, and a leading `*` for
		// "anywhere" (a pattern that starts with a wildcard is deliberately not anchored).
		ListExample: "~/.ssh/*\n*/.env",
		Apply: func(d *desktopApp, _ *config.Config) error {
			return pushError("文件权限策略", d.applyToLiveRuns(func(r liveRun) error {
				primary, additional := d.sessionRoots(r.ID)
				r.Agent.SetPermissionTrees(context.Background(), primary, additional)
				return nil
			}))
		},
	},

	// --- MCP ---
	{
		Key:      "active_mcp_profile",
		Category: catMCP,
		Mode:     applyPush,
		// This row OWNS the write of its leaf (OwnsWrite): the batch must not pre-apply the profile,
		// or a switch that fails would leave the field naming a profile whose servers are not
		// loaded — and the panel would show it as active. d.switchMCPProfileTo writes it, under
		// cfgMu, only after the connections have actually moved.
		OwnsWrite: true,
		Apply: func(d *desktopApp, _ *config.Config) error {
			profile, ok := d.savingUpdates["active_mcp_profile"].(string)
			if !ok {
				return nil // not a string: the batch's type check refused it long before this step
			}
			if d.mcp == nil {
				// MCP is off: the value is written down and read at the next start. The row is
				// still its writer, so it is written HERE.
				d.withConfig(func(c *config.Config) { c.ActiveMCPProfile = profile })
				return nil
			}
			// One call, not one per session: the manager and its connections are shared, and the
			// profile file is resolved against the displayed session's tree (mcp.<name>.json can
			// live in a project).
			return d.switchMCPProfileTo(profile)
		},
	},
	{
		Key:      "mcp_tool_search",
		Category: catMCP,
		Mode:     applyPush,
		Apply: func(d *desktopApp, cfg *config.Config) error {
			if d.mcp == nil {
				return nil
			}
			// No reconnect: the pool already holds every discovered tool, so the auto-load
			// decision is re-derived from it (the manager also updates the live sessions'
			// discovered sets). The registry half is per agent — the pool is per process.
			//
			// RefreshMCPSearch is the other half of this row: `matcher` and the jev_*
			// leaves are read when an agent BUILDS its searcher, so without it a live
			// session would keep matching the old way (and, for a matcher change, keep
			// describing the old advice to the model).
			added, removed := d.mcp.RecomputeAutoLoad(cfg)
			return pushError("MCP 工具搜索", d.applyToLiveAgents(func(a *agent.AIAgent) error {
				for _, dt := range removed {
					a.UnregisterTool(dt.Name)
				}
				for _, dt := range added {
					a.RegisterTool(dt.Tool)
				}
				a.RefreshMCPSearch()
				return nil
			}))
		},
	},
	{
		Key:      "mcp_tool_refresh",
		Category: catMCP,
		Mode:     applyPush,
		Apply: func(d *desktopApp, cfg *config.Config) error {
			if d.mcp == nil {
				return nil
			}
			a, note := d.appAgentForMCP()
			if a == nil {
				return errors.New(note)
			}
			// ONCE, from the app — not per session: StartRefresher re-points the SHARED poller's
			// callback at whichever agent calls it, so driving it from every session would leave
			// it bound to whichever ran last.
			return a.ReloadMCPToolRefresh(context.Background(), cfg)
		},
	},

	// --- per-session subsystems built at construction ---
	{
		Key:      "subagent",
		Category: catExtend,
		Mode:     applyPush,
		Apply: func(d *desktopApp, cfg *config.Config) error {
			// Covers subagent.provider too: ReloadSubagent re-resolves the dedicated provider
			// and rebuilds the executor (worktree included) under one call.
			return pushError("subagent", d.applyToLiveAgents(func(a *agent.AIAgent) error {
				return a.ReloadSubagent(cfg)
			}))
		},
	},
	{
		Key:      "memory",
		Category: catExtend,
		Mode:     applyPush,
		Apply: func(d *desktopApp, cfg *config.Config) error {
			// The reminder is in the collector for the agent's life, so it is pointed at the new
			// backend instead of being re-registered — see ReloadMemory.
			return pushError("memory", d.applyToLiveAgents(func(a *agent.AIAgent) error {
				return a.ReloadMemory(context.Background(), cfg)
			}))
		},
	},
	{
		Key:      "hooks",
		Category: catExtend,
		Mode:     applyPush,
		Apply: func(d *desktopApp, cfg *config.Config) error {
			return pushError("hooks", d.applyToLiveAgents(func(a *agent.AIAgent) error {
				return a.ReloadHooks(cfg)
			}))
		},
	},

	// --- snapshots and bookkeeping ---
	{Key: "checkpoints.enabled", Category: catSession, Mode: applyLazy},
	// The three limits need no reload call: the manager bakes them in at construction, and the
	// per-session key now CONTAINS them, so the next use rebuilds it with the new values.
	{Key: "checkpoints.max_files", Category: catSession, Mode: applyLazy, Note: "Options 已并进检查点 manager 的 key：下次用到时按新值重建"},
	{Key: "checkpoints.max_bytes", Category: catSession, Mode: applyLazy, Note: "同 checkpoints.max_files"},
	{Key: "checkpoints.retain", Category: catSession, Mode: applyLazy, Note: "同 checkpoints.max_files"},
	{Key: "oneoff", Category: catSession, Mode: applyLazy},
	{Key: "compact", Category: catSession, Mode: applyLazy},
	{Key: "review", Category: catSession, Mode: applyLazy, Note: "ResolveReviewOptions(cfg) 每次命令现读",
		// Its one list is TOOL NAMES (config.AllowedTools), so it gets its own example rather
		// than the plain hint: a list box is where a reader meets the value's shape.
		ListExample: "ReadFile\nGrep"},
	// Registered unconditionally, reading the config on every generation (see GitReminder):
	// nothing has to be pushed, and nothing has to be rebuilt — which is the point, since the
	// collector cannot remove a reminder without taking every other reminder's state with it.
	{Key: "system_reminder", Category: catSession, Mode: applyLazy, Note: "git reminder 每次生成时现读配置"},

	// peers: 这台机器可以向哪些节点委派（发送方目录，与 node.enabled 无关）。工具与 reminder
	// 都在用时现读，所以是热改：改完下一轮就生效，不用重启。
	{Key: "peers", Category: catAdvanced, Mode: applyLazy,
		Note: "发送方目录：name / url / note；工具与 reminder 每轮现读，改完下一轮生效"},

	// --- consumed once at startup ---
	{
		Key:      "logs.level",
		Category: catAdvanced,
		Mode:     applyPush,
		Apply: func(_ *desktopApp, cfg *config.Config) error {
			// Process-wide, not per session: every handler holds the same LevelVar, so this
			// reaches loggers built at startup — which is exactly what the old scalar could not.
			logger.SetLevel(cfg.Logs.Level)
			return nil
		},
	},
	{Key: "logs", Category: catAdvanced, Mode: applyRestart, Note: "max_size / max_files 冻结在 rotatingWriter 上（含已打开的 fd）；logs.level 有更具体的一行，已是热改"},
	{Key: "debug.pprof", Category: catAdvanced, Mode: applyRestart, Note: "启动即绑端口"},
	{Key: "herdr", Category: catAdvanced, Mode: applyRestart, Note: "hook 初始化时读，且 DetectHerdr 还看 HERDR_ENV"},
	// node: 入口启动时起/停本机节点服务（StartOrAttachNode），运行中切换需要能起停服务本身，
	// 首版不做 —— 所以这一整块是 ⟳ 而不是热改。
	//
	// **每个键各有一行**，这不是啰嗦：一条块级说明会印在它覆盖的**每一个**字段下面，于是读者
	// 在 `listen` 底下读到的是 `workspace` 的事。块里字段各说各话时，就得到叶子
	// （settings_test.go 的 TestANoteIsSharedOnlyWithinOneBlock 钉着这条）。
	{Key: "node.enabled", Category: catAdvanced, Mode: applyRestart,
		Note: "承载节点服务：别的机器可以把远程任务发给本机（默认关，关着连数据目录、锁、监听都不碰）。运行中起停需要能控制服务本身，首版没做"},
	{Key: "node.listen", Category: catAdvanced, Mode: applyRestart,
		Note: "远端 A2A 面绑的地址，启动时绑定；离开 loopback 就自动变成 mTLS、要求调用方出示证书。绑通配地址必须同时填 public_url，否则拒绝启动"},
	{Key: "node.public_url", Category: catAdvanced, Mode: applyRestart,
		Note: "调用方能拨到的地址，进 Card 也进发现公告；留空从 listen 推 —— 绑通配地址时推不出来，那时必须填"},
	{Key: "node.name", Category: catAdvanced, Mode: applyRestart,
		Note: "本机自称的名字：进 Card、随每次调用带给对端（对端只当「自报」记）。不可冒充的是密钥推出来的 node_id"},
	{Key: "node.description", Category: catAdvanced, Mode: applyRestart,
		Note: "一句话说明这台机器是干什么的，进 Card、对端的面板里读得到；留空用内置那句。Card 是启动时发布的自述，所以要重启"},
	{Key: "node.workspace", Category: catAdvanced, Mode: applyRestart,
		Note: "远程任务的工作目录（默认 ~/workspace，~ 会展开、首次用到时才建）；留空这个节点起不来"},
	{Key: "node.question_timeout", Category: catAdvanced, Mode: applyRestart,
		Note: "一个远程提问等多久才判失败：到点这一轮判 FAILED、问题留在状态里；0 表示不限时。时限在建 runner 时定下"},
	{Key: "node.max_parked", Category: catAdvanced, Mode: applyRestart,
		Note: "同时挂住几个提问（每个都占一条会话和一轮 turn）；到上限是「这次问不出去」，不是拒绝新活儿"},
	{Key: "node.discovery.mdns.enabled", Category: catAdvanced, Mode: applyRestart,
		Note: "本机在局域网上找别人（浏览），以及在承载节点服务的进程里被别人找到（公告）——一个开关管这两半；第一次要用发送侧时才起浏览器"},
	{Key: "node.discovery.mdns.interfaces", Category: catAdvanced, Mode: applyRestart,
		Note: "在哪些网卡上浏览/公告，如 en0、eth0；留空＝自动挑能用的那几个（排除 loopback、VPN 隧道、虚拟机网桥、只有 link-local 地址的网卡）"},
	{Key: "node.discovery.gossip.enabled", Category: catAdvanced, Mode: applyRestart,
		Note: "本机去找别人（不限于本网段的）与别人来找本机，都由这一个开关决定：问邻居它替谁背书；路由挂在远端 A2A 面上，节点服务起来时才决定它答不答"},
	{Key: "node.discovery.gossip.period", Category: catAdvanced, Mode: applyRestart,
		Note: "问邻居的节拍：每这么久挑一轮换（默认 3 分钟，0／留空＝内置默认）；它得满足「节拍 × 邻居数 ÷ 扇出 < contact_ttl」，否则总有邻居轮不到刷新"},
	{Key: "node.discovery.gossip.fanout", Category: catAdvanced, Mode: applyRestart,
		Note: "一轮问几个邻居（默认 2，0／留空＝内置默认）；每个都是一次请求，这是收窄上面那条「轮不到」的旋钮"},
	{Key: "node.discovery.gossip.nudge", Category: catAdvanced, Mode: applyRestart,
		Note: "目录内容一变（多了一台机器／多了一个地址），下一轮提前到这么久之后（默认 10 秒，0／留空＝内置默认）；两次提前之间的下限固定是它的两倍，不单列"},
	{Key: "node.discovery.gossip.timeout", Category: catAdvanced, Mode: applyRestart,
		Note: "一次交换的预算，地址与 body 全算在内（默认 5 秒，0／留空＝内置默认）；比一次握手还短就会把每一轮都变成失败（也就是静音）"},
	{Key: "node.discovery.gossip.mute", Category: catAdvanced, Mode: applyRestart,
		Note: "一轮失败的邻居被跳过多久（默认 5 分钟，0／留空＝内置默认）；它是「配置里写了、但机器关着」这件事的代价上限"},
	{Key: "node.discovery.ttl", Category: catAdvanced, Mode: applyRestart,
		Note: "发现到的机器在目录里留多久（默认 1 小时，0／留空＝用内置默认）；这个窗口是建账本时读的，而一个进程只建一次账本"},
	{Key: "node.discovery.contact_ttl", Category: catAdvanced, Mode: applyRestart,
		Note: "本机肯替一台机器背书、以及把邻居的一条转述当成新鲜的有效期（默认 10 分钟，0／留空＝用内置默认）；同样是建账本时读的"},

	// --- another entry mode's keys ---
	{Key: "tui", Mode: applyUnused, Note: "TUI 专属"},
	// The one applyUnused row this process PARTLY reads: a channel's `notify` switch decides
	// whether the NotifyUser tool mounts (see docs/agents/channel.md), so "desktop 不启用" would
	// be false. It stays applyUnused because that mode is about the FORM — channel names are
	// user-defined map keys, so no form can enumerate them — not about whether anything reads it.
	{Key: "channel", Mode: applyUnused, Note: "channel 模式的 IM 后端；desktop 只读其中的 notify 开关（决定 NotifyUser 工具挂不挂），账号与收信仍归 channel 进程"},
	{Key: "cron", Category: catAdvanced, Mode: applyRestart,
		Note: "后台定时任务；channel 与 desktop 各自只调度发给自己的任务，改动后要重启对应进程。其中 notify_matcher 与 jev_threshold 只被 channel 的定时任务使用"},
	{Key: "dream", Category: catSession, Mode: applyRestart,
		Note: "后台记忆整理（AutoDream）：桌面每 30 分钟检查一次，有新会话且距上次整理超过 min_interval 才真正整理；enabled 保存后重启生效。schedule 只被 channel 进程使用，桌面按 min_interval 检查"},
	{Key: "acp", Mode: applyUnused, Note: "ACP 入口的配置"},
	{Key: "deep_research", Mode: applyUnused, Note: "desktop 没有 research 命令（TUI 与 tachi -p 才有）"},
	{Key: "web", Mode: applyUnused, Note: "tachi web 是独立进程，重启那个进程即可"},
}

// settingChoices declares the leaves whose value is a CHOICE from a closed set, and where that
// set comes from.
//
// It is keyed by the leaf's path, and it is a SECOND table rather than a field on settingApply,
// because the two have different granularity. A row speaks for a BLOCK — `subagent` covers
// eleven leaves and pushes them all the same way — while a choice belongs to ONE leaf:
// `subagent.provider` names a provider, `subagent.max_concurrency` is a number. Hanging this on
// the row would mean splitting block rows per leaf, and copying their Apply closures, just to
// attach a dropdown.
//
// A key may stand for a LIST ITEM'S leaf, with `*` for a sequence index or a map key — the same
// character config.YAMLPaths writes, so the key here is the leaf as the config's own walker
// names it (`providers.*.type`). That is what makes the guard test a set membership rather than
// a second list: every key must BE one of config's structural leaves. One decision, however many
// providers the config happens to hold.
//
// The values are FUNCTIONS of the live config, because the choices are what THIS config defines:
// a list of provider names hard-coded here would be a second answer to "which providers exist",
// and it would be wrong the moment someone adds one. A closed set is the other direction — see
// providerTypes — and takes the function shape all the same, so a caller never has to know which
// kind it is looking at.
var settingChoices = map[string]func(*config.Config) []string{
	"provider":                providerNames,
	"title_provider":          providerNames,
	"commit_provider":         providerNames,
	"run_provider":            providerNames,
	"subagent.provider":       providerNames,
	"review.provider":         providerNames,
	"memory.keyword_provider": providerNames,
	"dream.provider":          providerNames,
	"providers.*.type":        providerTypes,
	"badge.transport":         badgeTransports,
}

// badgeTransports is the device link's transport, as a closed set.
//
// `off` is listed rather than expressed as an empty value on purpose: the settings page draws
// a dropdown from this list, and a reader needs a visible way to turn the link OFF — an empty
// string would leave them guessing that clearing the box is how.
func badgeTransports(*config.Config) []string {
	// `ble` 排最后：它不需要线，但第一次要在电脑上输一次 6 位配对码（设备屏幕上有），
	// 所以它不是「插上就能用」的那一类。
	//
	// 曾经还有一个 `tcp`（tachi 监听一个端口、脚本冒充设备连上来），那是链路刚起步时
	// 的脚手架——设备已经能连了，它就没有存在的理由了。
	return []string{"off", "serial", "ble"}
}

// providerNames lists the providers this config defines, in file order — the choice list for
// every key that NAMES one: the default pick (`provider`), the per-command bindings
// (title / commit / run) and the subsystem ones (subagent, review, memory's keyword extractor).
//
// An entry with no name is skipped (a half-written provider is not a choice), and duplicates
// collapse: two entries sharing a name are one choice to the reader even though they are two
// providers. Empty when the config defines none — which the page shows as a text box, because
// a dropdown with nothing in it is a trap.
func providerNames(cfg *config.Config) []string {
	out := make([]string, 0, len(cfg.Providers))
	seen := make(map[string]bool, len(cfg.Providers))
	for _, p := range cfg.Providers {
		if p.Name == "" || seen[p.Name] {
			continue
		}
		seen[p.Name] = true
		out = append(out, p.Name)
	}
	return out
}

// providerTypes is the closed set of provider TYPES (openai / openai-res / anthropic), from the
// constants config and llm share. It is static where providerNames is config-derived, and the
// reason it is worth a dropdown either way: nothing validates this field at load time, so a
// typo survives parsing and then fails when a session resolves its provider
// ("unsupported provider type: …") — a config that loads and then cannot talk to anything.
func providerTypes(*config.Config) []string { return config.ProviderTypes() }

// settingFoldedLists names the config lists whose items the form draws FOLDED whatever their
// size, and it exists because size is the wrong question for these two.
//
// The size rule's premise is that a folded item costs a click while an open one costs scrolling,
// and that is true of a list a reader SCANS (`providers`, `web_search.providers`: pick the one you
// want and open it). A web_search / web_fetch provider is not scanned — it is three leaves, and what the
// reader comes for is the block's own scalars (`proxy`, `timeout`, `max_results`) or the one
// provider they already know they want. Three open items therefore bury the page's real content
// under a wall of API keys, which is precisely the scrolling the size rule was invented to avoid.
//
// It is a DECLARATION rather than another number, because there is no number that says this: the
// items are small in leaves and large in screen, and a threshold that folded them would fold every
// three-leaf item anywhere. Keyed by the list's own path — the same vocabulary
// config.ListItemPaths writes, so this is a set membership the guard test can check rather than a
// second list of paths.
var settingFoldedLists = []string{
	"web_search.providers",
	"web_fetch.providers",
}

// choiceFor resolves a concrete leaf to the choice set declared for it: an exact key first, then
// a structural one, where `*` stands for one segment (`providers.*.type` covers `providers.0.type`
// and `providers.1.type` but never `web_search.providers.0.type` — that would be a different
// list, whose `type` means a different thing).
func choiceFor(key string) (func(*config.Config) []string, bool) {
	if fn, ok := settingChoices[key]; ok {
		return fn, true
	}
	for pattern, fn := range settingChoices {
		if !strings.Contains(pattern, "*") {
			continue
		}
		ps, ks := strings.Split(pattern, "."), strings.Split(key, ".")
		if len(ps) != len(ks) {
			continue
		}
		matched := true
		for i := range ps {
			if ps[i] != "*" && ps[i] != ks[i] {
				matched = false
				break
			}
		}
		if matched {
			return fn, true
		}
	}
	return nil, false
}

// pushError folds per-session failures into one error for a row's Apply, or nil when every
// session took the change. what names the setting in the user's words, because the caller
// that renders it (the post-save summary) has no idea which row it came from.
func pushError(what string, failures []string) error {
	if len(failures) == 0 {
		return nil
	}
	return fmt.Errorf("%s：%s", what, strings.Join(failures, "；"))
}

// SettingFieldVO is one field of the settings form: a settingsTable row plus the value the
// form should show.
type SettingFieldVO struct {
	// Key is the dot path the form sends back, and the same path config's reader and writer take.
	Key string `json:"key"`
	// Kind is how to render the input: "string" / "bool" / "int" / "float" / "string_list" /
	// "other". Derived from the value's own Go type rather than declared per row, because the
	// row's Key already fixes the field and a second declaration is a second thing to get
	// wrong. A key that needs more (an enum's choices) will carry it here when one exists.
	Kind string `json:"kind"`
	// Value is the effective value — an unset pointer reads as its default, so the form never
	// shows an empty box for a setting that is effectively true.
	Value any `json:"value"`
	// NeedsRestart is the row's own answer (settingApply.needsRestart): the form puts ⟳ beside
	// these and nothing else. It is not a flag anyone sets.
	NeedsRestart bool `json:"needsRestart"`
	// Category is the row's own answer (settingApply.Category): which group of the form the
	// field shows up under. The frontend groups by this name and never derives it from the
	// key — "where does 日志 live" is not a question the dot path answers.
	Category string `json:"category"`
	// Options is the closed set this leaf's value comes from, when it has one: the provider
	// names this config defines, or the provider TYPES, for the keys that name one (see
	// settingChoices).
	//
	// Empty means "no declared set", which is NOT the same as "nothing is legal" — it is the
	// free-text case, and it is also what a config with no providers at all yields
	// (providerNames). The page renders a dropdown only when there is something to choose, so
	// the two are told apart by the caller seeing zero options and falling back.
	//
	// A value the list does not contain is still legal — the form keeps it as its own entry
	// rather than silently switching the field to something else.
	Options []string `json:"options,omitempty"`
	// Optional says the leaf can be left UNSET, and that being unset MEANS something: these are
	// the fields whose nil is a third state rather than an empty one (`vision: nil` = decide it
	// from the model, `max_retries: nil` = the path's default). The form draws 自动 for them, and
	// they are the only fields it can clear back to it.
	Optional bool `json:"optional,omitempty"`
	// Secret is config.LeafInfo's: this leaf holds a credential (`api_key`, a provider's `key`),
	// so the form draws it masked and puts a 眼睛 beside it. Derived from the leaf's yaml name
	// (config's secretLeafNames) rather than declared here, for the same reason Kind is: the
	// leaf name already fixes the field, and a second declaration is a second thing to get wrong.
	Secret bool `json:"secret,omitempty"`
	// IsSet says whether the leaf currently holds a value. `Optional && !IsSet` is the 自动
	// state; a leaf that is not optional is always set. It cannot be read off Value: an unset
	// pointer WITH a default reads as that default (see config.LeafInfo).
	IsSet bool `json:"isSet"`
	// Note is why: the reason a restart is needed, or which entry mode owns the key instead.
	Note string `json:"note,omitempty"`
	// ListExample is the row's own answer (settingApply.ListExample): the example values a
	// `string_list` box shows under its "每行一项" hint, one per line. Empty means no example,
	// which is what a list whose shape the page cannot guess gets.
	ListExample string `json:"listExample,omitempty"`
}

// SettingsSchemaVO is everything the settings page needs to draw itself, in one answer: the
// fields, and the order the left column lists their groups in.
//
// The order rides along rather than being declared in the frontend for the same reason the
// categories do (see settingsCategoryOrder): a second list is a second thing to keep in step,
// and a field whose category the frontend does not know would be a field it silently drops.
type SettingsSchemaVO struct {
	// Categories is settingsCategoryOrder: every group, in display order. All of them are
	// non-empty — the guard test fails if one has no field, and the page's nav is this list.
	Categories []string `json:"categories"`
	// Fields is one entry per editable leaf of the live config, in path order. The form
	// groups them; nothing here is sorted by category, because within a group the natural
	// order is the config's own.
	Fields []SettingFieldVO `json:"fields"`
	// Lists are the config lists the page may ADD items to (config.ListItemPaths, filtered to the
	// rows this process offers).
	//
	// Declared rather than left for the form to infer from the fields, because an EMPTY list has
	// no fields: without this the page could not offer the first item of a list, which is exactly
	// when someone wants it.
	Lists []SettingListVO `json:"lists"`
	// FileSources maps a list item's own path (`providers.0`) to the FILE that item lives in
	// (`provider.deepseek.json`), for the lists whose items each come from their own file.
	//
	// It rides here rather than on SettingFieldVO because it is a fact about the ITEM, not about
	// a leaf: every leaf of one provider is in the same file, and a page that showed the file
	// eleven times inside one card would be saying one thing eleven ways. Only `providers` has it
	// today — MCP servers have the same idea but their editor is a block of its own
	// (MCPServersEditor), not a list of fields.
	//
	// An item the page has just ADDED has no entry: it is not in any file yet.
	FileSources map[string]string `json:"fileSources,omitempty"`
	// MCPServerEditor is the CATEGORY the page draws the MCP servers editor in.
	//
	// That editor is not a row: MCP servers live in mcp.json, not config.yaml (`MCPServers` is
	// `yaml:"-"`), so it has no leaf to hang off and does not go through SaveSettings — it writes
	// its own file the moment the user saves an entry. Which GROUP it belongs to is still a claim
	// about the group, and the groups are declared here (settingsCategoryOrder), so the page is
	// told rather than left to match on a name.
	MCPServerEditor string `json:"mcpServerEditor"`
	// MobilePairing is the CATEGORY the page draws the phone pairing card in.
	//
	// Same reasoning as MCPServerEditor: the card is not a config leaf (its payload is a
	// credential that lives in ~/.tachi/mobile.json, and its data comes from a binding rather
	// than from the form), but WHICH GROUP it belongs to is a claim about the groups, and those
	// are declared here. Empty would mean "this build has no such card".
	MobilePairing string `json:"mobilePairing"`
}

// ListEdits is config's own vocabulary for the structural half of a save, aliased here so this file
// reads the way the design does without a package prefix on every field.
type ListEdits = config.ListEdits

// SettingListVO is one list the settings page may grow: the list's own path, the group it is drawn
// in (which is the category of the row that owns it — `web_search.providers` belongs to
// `web_search`'s row, not to a row of its own), and the two facts a page needs in order to draw an
// item that is not in the config yet.
type SettingListVO struct {
	Path     string `json:"path"`
	Category string `json:"category"`
	// Folded says this list's items are drawn folded whatever their size (settingFoldedLists).
	// It rides on the list rather than on the field because it is a fact about the LIST: within
	// one list every item is the same struct, so their sizes agree and the decision cannot differ
	// per item. The page matches an item to its list by path — the item path IS the list path
	// plus an index — so nothing has to be repeated per card.
	Folded bool `json:"folded,omitempty"`
	// Length is how many items the list holds NOW (config.ListLen). It is what lets the page name
	// the item it is about to add: that path is this length, plus however many items of its own
	// list the page is already holding. The arithmetic is the page's — its unsaved state is its own
	// (see Element) — and config.ListEdits checks the answer at save time (the element path it is
	// given must be the list's end).
	Length int `json:"length"`
	// Element is the TEMPLATE of one item: the leaves it has, how to draw each and what it starts
	// at (config.ElementFields — the element's declared shape, plus a defaulted scaffold's values).
	//
	// It rides on the list because it is the same for every item of it, and because it is the only
	// way to draw an item that does not exist: every other field the page draws comes from a value
	// the config holds (PathsFor walks the live config), and a new item has no value anywhere.
	Element []SettingElementFieldVO `json:"element"`
}

// SettingElementFieldVO is one leaf of a list's item template: SettingFieldVO minus the path,
// because the path is the page's to compose — `<list>.<index>.<tail>`, for an index only the page
// knows (it is the list's length plus its own pending items).
//
// A tail rather than a key is what makes the template reusable, and the rest of the fields are
// SettingFieldVO's for the same reason they are there: the page's draft/dirty/save machinery must
// treat a synthesized row exactly like a read one, or a new item's card would be a second kind of
// field with its own rules.
type SettingElementFieldVO struct {
	// Tail is the leaf's path inside the element (`name`, `spec.proxy`).
	Tail string `json:"tail"`
	// Kind, Value, Optional, IsSet, Secret, NeedsRestart, Category, Options and Note are
	// SettingFieldVO's own — see there for what each one means.
	Kind         string   `json:"kind"`
	Value        any      `json:"value"`
	Options      []string `json:"options,omitempty"`
	Optional     bool     `json:"optional,omitempty"`
	IsSet        bool     `json:"isSet"`
	Secret       bool     `json:"secret,omitempty"`
	NeedsRestart bool     `json:"needsRestart"`
	Category     string   `json:"category"`
	Note         string   `json:"note,omitempty"`
}

// listElementTemplate describes one item of the list at listPath: the leaves a new item will have,
// each with the row that speaks for it and the choices that leaf offers.
//
// The row and the choices are looked up against a SYNTHETIC index (`providers.0.<tail>`), which is
// not a shortcut: `settingFor` and `choiceFor` both read a numeric segment as "one element of that
// list" — `providers.*.type` is the same leaf whatever the index — so one lookup serves every item
// the page will ever add. A template per index would be the same answer spelled N times.
//
// An empty answer means the shape could not be described at all, which a path ListItemPaths named
// cannot do (it is a slice of structs by construction) — and the guard test would catch it either
// way: a declared list with no template is a ＋ that draws an empty card.
func listElementTemplate(cfg *config.Config, listPath string) []SettingElementFieldVO {
	leaves, err := config.ElementFields(listPath)
	if err != nil {
		return nil
	}
	probe := listPath + ".0"
	out := make([]SettingElementFieldVO, 0, len(leaves))
	for _, leaf := range leaves {
		key := probe + "." + leaf.Tail
		row, ok := settingFor(key)
		if !ok || !row.offered() {
			continue
		}
		var options []string
		if choose, ok := choiceFor(key); ok && leaf.Kind == "string" {
			options = choose(cfg)
		}
		out = append(out, SettingElementFieldVO{
			Tail: leaf.Tail, Kind: leaf.Kind, Value: leaf.Value, Options: options,
			Optional: leaf.Optional, IsSet: leaf.IsSet, Secret: leaf.Secret,
			NeedsRestart: row.needsRestart(), Category: row.Category, Note: row.Note,
		})
	}
	return out
}

// GetSettingsSchema enumerates the fields the settings page may offer, one per editable leaf of the
// live config.
//
// It walks the CONFIG's leaves (config.PathsFor) rather than the table's rows, because a row
// is often a block — `memory` speaks for several leaves, and a form needs one input per leaf,
// each with the block's mode and note. The two filters are the row's: a key no row covers cannot happen
// (the guard test in settings_test.go fails the build first), and an applyUnused row is another
// entry mode's key, so it is left out rather than offered as a switch that does nothing.
//
// A value that cannot be read is still listed, with the reason in Note: hiding a field would
// present a smaller settings page as a complete one, and the failure is the interesting part.
//
// The schema is a pure function of the CONFIG, and that is a property worth keeping: the page's own
// unsaved state (the item it is about to add, the one it dropped) is the page's, so a re-read never
// has to reproduce it — it hands over the two facts an item that does not exist yet needs (the
// list's Length and its Element template, see SettingListVO) and lets the page draw it.
func (s *AgentService) GetSettingsSchema() *SettingsSchemaVO {
	d := s.desk
	cfg := d.config()
	if cfg == nil {
		return nil
	}
	paths := config.PathsFor(cfg)
	fields := make([]SettingFieldVO, 0, len(paths))
	for _, key := range paths {
		row, ok := settingFor(key)
		if !ok || !row.offered() {
			continue
		}
		fields = append(fields, settingField(cfg, key, row))
	}
	// A fresh slice rather than settingsCategoryOrder itself: the binding serializes this, and
	// a caller that appended to it would be mutating the package's own list.
	return &SettingsSchemaVO{
		Categories:      append([]string(nil), settingsCategoryOrder...),
		Fields:          fields,
		Lists:           settingLists(cfg),
		FileSources:     itemFileSources(cfg),
		MCPServerEditor: catMCP,
		MobilePairing:   catExtend,
	}
}

// itemFileSources names the file each list item came from, for the items that HAVE one.
//
// Today that is `providers`, and the reason is the same one that moved them out of config.yaml:
// these entries live in files the reader can open, and "which file is this one in" is the first
// question a save raises ("where did my edit go?"). The path is the item's own path
// (`providers.0`), which is what a card is drawn from.
func itemFileSources(cfg *config.Config) map[string]string {
	if len(cfg.Providers) == 0 {
		return nil
	}
	out := make(map[string]string, len(cfg.Providers))
	for i, p := range cfg.Providers {
		if p.SourceFile == "" {
			continue
		}
		out[fmt.Sprintf("providers.%d", i)] = filepath.Base(p.SourceFile)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// settingField is one schema row: the table's answer for the key, plus everything that comes from
// the value and the leaf's declaration.
func settingField(cfg *config.Config, key string, row settingApply) SettingFieldVO {
	value, err := config.ReadPath(cfg, key)
	note := row.Note
	if err != nil {
		note = joinNotes(note, "读取失败："+err.Error())
	}
	// How the leaf is declared, which the value cannot say: an unset *bool holds nothing to
	// infer a control from, and `unset` is a state of its own for these fields.
	info, infoErr := config.DescribeLeaf(cfg, key)
	if infoErr != nil {
		// The same walk ReadPath just failed at; one sentence is enough.
		info.Kind = config.KindOf(value)
	}
	// A declared choice set is only offered on a leaf that can hold one: a dropdown on a
	// number would be a way to save a value the field refuses. (The guard test in
	// settings_test.go holds the table to config's own leaves, and to string leaves at that.)
	var options []string
	if choose, ok := choiceFor(key); ok && info.Kind == "string" {
		options = choose(cfg)
	}
	return SettingFieldVO{
		Key:          key,
		Kind:         info.Kind,
		Value:        value,
		NeedsRestart: row.needsRestart(),
		Category:     row.Category,
		Options:      options,
		Optional:     info.Optional,
		IsSet:        info.IsSet,
		Secret:       info.Secret,
		Note:         note,
		ListExample:  row.ListExample,
	}
}

// settingLists reports the config lists this page may add items to, each with the group it is
// drawn in and the two facts an item that does not exist yet needs (SettingListVO).
//
// The rows decide the group — a list belongs where the block that owns it is drawn, and a list
// whose row is not offered (another entry mode's key) is not offered either — while the length and
// the template come from the config itself: they are what the page draws its own pending item from.
func settingLists(cfg *config.Config) []SettingListVO {
	paths := config.ListItemPaths()
	out := make([]SettingListVO, 0, len(paths))
	for _, path := range paths {
		row, ok := settingFor(path)
		if !ok || !row.offered() {
			continue
		}
		length, err := config.ListLen(cfg, path)
		if err != nil {
			length = 0 // not reachable for a path ListItemPaths named; a wrong index is refused at save
		}
		out = append(out, SettingListVO{
			Path:     path,
			Category: row.Category,
			Folded:   slices.Contains(settingFoldedLists, path),
			Length:   length,
			Element:  listElementTemplate(cfg, path),
		})
	}
	return out
}

// SettingsRemovalVO is what the page is told when it drops an item: whether the drop is possible
// at all, and the one sentence to show.
//
// Two fields rather than a bare string for the same reason SaveSettingsResultVO has two: OK tells
// the page whether to REMOVE the card (it has not, and if this is false it must not), while Summary
// is the page's to print as it is. A drop that is fine may still have something to say (the keys
// still naming the item), so "OK with a sentence" is a real state.
type SettingsRemovalVO struct {
	OK      bool   `json:"ok"`
	Summary string `json:"summary"`
}

// SettingsRemovalWarning answers the two questions a page has when the reader drops a list item,
// and it is READ-ONLY: the removal itself is the page's own pending state, which joins the next
// save (see SaveSettings).
//
//  1. CAN it be dropped? A malformed path, or an index the list does not have, means the page and
//     the config disagree — and the answer has to come now, while the card is still on screen,
//     rather than at save time when the reader has already thrown it away.
//  2. WHAT still names it? A name a list item is known by (`providers.2.name`) may be written into
//     keys that mean "use that one" — `provider`, `title_provider`, each `*_provider` — and those
//     are not list items, so dropping the item leaves them pointing at nothing. Saying so is the
//     difference between a warning and a config that quietly stops resolving, and it is said HERE
//     because this is the moment the reader can act on it.
func (s *AgentService) SettingsRemovalWarning(elementPath string) *SettingsRemovalVO {
	d := s.desk
	cfg := d.config()
	if cfg == nil {
		return &SettingsRemovalVO{Summary: "配置尚未加载，改不了"}
	}
	if err := validateRemoval(cfg, elementPath, nil); err != nil {
		return &SettingsRemovalVO{Summary: "删不了：" + err.Error()}
	}
	name := ""
	if leaves, err := config.ListItemLeaves(cfg, elementPath); err == nil {
		if s, ok := leaves["name"].(string); ok {
			name = s
		}
	}
	dangling := danglingReferences(cfg, name)
	if len(dangling) == 0 {
		return &SettingsRemovalVO{OK: true}
	}
	return &SettingsRemovalVO{OK: true, Summary: fmt.Sprintf("注意 %s 还写着「%s」，它现在指向一个不存在的条目，保存前请改掉",
		strings.Join(dangling, "、"), name)}
}

// validateRemovals refuses a set of removals this save cannot perform, BEFORE anything is touched:
// a malformed path, a duplicate, or an index the list does not have.
//
// The removals are applied against the list as it is NOW (the appends that may run before them only
// add at the END, so no index moves), and they are applied ONE BY ONE — so the second removal of a
// path that is already spoken for would delete a second, unrelated element. That is not a
// hypothetical: a page that offers the same button twice (or a stale window) can send `providers.0`
// twice, and the pair silently drops two providers. Duplicates are therefore refused rather than
// deduplicated: the caller's intent is not knowable, and the honest answer is "this page is
// confused, reload it".
func validateRemovals(cfg *config.Config, removes []string) error {
	seen := make(map[string]bool, len(removes))
	for i, elementPath := range removes {
		if seen[elementPath] {
			return fmt.Errorf("%s 被标了两次要删", elementPath)
		}
		seen[elementPath] = true
		if err := validateRemoval(cfg, elementPath, removes[:i]); err != nil {
			return err
		}
	}
	return nil
}

// validateRemoval refuses one element path the list cannot drop. `before` is the removals of the
// SAME batch that are applied before it: they are already spoken for, so they are subtracted from
// the list's length — otherwise a batch that drops two items of one list would refuse the second
// one's index, which is legal against the list as the caller saw it.
func validateRemoval(cfg *config.Config, elementPath string, before []string) error {
	listPath, index, err := config.SplitElementPath(elementPath)
	if err != nil {
		return err
	}
	length, err := config.ListLen(cfg, listPath)
	if err != nil {
		return err
	}
	earlier := 0
	for _, p := range before {
		if other, _, err := config.SplitElementPath(p); err == nil && other == listPath {
			earlier++
		}
	}
	if index >= length-earlier {
		return fmt.Errorf("%s: 没有第 %d 项（这一批已经删了 %d 项，配置里还有 %d 项）",
			listPath, index, earlier, length)
	}
	return nil
}

// ownedLeaves collects the batch's leaves whose row writes them ITSELF (settingApply.OwnsWrite):
// they are the ones this save must not put into memory yet.
func ownedLeaves(rows map[string]settingApply, updates map[string]any) map[string]any {
	var out map[string]any
	for key, row := range rows {
		if !row.OwnsWrite {
			continue
		}
		if out == nil {
			out = map[string]any{}
		}
		out[key] = updates[key]
	}
	return out
}

// overlayOwnedLeaves lays those leaves over the config and returns the function that lifts them
// again — so the marshal in between writes what the user asked for while the running process is
// told nothing.
//
// The row's push is what writes them for real (and what refuses to, when the switch it represents
// cannot be made); the file must hold them either way, because that is what "saved" means. The two
// orders meet here and nowhere else.
//
// The returned function is never nil and is safe to defer unconditionally.
func overlayOwnedLeaves(cfg *config.Config, leaves map[string]any) func() {
	if len(leaves) == 0 {
		return func() {}
	}
	keys := make([]string, 0, len(leaves))
	for key := range leaves {
		keys = append(keys, key)
	}
	snap, err := config.SnapshotLeaves(cfg, keys)
	if err != nil {
		// A leaf this batch resolved but cannot be read back would be a bug in the path API, and
		// there is nothing to lift: the marshal writes the value the object already holds.
		return func() {}
	}
	if err := config.ApplyUpdates(cfg, leaves); err != nil {
		config.RestoreLeaves(snap)
		return func() {}
	}
	return func() { config.RestoreLeaves(snap) }
}

// danglingReferences names the keys that currently hold this item's name, for the warning a
// removal reports.
//
// It walks settingChoices — the keys whose value is a CHOICE from a set — because that is exactly
// the set of keys that can point at a list item. A structural key (`providers.*.type`) is skipped:
// its choices are the list's own vocabulary, not a reference to a particular item, and comparing a
// user's provider name against them would warn about the wrong thing.
func danglingReferences(cfg *config.Config, name string) []string {
	if name == "" {
		return nil
	}
	var out []string
	for key := range settingChoices {
		if strings.Contains(key, "*") {
			continue
		}
		value, err := config.ReadPath(cfg, key)
		if err != nil {
			continue
		}
		if s, ok := value.(string); ok && s == name {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// normalizeUnsets turns the PAGE's null into config.Unset, for the leaves that can take one.
//
// The two are not the same thing to the rest of this pipeline: config refuses a bare nil on
// purpose (it is what a caller who forgot to dereference a pointer looks like, and writing that
// would silently clear a setting), while Unset is a value that means it. A form needs to say
// "unset" (its 自动 state), so the translation happens at the boundary where the form's
// vocabulary lives — and only for a leaf that can be unset at all.
//
// A nil for any other leaf is left alone: the type check refuses it with the sentence that says
// what a nil is, which is the right answer for a leaf whose field has no null to write.
//
// It mutates updates in place: that map came from this call's own binding, and the steps after
// need to see what config will actually be given.
func normalizeUnsets(cfg *config.Config, updates map[string]any) {
	for key, v := range updates {
		if v != nil {
			continue
		}
		info, err := config.DescribeLeaf(cfg, key)
		if err != nil {
			continue // an unreadable path is reported by the step that resolves it
		}
		if info.Optional {
			updates[key] = config.Unset{}
		}
	}
}

// resolveBatch is the pre-flight half of EVERY batch this page sends — the save and the preview
// run it before either touches anything, so a preview cannot promise something the save then
// refuses (nor the other way round: the reader would have been shown "nothing to change" for a
// batch that is about to be rejected).
//
//  1. every key must be one this page owns: an unknown key, or one another entry mode owns,
//     refuses the whole batch before anything is touched. A half-saved form is a state no reader
//     can reason about.
//  2. the structural half has to be doable at all: a page that has been open a while can name an
//     index the list no longer has, or the same one twice (see validateRemovals). The appends are
//     checked by the code that creates them — the list's own length decides it.
//
// Returns the rows of the keys it accepted (and those keys, sorted — the order every later step
// speaks in), or the sentence to show instead of doing anything.
func resolveBatch(cfg *config.Config, edits config.ListEdits) (map[string]settingApply, []string, string) {
	keys := make([]string, 0, len(edits.Updates))
	for key := range edits.Updates {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	rows := make(map[string]settingApply, len(keys))
	for _, key := range keys {
		row, ok := settingFor(key)
		if !ok {
			return nil, nil, fmt.Sprintf("未知配置项：%s", key)
		}
		if !row.offered() {
			return nil, nil, fmt.Sprintf("%s 属于别的入口模式，页面不提供（%s），改不了", key, row.Note)
		}
		rows[key] = row
	}
	if err := validateRemovals(cfg, edits.Removes); err != nil {
		return nil, nil, "改不了：" + err.Error()
	}
	return rows, keys, ""
}

// SaveSettingsResultVO is what the page gets back from a save: whether anything was written,
// and the one sentence to show.
//
// Two fields rather than a bare string, because the page does two different things with the
// answer: a save that WROTE clears the form and re-reads every value (they are the new ones),
// while a save that was REFUSED has to leave the reader's typing alone — they need to fix the
// value, and a form that threw away a whole batch over one typo would be worse than no form.
// The sentence stays a sentence (the desktop's convention for bound methods: the frontend does
// not translate Go errors), so nothing here asks the page to read the text.
type SaveSettingsResultVO struct {
	// OK is true when the file was written: every value is in config.yaml, and the running
	// process was told everything that can be told. False means nothing moved anywhere — not
	// memory, not the file, not the running process.
	OK bool `json:"ok"`
	// Summary is the sentence the page shows as it is: what took effect, what waits for a
	// restart, what waits for a turn to end, and which session refused what.
	Summary string `json:"summary"`
}

// SaveSettings applies a batch of edited leaves — plus the list items the page is adding and
// dropping — and tells the running process about them.
//
// The three arguments are ONE transaction, which is the whole point: the page's unsaved state is
// the page's own (a draft, an item it is about to add, an item it just dropped), and this is where
// all of it becomes real at once. `appends` are the element paths the page DREW (`providers.3`),
// and `removes` are the elements it dropped; both are checked against the config here, because a
// page that has been open a while is the only thing in this system that can hold a stale view.
//
// The order is the design's (§六), and each step exists because of what the previous one can
// leave behind:
//
//  1. resolve EVERY key first — an unknown key, or one another entry mode owns, refuses the
//     whole batch before anything is touched. A half-saved form is a state no reader can
//     reason about. The structural half is checked in the same spirit (see validateRemovals).
//  2. turn the page's null into config.Unset for the leaves that can take one (normalizeUnsets):
//     the form's 自动 is the only thing that can say "unset", and a bare nil stays refused.
//  3. snapshot the leaves and apply them in one locked step (all or none), together with the
//     STRUCTURAL half: the list items the page is adding and dropping are appended and removed
//     here, in the order config.ListEdits documents (appends, then the leaf updates, then the
//     removals — only that order addresses the elements the form meant). The type check is this
//     step's own: config.SetPath refuses a value the field cannot hold, and the batch is put back
//     whole.
//  4. write config.yaml, as ONE write of the WHOLE config (config.Save): the file is the object
//     marshalled, so there is no per-key half to keep in step. Only a save that got this far
//     continues: if the write fails, the memory is put back exactly as it was and the answer says
//     so — "applied for this session but not saved" is a state the next restart contradicts, and
//     the reader cannot tell which of the two they have.
//  5. push each row's `Apply` — the runtime half, and LAST, because a push has no undo: it
//     rebuilds tools, moves a process-wide log level, swaps backends, reconnects servers. Run
//     before the write, a failure at step 4 would leave the running process on values the file
//     (and the restored memory) does not have, while the answer promised the opposite. Its own
//     failures are per-session and reported, not fatal — the value IS saved.
//
// The whole sequence is serialized by d.settingsMu: two saves landing at once would otherwise
// interleave their snapshots, config writes and pushes.
//
// Returns whether the batch was written, plus the sentence the page shows (the desktop's
// convention for bound methods: a sentence rather than an error the frontend would have to
// translate). The two are not redundant — see SaveSettingsResultVO.
func (s *AgentService) SaveSettings(updates map[string]any, appends, removes []string) *SaveSettingsResultVO {
	d := s.desk
	d.settingsMu.Lock()
	defer d.settingsMu.Unlock()

	cfg := d.config()
	if cfg == nil {
		return &SaveSettingsResultVO{OK: false, Summary: "配置尚未加载，改不了"}
	}
	edits := ListEdits{Updates: updates, Appends: appends, Removes: removes}
	if edits.IsEmpty() {
		return &SaveSettingsResultVO{OK: false, Summary: "没有要保存的改动"}
	}

	// 1. Every key must be one this page owns — and the structural half has to be doable at all.
	// One pre-flight, shared with the PREVIEW (see resolveBatch): two copies is how a preview comes
	// to promise something this refuses.
	rows, keys, refusal := resolveBatch(cfg, edits)
	if refusal != "" {
		return &SaveSettingsResultVO{OK: false, Summary: refusal}
	}

	// 2. The page's null means 自动 on a leaf that has that state — see normalizeUnsets. It has to
	// happen before the values are applied, because what the apply and the write both need to see is
	// what config will actually be given.
	normalizeUnsets(cfg, updates)

	// 3. In memory first: a batch is all-or-nothing. The value's own type is what validates it here
	// (config.SetPath refuses what the field cannot hold), and ApplyUpdates puts back every leaf it
	// already wrote if a later one fails — so a bad value never reaches the file, and "validated but
	// not applied" is not a state this leaves behind.
	// A row that OWNS the write of its leaf (settingApply.OwnsWrite) is deliberately NOT in the
	// MEMORY batch: its push writes the value itself, at the moment the runtime can actually take it,
	// so there is one writer of that leaf and a failed push leaves it alone. It is still written to
	// the FILE by this save — that is what "saved" means for every row, and the file carries the
	// whole config (see step 4).
	//
	// The snapshot covers the structural half too, and it has to be taken BEFORE it: putting a
	// config back means truncating the lists that grew and re-inserting what was dropped, which is
	// only possible from what was recorded first.
	//
	// A leaf of an item this same batch APPENDS is an ORDINARY update: the appends below run first,
	// so by the time the updates land the element exists and the path resolves. Order is the whole
	// mechanism — one object means there is nothing else to keep in step.
	batched := make(map[string]any, len(updates))
	for _, key := range keys {
		if rows[key].OwnsWrite {
			continue
		}
		batched[key] = updates[key]
	}
	memoryEdits := ListEdits{Updates: batched, Appends: edits.Appends, Removes: edits.Removes}
	var snap *config.ListEditSnapshot
	if !memoryEdits.IsEmpty() {
		var snapErr error
		d.withConfig(func(c *config.Config) {
			snap, snapErr = config.SnapshotListEdits(c, memoryEdits)
		})
		if snapErr != nil {
			return &SaveSettingsResultVO{OK: false, Summary: "改不了：" + snapErr.Error()}
		}
		var applyErr error
		d.withConfig(func(c *config.Config) {
			applyErr = config.ApplyListEdits(c, memoryEdits)
		})
		if applyErr != nil {
			d.withConfig(func(c *config.Config) { config.RestoreListEdits(c, snap) })
			return &SaveSettingsResultVO{OK: false, Summary: "改不了：" + applyErr.Error() + "（已撤销，配置文件未动）"}
		}
	}

	// 4. The file: it is what survives the process, and what it holds is the WHOLE config — the file
	// IS the config object, marshalled (config.Save), so a key this page does not own rides along
	// untouched instead of having to be named in the batch, and a row that owns its write is written
	// here like every other leaf.
	//
	// That is the trade: the file stops being a hand-written document, so its comments and its own
	// layout do not survive (config.Save's doc says so; backupOnce keeps one copy of the original).
	// What it buys is that there is nothing left to keep in step.
	//
	// The marshal happens UNDER the lock, like every other read and write of the object: a
	// concurrent writer (the MCP profile switch) must not be mid-way through it.
	//
	// A row that OWNS its leaf (OwnsWrite) is the one value the marshal cannot carry by itself: it
	// reaches MEMORY during its push, which has not run yet — because a switch that fails must leave
	// the field naming the profile that is actually loaded. The FILE still has to hold what the user
	// asked for, so those leaves are laid over the object for this one marshal and lifted again
	// before the push writes them for real (see overlayOwnedLeaves).
	// provider 定义不在 config.yaml 里（它们在自己的 provider.*.json 文件里，见
	// config/provider_json.go），所以那半边要单独写：这次保存改过的 provider 叶子此刻已经在
	// 内存里，把这些条目按各自的来源整份写回文件就是它。写失败时这里把内存也一并撤回，跟下面
	// config.yaml 写失败是同一种处理。
	providerDir := ""
	if cfgPath, err := config.ConfigPath(); err == nil {
		providerDir = filepath.Dir(cfgPath)
	}
	var saveErr error
	d.withConfig(func(c *config.Config) {
		lift := overlayOwnedLeaves(c, ownedLeaves(rows, updates))
		defer lift()
		undo, provErr := config.SyncProviderFiles(providerDir, c.Providers)
		if provErr != nil {
			saveErr = provErr
			return
		}
		if saveErr = config.Save(c); saveErr != nil {
			// 两份文件是同一次保存的两半：yaml 没写成，provider 文件也不该留着新内容。
			undo()
		}
	})
	if saveErr != nil {
		if !memoryEdits.IsEmpty() {
			d.withConfig(func(c *config.Config) { config.RestoreListEdits(c, snap) })
		}
		return &SaveSettingsResultVO{OK: false, Summary: "没能写入配置文件：" + saveErr.Error() + "（这次改动已撤销，内存与文件都没变，运行中的进程也未被通知）"}
	}
	// 5. The runtime half: everything already built that keeps its own copy of these values.
	// Failures are per-session and the value IS saved, so they are reported, not rolled back.
	//
	// `pushing` marks this step: the fan-out queues a push for a session whose turn is in flight
	// (see deferred_push.go), and those sessions are reported in the summary instead of being
	// counted as already in effect.
	var pushFailures, deferredKeys []string
	d.deferredThisSave = nil
	d.pushing = true
	d.savingUpdates = updates
	for _, key := range keys {
		row := rows[key]
		if row.Apply == nil {
			continue // nothing built yet that needs telling (or a key nothing consumes)
		}
		before := len(d.deferredThisSave)
		if err := row.Apply(d, cfg); err != nil {
			pushFailures = append(pushFailures, pushError(key, []string{err.Error()}).Error())
		}
		if len(d.deferredThisSave) > before {
			deferredKeys = append(deferredKeys, key)
		}
	}
	d.pushing = false
	d.savingUpdates = nil

	// The summary: what took effect, what waits for a restart, what waits for a turn to end, and
	// what some session refused. A deferred key is NOT counted as already in effect — it is in
	// the file, and the running process will have it when the turn it is in finishes.
	deferred := make(map[string]bool, len(deferredKeys))
	for _, key := range deferredKeys {
		deferred[key] = true
	}
	var live, restart []string
	for _, key := range keys {
		switch {
		case rows[key].needsRestart():
			restart = append(restart, key)
		case deferred[key]:
		default:
			live = append(live, key)
		}
	}
	var parts []string
	if len(live) > 0 {
		parts = append(parts, fmt.Sprintf("%d 项已生效", len(live)))
	}
	if len(restart) > 0 {
		parts = append(parts, fmt.Sprintf("%d 项需重启后生效（%s）", len(restart), strings.Join(restart, "、")))
	}
	if len(deferredKeys) > 0 {
		parts = append(parts, fmt.Sprintf("%d 项等这一轮结束生效（%s）：会话%s正在运行",
			len(deferredKeys), strings.Join(deferredKeys, "、"), strings.Join(d.deferredSessionLabels(), "、")))
	}
	if len(pushFailures) > 0 {
		parts = append(parts, "有会话没能跟着更新："+strings.Join(pushFailures, "；"))
	}
	if len(parts) == 0 {
		return &SaveSettingsResultVO{OK: true, Summary: "已保存"}
	}
	return &SaveSettingsResultVO{OK: true, Summary: strings.Join(parts, "；")}
}

// SettingsPreviewVO is what the reader sees when they ask what 保存 is about to do: the lines the
// save would add and drop in config.yaml, and the one thing a diff of it cannot show.
type SettingsPreviewVO struct {
	// OK is true when the batch is one the save would accept. False means it would be REFUSED, and
	// Summary says why — the same sentence the save itself would answer with, because both run the
	// same pre-flight (resolveBatch). That is worth more than it looks: a reader can find out that a
	// key is not theirs, or that an item cannot be dropped, without touching the file.
	OK bool `json:"ok"`
	// Summary is the refusal, or "" when there is something to show.
	Summary string `json:"summary"`
	// Hunks are the file's changed lines in the app's one diff model (pkg/linediff), so the page
	// renders them with the same renderer every other diff uses.
	Hunks []linediff.Hunk `json:"hunks"`
	// Added / Removed count those hunks (linediff.Counts — the same numbers the diff pane shows).
	Added   int `json:"added"`
	Removed int `json:"removed"`
	// Rewrites is the fact the diff CANNOT carry: config.yaml on disk is not what this process
	// would write, so saving rewrites the whole document — the reader's comments, blank lines and
	// hand layout do not survive it (see config.Save, and backupOnce, which keeps ONE copy of the
	// original as config.yaml.bak). False once the file is the marshalled form, which is the state
	// every file reaches after its first save from this page.
	//
	// It is a PREDICATE rather than a sentence because it is a fact about the file; the page decides
	// how to warn (it also has the diff right there to point at).
	Rewrites bool `json:"rewrites"`
	// Files are the changes that land in the OTHER files of a save — the provider.*.json ones
	// (`providers` lives there, see config/provider_json.go). Empty when this batch touches none,
	// which is most saves.
	//
	// A separate list rather than one merged diff, because the reader's question ("what is about to
	// change") has one answer PER FILE and these are files they can open by that name. Hunks stays
	// config.yaml's own diff.
	Files []SettingsFilePreviewVO `json:"files,omitempty"`
}

// SettingsFilePreviewVO is one non-config.yaml file a save would write, and what would change in
// it.
type SettingsFilePreviewVO struct {
	// Name is the file's base name (`provider.deepseek.json`). The page has no use for a full path
	// — these all live in one directory — and a base name is what the reader would go and open.
	Name string `json:"name"`
	// New says the file does not exist yet: an entry the page just added, whose name picked a file
	// of its own. It has no left side to diff, so Hunks holds the whole file as additions.
	New bool `json:"new"`
	// Deleted says the file would go away: every provider it defined was removed. `name` is then
	// the only thing there is to show.
	Deleted bool `json:"deleted"`
	// Hunks are the changed lines (linediff — the renderer every other diff uses).
	Hunks []linediff.Hunk `json:"hunks,omitempty"`
	// Added / Removed count them.
	Added   int `json:"added"`
	Removed int `json:"removed"`
}

// diffPreviewContext is how much of the generated file the preview keeps around each change. Three
// is `git diff`'s own default, and the reason holds here too: enough to see what the changed line
// belongs to (its neighbours, the item it sits in), little enough that the change is what the
// reader sees first.
const diffPreviewContext = 3

// PreviewSettings answers "what will pressing 保存 write?": the same batch, through the same
// pre-flight and the same apply, MARSHALLED instead of written — and then put back.
//
// It diffs the GENERATED file against the generated file, not against the bytes on disk: the file
// is a projection of the config object (config.Save marshals it whole), so a diff against the
// reader's hand-written document would be mostly the document's own comments and layout — noise
// that buries the two lines they are about to change, and noise that is not this save's doing
// (defaults and provider-alias expansion are applied in memory at load, so the first save would
// "change" keys nobody touched). What that rewrite DOES cost the reader is Rewrites, said in words
// and only when it is true.
//
// The provider files are the exception in the other direction: they have no generated projection to
// diff against (they ARE the store), so their left side is what is on disk — see
// previewProviderFiles.
//
// The batch is applied to the LIVE config and rolled back, rather than to a copy: a copy would have
// to be built by marshalling and re-reading, and both of those go through the same code the answer
// is about — a copy that marshals differently from the object would make the preview lie in the one
// direction that matters. Rollback is SnapshotListEdits/RestoreListEdits, the pair the save itself
// uses when the file write fails, so what this sends is exactly what a save would send.
//
// It holds d.settingsMu like the save does — it mutates the config, so it must not interleave with
// one — and it is READ-ONLY from the outside: nothing is written, and nothing is pushed.
func (s *AgentService) PreviewSettings(updates map[string]any, appends, removes []string) *SettingsPreviewVO {
	d := s.desk
	d.settingsMu.Lock()
	defer d.settingsMu.Unlock()

	cfg := d.config()
	if cfg == nil {
		return &SettingsPreviewVO{Summary: "配置尚未加载，改不了"}
	}
	providerDir := ""
	if cfgPath, err := config.ConfigPath(); err == nil {
		providerDir = filepath.Dir(cfgPath)
	}
	edits := config.ListEdits{Updates: updates, Appends: appends, Removes: removes}
	if edits.IsEmpty() {
		return &SettingsPreviewVO{OK: true, Summary: "没有要保存的改动"}
	}
	rows, keys, refusal := resolveBatch(cfg, edits)
	if refusal != "" {
		return &SettingsPreviewVO{Summary: refusal}
	}
	// The page's null means 自动 on a leaf that has that state — the save's step 2, and it has to
	// happen before anything is marshalled, because what the diff shows is what config will be
	// given. (It writes into the caller's map, exactly as the save does; each call arrives with its
	// own.)
	normalizeUnsets(cfg, updates)

	// The save's step 3, split the same way and for the same reason (see SaveSettings): a row that
	// OWNS its leaf is NOT applied to memory — its push writes it later, when the runtime can take
	// it — while the FILE carries it like every other value. So it is laid over the object for this
	// one marshal and lifted again, or the preview would show "no change" for the very key whose
	// change the reader is asking about.
	batched := make(map[string]any, len(updates))
	for _, key := range keys {
		if rows[key].OwnsWrite {
			continue
		}
		batched[key] = updates[key]
	}
	applied := config.ListEdits{Updates: batched, Appends: appends, Removes: removes}

	var before, after []byte
	var providerFiles []SettingsFilePreviewVO
	var previewErr error
	d.withConfig(func(c *config.Config) {
		// The file as this process would write it RIGHT NOW is the diff's left side…
		if before, previewErr = config.Marshal(c); previewErr != nil {
			return
		}
		// …and the same thing with the batch applied is the right side. All of it under ONE
		// withConfig: the write lock is what keeps a concurrent reader (or the MCP profile switch,
		// which writes the config too) from seeing the config mid-batch. An EMPTY applied edit is
		// the normal case for the keys that own their write, and it still needs the overlay below.
		snap, err := config.SnapshotListEdits(c, applied)
		if err != nil {
			previewErr = err
			return
		}
		if err := config.ApplyListEdits(c, applied); err != nil {
			config.RestoreListEdits(c, snap)
			previewErr = err
			return
		}
		lift := overlayOwnedLeaves(c, ownedLeaves(rows, updates))
		after, previewErr = config.Marshal(c)
		// The provider files are the other half of the same save. Their left side is what is ON
		// DISK (unlike config.yaml, there is no in-memory projection of them): the files ARE the
		// store, and the plan below is exactly what the save would write.
		if previewErr == nil {
			providerFiles, previewErr = previewProviderFiles(providerDir, c.Providers)
		}
		lift()
		config.RestoreListEdits(c, snap)
	})
	if previewErr != nil {
		return &SettingsPreviewVO{Summary: "改不了：" + previewErr.Error()}
	}

	// The one fact the two marshals cannot show. Read outside the lock: the file is not this
	// process's memory, and SaveTo compares the same two things before deciding to write at all.
	rewrites := false
	if path, err := config.ConfigPath(); err == nil {
		if current, err := os.ReadFile(path); err == nil {
			rewrites = !bytes.Equal(current, before)
		}
	}
	// The diff is a WHOLE-FILE one with a few lines of context (DiffContext, not Fragments): the
	// change is what the reader asked for, and Fragments would hand back the whole config the moment
	// two leaves changed in different places (it trims only the common prefix and suffix).
	hunks := linediff.DiffContext(string(before), string(after), diffPreviewContext)
	added, removed := linediff.Counts(hunks)
	return &SettingsPreviewVO{OK: true, Hunks: hunks, Added: added, Removed: removed, Rewrites: rewrites, Files: providerFiles}
}

// previewProviderFiles is the provider half of a preview: for every file the save would write,
// diff what is on disk against what the plan would put there; for every file it would delete, say
// so. Only files that actually differ are reported — a save that changed `language` and nothing
// else produces no provider entries, which is what keeps the preview about the reader's change.
//
// It reads the DISK rather than an in-memory copy, because for these files there is no copy: the
// directory is the store (config.LoadProviders reads it at every load). That also means this must
// stay read-only — it computes a plan and lets it go.
func previewProviderFiles(dir string, providers []config.ProviderConfig) ([]SettingsFilePreviewVO, error) {
	plan, err := config.PlanProviderFiles(dir, providers)
	if err != nil {
		return nil, err
	}
	if plan.Empty() {
		return nil, nil
	}
	out := make([]SettingsFilePreviewVO, 0, len(plan.Files)+len(plan.Deletes))
	for _, path := range plan.Paths() {
		vo := SettingsFilePreviewVO{Name: filepath.Base(path)}
		current, readErr := os.ReadFile(path)
		vo.New = readErr != nil && errors.Is(readErr, os.ErrNotExist)
		if next, ok := plan.Files[path]; ok {
			if !vo.New && string(current) == string(next) {
				continue // 内容没变，不该出现在预览里
			}
			vo.Hunks = linediff.DiffContext(string(current), string(next), diffPreviewContext)
			vo.Added, vo.Removed = linediff.Counts(vo.Hunks)
		} else {
			vo.Deleted = true
			vo.Hunks = linediff.DiffContext(string(current), "", diffPreviewContext)
			_, vo.Removed = linediff.Counts(vo.Hunks)
		}
		out = append(out, vo)
	}
	return out, nil
}

// deferredSessionLabels names the sessions whose push waited for a turn to end, in the summary's
// own words ("「标题」", falling back to the id). Read by the saving goroutine, which is the only
// writer of deferredThisSave.
func (d *desktopApp) deferredSessionLabels() []string {
	out := make([]string, 0, len(d.deferredThisSave))
	for _, id := range d.deferredThisSave {
		if t := d.sessionTitle(id); t != "" {
			out = append(out, "「"+t+"」")
			continue
		}
		out = append(out, id)
	}
	return out
}
