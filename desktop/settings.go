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
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/config"
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
	// catExtend is the subsystems an agent is BUILT with — LSP, memory, subagents, hooks.
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

	// --- sessions ---
	{Key: "session_cleanup_max_count", Category: catGeneral, Mode: applyLazy, Note: "建新会话时读；已在运行的会话 manager 保留旧上限"},

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
		Apply: func(d *desktopApp, _ *config.Config) error {
			return pushError("权限策略", d.applyToLiveRuns(func(r liveRun) error {
				// SetPermissionRoot is the same rebuild a tree change already goes through: it
				// compiles a fresh policy from the LIVE config against this session's own tree
				// (project-level rules live there) and inherits the session's approvals. The
				// engine needs no new path — only the trigger was missing.
				r.Agent.SetPermissionRoot(context.Background(), d.sessionWorkDir(r.ID))
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
			added, removed := d.mcp.RecomputeAutoLoad(cfg)
			if len(added) == 0 && len(removed) == 0 {
				return nil
			}
			return pushError("MCP 工具搜索", d.applyToLiveAgents(func(a *agent.AIAgent) error {
				for _, dt := range removed {
					a.UnregisterTool(dt.Name)
				}
				for _, dt := range added {
					a.RegisterTool(dt.Tool)
				}
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
		Key:      "lsp",
		Category: catExtend,
		Mode:     applyPush,
		Apply: func(d *desktopApp, cfg *config.Config) error {
			// Per session: the manager is built per agent, and ReloadLSP tears the old one down
			// first (its child processes would otherwise be orphaned).
			return pushError("LSP", d.applyToLiveAgents(func(a *agent.AIAgent) error {
				return a.ReloadLSP(context.Background(), cfg)
			}))
		},
	},
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
	{Key: "review", Category: catSession, Mode: applyLazy, Note: "ResolveReviewOptions(cfg) 每次命令现读"},
	// Registered unconditionally, reading the config on every generation (see GitReminder):
	// nothing has to be pushed, and nothing has to be rebuilt — which is the point, since the
	// collector cannot remove a reminder without taking every other reminder's state with it.
	{Key: "system_reminder", Category: catSession, Mode: applyLazy, Note: "git reminder 每次生成时现读配置"},

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

	// --- another entry mode's keys ---
	{Key: "tui", Mode: applyUnused, Note: "TUI 专属"},
	{Key: "channel", Mode: applyUnused, Note: "channel 模式的 IM 后端，desktop 不启用"},
	{Key: "cron", Mode: applyUnused, Note: "channel 模式的后台调度"},
	{Key: "dream", Mode: applyUnused, Note: "channel 模式的后台记忆整理"},
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
	"providers.*.type":        providerTypes,
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
// and that is true of a list a reader SCANS (`providers`, `lsp.servers`: pick the one you want and
// open it). A web_search / web_fetch provider is not scanned — it is three leaves, and what the
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
	// IsSet says whether the leaf currently holds a value. `Optional && !IsSet` is the 自动
	// state; a leaf that is not optional is always set. It cannot be read off Value: an unset
	// pointer WITH a default reads as that default (see config.LeafInfo).
	IsSet bool `json:"isSet"`
	// Note is why: the reason a restart is needed, or which entry mode owns the key instead.
	Note string `json:"note,omitempty"`
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
}

// ListEdits is config's own vocabulary for the structural half of a save, aliased here so this file
// reads the way the design does without a package prefix on every field.
type ListEdits = config.ListEdits

// pendingAdd is one staged list item: where it will land (`providers.3`), and nothing else.
//
// The index is the whole content because that is all the staging has to say: the item's VALUES are
// leaf updates like any other (the schema declares the element's leaves, so the form draws them for
// an element that is not in the config yet), and the element is created by the append that runs
// before those updates in the same batch.
type pendingAdd struct {
	Element string
}

// SettingListVO is one list the settings page may grow: the list's own path, and the group it is
// drawn in (which is the category of the row that owns it — `web_search.providers` belongs to
// `web_search`'s row, not to a row of its own).
type SettingListVO struct {
	Path     string `json:"path"`
	Category string `json:"category"`
	// Folded says this list's items are drawn folded whatever their size (settingFoldedLists).
	// It rides on the list rather than on the field because it is a fact about the LIST: within
	// one list every item is the same struct, so their sizes agree and the decision cannot differ
	// per item. The page matches an item to its list by path — the item path IS the list path
	// plus an index — so nothing has to be repeated per card.
	Folded bool `json:"folded,omitempty"`
}

// GetSettingsSchema enumerates the fields the settings page may offer, one per editable leaf
// of the live config — PLUS one per leaf of every item the page has staged (see pendingAdd).
//
// It walks the CONFIG's leaves (config.PathsFor) rather than the table's rows, because a row
// is often a block — `lsp` speaks for eleven leaves, and a form needs eleven inputs, each with
// the block's mode and note. The two filters are the row's: a key no row covers cannot happen
// (the guard test in settings_test.go fails the build first), and an applyUnused row is another
// entry mode's key, so it is left out rather than offered as a switch that does nothing.
//
// A value that cannot be read is still listed, with the reason in Note: hiding a field would
// present a smaller settings page as a complete one, and the failure is the interesting part.
//
// The staged items are the exception that proves the rule. An item the reader just added is not in
// the config, so nothing walks its leaves — and it must still be EDITABLE, or "add an item" would
// draw a card with no inputs. They are therefore enumerated from the element type
// (config.ElementFields) and appended as ordinary fields: the page's draft/dirty/save machinery
// treats them like any other row, and SaveSettings carries them into the same batch as the appends.
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
	fields = append(fields, s.stagedFields(cfg)...)
	// A fresh slice rather than settingsCategoryOrder itself: the binding serializes this, and
	// a caller that appended to it would be mutating the package's own list.
	return &SettingsSchemaVO{
		Categories: append([]string(nil), settingsCategoryOrder...),
		Fields:     fields,
		Lists:      settingLists(),
	}
}

// stagedFields describes the leaves of every list item this page has staged: the fields of an
// element that is not in the config yet.
//
// Their KEY is the path the item will have (`providers.3.name`), which is what makes them ordinary
// fields — the save sends them like any other leaf, and the append that creates the element is what
// makes those paths exist by the time the same batch writes them (the append comes first, see
// config.ListEdits). Their VALUE is the scaffold's (the element type's defaults), because the
// element does not exist to have one.
//
// The row is resolved the same way an existing element's would be, through `settingFor`, so the
// staged leaves appear in the right group with the right ⟳ mark — the list's row speaks for the
// whole block. There is no separate declaration for "what a new item looks like", because there is
// nothing to declare: it is the same element type.
func (s *AgentService) stagedFields(cfg *config.Config) []SettingFieldVO {
	d := s.desk
	d.settingsMu.Lock()
	staged := append([]pendingAdd(nil), d.pendingAdds...)
	d.settingsMu.Unlock()
	if len(staged) == 0 {
		return nil
	}

	// One template per list, computed once: the leaves of an element are a property of its type,
	// and a page with three staged providers must not describe the same type three times.
	templates := make(map[string][]config.ElementField)
	var out []SettingFieldVO
	for _, add := range staged {
		listPath, _, err := config.SplitElementPath(add.Element)
		if err != nil {
			continue
		}
		tmpl, ok := templates[listPath]
		if !ok {
			tmpl, err = config.ElementFields(listPath)
			if err != nil {
				continue
			}
			templates[listPath] = tmpl
		}
		for _, leaf := range tmpl {
			key := add.Element + "." + leaf.Tail
			row, ok := settingFor(key)
			if !ok || !row.offered() {
				continue
			}
			// Built from the TEMPLATE, not through settingField: that helper reads the value and
			// describes the leaf against the config, and this element is not in the config — its
			// path genuinely does not resolve yet (which is the whole reason the template exists).
			// Only the row's own two facts come from the table, exactly as they do for a real one.
			out = append(out, SettingFieldVO{
				Key:          key,
				Kind:         leaf.Kind,
				Value:        leaf.Value,
				NeedsRestart: row.needsRestart(),
				Category:     row.Category,
				Optional:     leaf.Optional,
				IsSet:        leaf.IsSet,
				Note:         row.Note,
			})
		}
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
		Note:         note,
	}
}

// settingLists reports the config lists this page may add items to, each with the group it is
// drawn in. The rows decide that group — a list belongs where the block that owns it is drawn,
// and a list whose row is not offered (another entry mode's key) is not offered either.
func settingLists() []SettingListVO {
	paths := config.ListItemPaths()
	out := make([]SettingListVO, 0, len(paths))
	for _, path := range paths {
		row, ok := settingFor(path)
		if !ok || !row.offered() {
			continue
		}
		out = append(out, SettingListVO{
			Path:     path,
			Category: row.Category,
			Folded:   slices.Contains(settingFoldedLists, path),
		})
	}
	return out
}

// SettingsItemResultVO is what growing or shrinking a list reports: whether it happened, the one
// sentence to show, and — for an add — the new item's own path.
//
// The path is reported rather than left for the page to work out, because where the item landed is
// the LIST's fact, not the page's (the append position is its length). The page uses it to open
// the card it just made: the reason someone presses ＋ is to fill the new item in.
type SettingsItemResultVO struct {
	OK          bool   `json:"ok"`
	Summary     string `json:"summary"`
	ElementPath string `json:"elementPath,omitempty"`
}

// AddSettingsItem appends one item to a config list, IN THE BATCH THIS SAVE IS BUILDING.
//
// It does not write config.yaml, and that is the change: it used to, because an item had to be in
// the file before any of its fields could be saved (the update path refuses a path the file does
// not have). Staging the structure here means the save that follows carries the item AND its
// values into one file write (config.ListEdits, whose doc comment holds the ordering rules), so a
// reader who adds an item and changes their mind can walk away with nothing written — which is
// what the page's own 「返回对话」 implies and what the immediate write made impossible.
//
// Nothing is PUSHED here either, and that is not a new rule: an item with no values yet is nothing
// to tell the running process about. The fields the user fills in are pushed by the save that
// writes them.
func (s *AgentService) AddSettingsItem(listPath string) *SettingsItemResultVO {
	fail := func(summary string) *SettingsItemResultVO {
		return &SettingsItemResultVO{Summary: summary}
	}
	d := s.desk
	d.settingsMu.Lock()
	defer d.settingsMu.Unlock()
	if !slices.Contains(config.ListItemPaths(), listPath) {
		return fail(fmt.Sprintf("%s 不是可以添加条目的列表", listPath))
	}
	cfg := d.config()
	if cfg == nil {
		return fail("配置尚未加载，改不了")
	}
	// The index the item will land at, and the promise the save checks: the form is showing this
	// list as it is NOW, so anything else means the config moved under it.
	index, err := config.ListLen(cfg, listPath)
	if err != nil {
		return fail("加不了：" + err.Error())
	}
	elementPath := fmt.Sprintf("%s.%d", listPath, index)
	d.pendingAdds = append(d.pendingAdds, pendingAdd{Element: elementPath})
	return &SettingsItemResultVO{
		OK:          true,
		Summary:     fmt.Sprintf("已在 %s 末尾加了一项（%s）；填好字段后点保存，它会和其余改动一起写进 config.yaml", listPath, elementPath),
		ElementPath: elementPath,
	}
}

// RemoveSettingsItem removes one item from a config list, IN THE BATCH THIS SAVE IS BUILDING.
//
// Like AddSettingsItem it no longer writes: the removal joins the save, so dropping an item and
// then leaving the page writes nothing — the same promise, in the other direction.
//
// The references are still reported HERE rather than at save time, because this is the moment the
// reader can act on them: a name a list item is known by (`providers.2.name`) may be written into
// keys that mean "use that one" — `provider`, `title_provider`, each `*_provider` — and those are
// not list items, so removing the item leaves them pointing at nothing. Saying so is the difference
// between a warning and a config that quietly stops resolving.
func (s *AgentService) RemoveSettingsItem(elementPath string) *SettingsItemResultVO {
	fail := func(summary string) *SettingsItemResultVO {
		return &SettingsItemResultVO{Summary: summary}
	}
	d := s.desk
	d.settingsMu.Lock()
	defer d.settingsMu.Unlock()
	cfg := d.config()
	if cfg == nil {
		return fail("配置尚未加载，改不了")
	}
	// An item that is still only STAGED cancels out instead of being scheduled for removal: it was
	// never in the config, so there is nothing to delete — and validating it against the config
	// would refuse (its index does not exist yet), which is the one path a reader takes most often:
	// press ＋, look at the card, change their mind.
	if i := slices.IndexFunc(d.pendingAdds, func(a pendingAdd) bool { return a.Element == elementPath }); i >= 0 {
		d.pendingAdds = slices.Delete(d.pendingAdds, i, i+1)
		d.pendingRemoves = slices.DeleteFunc(d.pendingRemoves, func(p string) bool { return p == elementPath })
		return &SettingsItemResultVO{
			OK:      true,
			Summary: "已取消这一项（它还没保存过，config.yaml 没有变）",
		}
	}
	if err := validatePendingRemoval(cfg, elementPath, d.pendingRemoves); err != nil {
		return fail("删不了：" + err.Error())
	}
	name := ""
	if leaves, err := config.ListItemLeaves(cfg, elementPath); err == nil {
		if s, ok := leaves["name"].(string); ok {
			name = s
		}
	}
	dangling := danglingReferences(cfg, name)

	d.pendingRemoves = append(d.pendingRemoves, elementPath)
	// An item added in this same visit and then dropped again cancels out: the save must not try to
	// append an element it is also removing (the append would shift the index the removal names).
	d.pendingAdds = slices.DeleteFunc(d.pendingAdds, func(a pendingAdd) bool { return a.Element == elementPath })

	summary := "已标记删除 " + elementPath + "；点保存后才会写进 config.yaml"
	if len(dangling) > 0 {
		summary += fmt.Sprintf("；注意 %s 还写着「%s」，它现在指向一个不存在的条目，保存前请改掉",
			strings.Join(dangling, "、"), name)
	}
	return &SettingsItemResultVO{OK: true, Summary: summary}
}

// validatePendingRemoval refuses an element path this save cannot remove: a malformed path, or an
// index the list does not have (the form and the config disagree, which the reader has to fix by
// reopening the page — the alternative is removing a different element than the one they clicked).
//
// The removals ALREADY staged for this list are subtracted from the length, because they are
// already spoken for: two items marked in one visit are applied as two removals from the same
// original list, so the second one's index is legal even though the live list no longer reaches it.
func validatePendingRemoval(cfg *config.Config, elementPath string, already []string) error {
	listPath, index, err := config.SplitElementPath(elementPath)
	if err != nil {
		return err
	}
	length, err := config.ListLen(cfg, listPath)
	if err != nil {
		return err
	}
	staged := 0
	for _, p := range already {
		if other, _, err := config.SplitElementPath(p); err == nil && other == listPath {
			staged++
		}
	}
	if index >= length-staged {
		return fmt.Errorf("%s: 没有第 %d 项（这一页已经标了 %d 项要删，配置里还有 %d 项）",
			listPath, index, staged, length)
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

// pendingAppendLists turns the staged adds into the lists they grow: one element at the end of
// each item's own list.
//
// The element's INDEX is what the staging recorded (`providers.3`), but nothing here carries it:
// the list grows at its end, so the index is the list's own answer, and the batch's updates — keyed
// by the full path the page saw — land on it once the append has run (see config.ListEdits).
func pendingAppendLists(pending []pendingAdd) []string {
	if len(pending) == 0 {
		return nil
	}
	out := make([]string, 0, len(pending))
	for _, add := range pending {
		listPath, _, err := config.SplitElementPath(add.Element)
		if err != nil {
			continue
		}
		out = append(out, listPath)
	}
	return out
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

// SaveSettings applies a batch of edited leaves and tells the running process about them.
//
// The order is the design's (§六), and each step exists because of what the previous one can
// leave behind:
//
//  1. resolve EVERY key first — an unknown key, or one another entry mode owns, refuses the
//     whole batch before anything is touched. A half-saved form is a state no reader can
//     reason about.
//  2. turn the page's null into config.Unset for the leaves that can take one (normalizeUnsets):
//     the form's 自动 is the only thing that can say "unset", and a bare nil stays refused.
//  3. snapshot the leaves and apply them in one locked step (all or none), together with the
//     STRUCTURAL half: the list items this page staged with ＋ and − are appended and removed here,
//     in the order config.ListEdits documents (appends, then the leaf updates, then the removals —
//     only that order addresses the elements the form meant). The type check is this step's own:
//     config.SetPath refuses a value the field cannot hold, and the batch is put back whole.
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
// interleave their snapshots, config writes and pushes. It is also what makes the STAGED structure
// consistent here: + and − take the same lock to add to it.
//
// Returns whether the batch was written, plus the sentence the page shows (the desktop's
// convention for bound methods: a sentence rather than an error the frontend would have to
// translate). The two are not redundant — see SaveSettingsResultVO.
func (s *AgentService) SaveSettings(updates map[string]any) *SaveSettingsResultVO {
	d := s.desk
	d.settingsMu.Lock()
	defer d.settingsMu.Unlock()

	cfg := d.config()
	if cfg == nil {
		return &SaveSettingsResultVO{OK: false, Summary: "配置尚未加载，改不了"}
	}
	// The staged structure joins this save and is CONSUMED by it: from here on the page's ＋/− marks
	// belong to this batch, and a failure below leaves them where they were (nothing is cleared
	// until the write is through), so the reader can fix what the sentence named and save again.
	edits := ListEdits{
		Updates: updates,
		Appends: pendingAppendLists(d.pendingAdds),
		Removes: append([]string(nil), d.pendingRemoves...),
	}
	if edits.IsEmpty() {
		return &SaveSettingsResultVO{OK: false, Summary: "没有要保存的改动"}
	}

	keys := make([]string, 0, len(updates))
	for key := range updates {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	// 1. Every key must be one this page owns.
	rows := make(map[string]settingApply, len(keys))
	for _, key := range keys {
		row, ok := settingFor(key)
		if !ok {
			return &SaveSettingsResultVO{OK: false, Summary: fmt.Sprintf("未知配置项：%s", key)}
		}
		if !row.offered() {
			return &SaveSettingsResultVO{OK: false, Summary: fmt.Sprintf("%s 属于别的入口模式（%s），桌面不读它，改不了", key, row.Note)}
		}
		rows[key] = row
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
	// A leaf of a staged item is an ORDINARY update: the appends below run first, so by the time the
	// updates land the element exists and the path resolves. Order is the whole mechanism — one
	// object means there is nothing else to keep in step.
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
	var saveErr error
	d.withConfig(func(c *config.Config) {
		lift := overlayOwnedLeaves(c, ownedLeaves(rows, updates))
		defer lift()
		saveErr = config.Save(c)
	})
	if saveErr != nil {
		if !memoryEdits.IsEmpty() {
			d.withConfig(func(c *config.Config) { config.RestoreListEdits(c, snap) })
		}
		return &SaveSettingsResultVO{OK: false, Summary: "没能写入 config.yaml：" + saveErr.Error() + "（这次改动已撤销，内存与文件都没变，运行中的进程也未被通知）"}
	}
	// The write landed, so the staged structure is now part of the config and no longer pending.
	d.pendingAdds = nil
	d.pendingRemoves = nil

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
