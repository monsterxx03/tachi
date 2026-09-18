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
	"reflect"
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

// settingApply is one row.
type settingApply struct {
	// Key is a dot path covering itself and everything under it, on SEGMENT boundaries:
	// "compact" covers "compact.auto", never "compaction"; "language" covers only itself.
	// It is also the path SavePartial is given, which is why it is dotted rather than
	// slash- or index-based.
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
		Key:  "language",
		Mode: applyPush,
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
		Key:   "extra_system_prompt",
		Mode:  applyPush,
		Apply: func(d *desktopApp, _ *config.Config) error { d.invalidateSystemPrompts(); return nil },
	},
	{
		Key:  "title_generation",
		Mode: applyPush,
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
		Key:  "title_provider",
		Mode: applyPush,
		Apply: func(d *desktopApp, cfg *config.Config) error {
			return pushError("标题 provider", d.applyToLiveAgents(func(a *agent.AIAgent) error {
				a.SetupTitleProvider(cfg)
				return nil
			}))
		},
	},
	{
		Key:  "commit_provider",
		Mode: applyPush,
		Apply: func(d *desktopApp, cfg *config.Config) error {
			return pushError("提交信息 provider", d.applyToLiveAgents(func(a *agent.AIAgent) error {
				a.SetupCommitProvider(cfg)
				return nil
			}))
		},
	},
	{
		Key:  "run_provider",
		Mode: applyPush,
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
	{Key: "provider", Mode: applyLazy, Note: "这是新会话的默认值；已开的会话各自记录着自己的 provider，要用 /model 切换"},
	// `providers` is the provider DEFINITIONS (model, base_url, api_key, thinking_level): an
	// edit has to reach the agents that were resolved before it.
	{
		Key:  "providers",
		Mode: applyPush,
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
	{Key: "provider_aliases", Mode: applyRestart, Note: "ExpandProviderAliases 只在 LoadFrom 末尾跑一次，别名早已展开成真名"},
	{Key: "max_tokens", Mode: applyLazy},
	{Key: "max_iterations", Mode: applyUnused, Note: "desktop 不传 MaxIterations（0 = 无限），只有 tachi -p 读它"},

	// --- sessions ---
	{Key: "session_cleanup_max_count", Mode: applyLazy, Note: "建新会话时读；已在运行的会话 manager 保留旧上限"},

	// --- tools ---
	{
		Key:  "web_search",
		Mode: applyPush,
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
		Key:  "web_fetch",
		Mode: applyPush,
		Apply: func(d *desktopApp, _ *config.Config) error {
			return pushError("抓取工具", d.applyToLiveAgents(func(a *agent.AIAgent) error {
				return a.ReloadTools()
			}))
		},
	},
	{
		Key:  "tool_result",
		Mode: applyPush,
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
		Key:  "permissions.bash",
		Mode: applyPush,
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
		Key:  "active_mcp_profile",
		Mode: applyPush,
		// This row OWNS the write of its leaf (OwnsWrite): the batch must not pre-apply the profile,
		// or a switch that fails would leave the field naming a profile whose servers are not
		// loaded — and the panel would show it as active. d.switchMCPProfileTo writes it, under
		// cfgMu, only after the connections have actually moved.
		OwnsWrite: true,
		Apply: func(d *desktopApp, _ *config.Config) error {
			profile, ok := d.savingUpdates["active_mcp_profile"].(string)
			if !ok {
				return nil // not a string: ValidateUpdates refused it long before this step
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
		Key:  "mcp_tool_search",
		Mode: applyPush,
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
		Key:  "mcp_tool_refresh",
		Mode: applyPush,
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
		Key:  "lsp",
		Mode: applyPush,
		Apply: func(d *desktopApp, cfg *config.Config) error {
			// Per session: the manager is built per agent, and ReloadLSP tears the old one down
			// first (its child processes would otherwise be orphaned).
			return pushError("LSP", d.applyToLiveAgents(func(a *agent.AIAgent) error {
				return a.ReloadLSP(context.Background(), cfg)
			}))
		},
	},
	{
		Key:  "subagent",
		Mode: applyPush,
		Apply: func(d *desktopApp, cfg *config.Config) error {
			// Covers subagent.provider too: ReloadSubagent re-resolves the dedicated provider
			// and rebuilds the executor (worktree included) under one call.
			return pushError("subagent", d.applyToLiveAgents(func(a *agent.AIAgent) error {
				return a.ReloadSubagent(cfg)
			}))
		},
	},
	{
		Key:  "memory",
		Mode: applyPush,
		Apply: func(d *desktopApp, cfg *config.Config) error {
			// The reminder is in the collector for the agent's life, so it is pointed at the new
			// backend instead of being re-registered — see ReloadMemory.
			return pushError("memory", d.applyToLiveAgents(func(a *agent.AIAgent) error {
				return a.ReloadMemory(context.Background(), cfg)
			}))
		},
	},
	{
		Key:  "hooks",
		Mode: applyPush,
		Apply: func(d *desktopApp, cfg *config.Config) error {
			return pushError("hooks", d.applyToLiveAgents(func(a *agent.AIAgent) error {
				return a.ReloadHooks(cfg)
			}))
		},
	},

	// --- snapshots and bookkeeping ---
	{Key: "checkpoints.enabled", Mode: applyLazy},
	// The three limits need no reload call: the manager bakes them in at construction, and the
	// per-session key now CONTAINS them, so the next use rebuilds it with the new values.
	{Key: "checkpoints.max_files", Mode: applyLazy, Note: "Options 已并进检查点 manager 的 key：下次用到时按新值重建"},
	{Key: "checkpoints.max_bytes", Mode: applyLazy, Note: "同 checkpoints.max_files"},
	{Key: "checkpoints.retain", Mode: applyLazy, Note: "同 checkpoints.max_files"},
	{Key: "oneoff", Mode: applyLazy},
	{Key: "compact", Mode: applyLazy},
	{Key: "review", Mode: applyLazy, Note: "ResolveReviewOptions(cfg) 每次命令现读"},
	// Registered unconditionally, reading the config on every generation (see GitReminder):
	// nothing has to be pushed, and nothing has to be rebuilt — which is the point, since the
	// collector cannot remove a reminder without taking every other reminder's state with it.
	{Key: "system_reminder", Mode: applyLazy, Note: "git reminder 每次生成时现读配置"},

	// --- consumed once at startup ---
	{
		Key:  "logs.level",
		Mode: applyPush,
		Apply: func(_ *desktopApp, cfg *config.Config) error {
			// Process-wide, not per session: every handler holds the same LevelVar, so this
			// reaches loggers built at startup — which is exactly what the old scalar could not.
			logger.SetLevel(cfg.Logs.Level)
			return nil
		},
	},
	{Key: "logs", Mode: applyRestart, Note: "max_size / max_files 冻结在 rotatingWriter 上（含已打开的 fd）；logs.level 有更具体的一行，已是热改"},
	{Key: "debug.pprof", Mode: applyRestart, Note: "启动即绑端口"},
	{Key: "herdr", Mode: applyRestart, Note: "hook 初始化时读，且 DetectHerdr 还看 HERDR_ENV"},

	// --- another entry mode's keys ---
	{Key: "tui", Mode: applyUnused, Note: "TUI 专属"},
	{Key: "channel", Mode: applyUnused, Note: "channel 模式的 IM 后端，desktop 不启用"},
	{Key: "cron", Mode: applyUnused, Note: "channel 模式的后台调度"},
	{Key: "dream", Mode: applyUnused, Note: "channel 模式的后台记忆整理"},
	{Key: "acp", Mode: applyUnused, Note: "ACP 入口的配置"},
	{Key: "deep_research", Mode: applyUnused, Note: "desktop 没有 research 命令（TUI 与 tachi -p 才有）"},
	{Key: "web", Mode: applyUnused, Note: "tachi web 是独立进程，重启那个进程即可"},
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
	// Key is the dot path the form sends back, and the same path SavePartial writes.
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
	// Note is why: the reason a restart is needed, or which entry mode owns the key instead.
	Note string `json:"note,omitempty"`
}

// GetSettingsSchema enumerates the fields the settings page may offer, one per editable leaf
// of the live config.
//
// It walks the CONFIG's leaves (config.PathsFor) rather than the table's rows, because a row
// is often a block — `lsp` speaks for eleven leaves, and a form needs eleven inputs, each with
// the block's mode and note. The two filters are the row's: a key no row covers cannot happen
// (the guard test in settings_test.go fails the build first), and an applyUnused row is another
// entry mode's key, so it is left out rather than offered as a switch that does nothing.
//
// A value that cannot be read is still listed, with the reason in Note: hiding a field would
// present a smaller settings page as a complete one, and the failure is the interesting part.
func (s *AgentService) GetSettingsSchema() []SettingFieldVO {
	d := s.desk
	cfg := d.config()
	if cfg == nil {
		return nil
	}
	paths := config.PathsFor(cfg)
	out := make([]SettingFieldVO, 0, len(paths))
	for _, key := range paths {
		row, ok := settingFor(key)
		if !ok || !row.offered() {
			continue
		}
		value, err := config.ReadPath(cfg, key)
		note := row.Note
		if err != nil {
			note = joinNotes(note, "读取失败："+err.Error())
		}
		out = append(out, SettingFieldVO{
			Key:          key,
			Kind:         kindOf(value),
			Value:        value,
			NeedsRestart: row.needsRestart(),
			Note:         note,
		})
	}
	return out
}

// kindOf names the input a value wants. Reflection rather than a switch on concrete types,
// because the config has named scalar types (`config.Duration` is an int64 underneath) that
// are still numbers to a form.
func kindOf(v any) string {
	if v == nil {
		return "other"
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "bool"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "int"
	case reflect.Float32, reflect.Float64:
		return "float"
	case reflect.Slice:
		if rv.Type().Elem().Kind() == reflect.String {
			return "string_list"
		}
	}
	return "other"
}

// SaveSettings applies a batch of edited leaves and tells the running process about them.
//
// The order is the design's (§六), and each step exists because of what the previous one can
// leave behind:
//
//  1. resolve EVERY key first — an unknown key, or one another entry mode owns, refuses the
//     whole batch before anything is touched. A half-saved form is a state no reader can
//     reason about.
//  2. validate the values against the FILE's own type table, before memory changes.
//  3. snapshot the leaves and apply them in one locked step (all or none).
//  4. write config.yaml. Only a save that got this far continues: if the write fails, the
//     memory is put back exactly as it was and the answer says so — "applied for this session
//     but not saved" is a state the next restart contradicts, and the reader cannot tell which
//     of the two they have.
//  5. push each row's `Apply` — the runtime half, and LAST, because a push has no undo: it
//     rebuilds tools, moves a process-wide log level, swaps backends, reconnects servers. Run
//     before the write, a failure at step 4 would leave the running process on values the file
//     (and the restored memory) does not have, while the answer promised the opposite. Its own
//     failures are per-session and reported, not fatal — the value IS saved.
//
// The whole sequence is serialized by d.settingsMu: two saves landing at once would otherwise
// interleave their snapshots, config writes and pushes.
//
// Returns the summary the page shows (the desktop's convention for bound methods: a sentence
// rather than an error the frontend would have to translate).
func (s *AgentService) SaveSettings(updates map[string]any) string {
	d := s.desk
	d.settingsMu.Lock()
	defer d.settingsMu.Unlock()

	cfg := d.config()
	if cfg == nil {
		return "配置尚未加载，改不了"
	}
	if len(updates) == 0 {
		return "没有要保存的改动"
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
			return fmt.Sprintf("未知配置项：%s", key)
		}
		if !row.offered() {
			return fmt.Sprintf("%s 属于别的入口模式（%s），桌面不读它，改不了", key, row.Note)
		}
		rows[key] = row
	}

	// 2. The type table the file write uses, so a bad value never reaches memory.
	if err := config.ValidateUpdates(updates); err != nil {
		return "值不合法：" + err.Error()
	}

	// 3. In memory first: a batch is all-or-nothing, and a bad value never reaches the file.
	// A row that OWNS the write of its leaf (settingApply.OwnsWrite) is deliberately NOT in the
	// batch: its push writes the value itself, at the moment the runtime can actually take it, so
	// there is one writer and a failed push leaves the field alone.
	batched := make(map[string]any, len(updates))
	batchedKeys := make([]string, 0, len(keys))
	for _, key := range keys {
		if rows[key].OwnsWrite {
			continue
		}
		batched[key] = updates[key]
		batchedKeys = append(batchedKeys, key)
	}
	var snap []config.LeafValue
	if len(batched) > 0 {
		var snapErr error
		snap, snapErr = config.SnapshotLeaves(cfg, batchedKeys)
		if snapErr != nil {
			return "改不了：" + snapErr.Error()
		}
		var applyErr error
		d.withConfig(func(c *config.Config) {
			applyErr = config.ApplyUpdates(c, batched)
		})
		if applyErr != nil {
			return "改不了：" + applyErr.Error() + "（已撤销，配置文件未动）"
		}
	}

	// 4. The file: it is what survives the process. A failure here undoes the memory and
	// returns — and it is honest to say so, because nothing has been pushed yet (step 5).
	if err := config.SavePartial(updates); err != nil {
		if len(batched) > 0 {
			d.withConfig(func(c *config.Config) { config.RestoreLeaves(snap) })
		}
		return "没能写入 config.yaml：" + err.Error() + "（这次改动已撤销，内存与文件都没变，运行中的进程也未被通知）"
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
		return "已保存"
	}
	return strings.Join(parts, "；")
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
