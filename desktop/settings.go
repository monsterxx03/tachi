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
	"fmt"
	"strings"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/config"
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
	{Key: "title_provider", Mode: applyPush, Note: "每个会话一份的 title provider，要在所有 live agent 上重解析"},
	{Key: "commit_provider", Mode: applyPush, Note: "每个会话一份的 commit provider，要在所有 live agent 上重解析"},
	{Key: "run_provider", Mode: applyPush, Note: "同上（run 模式的 provider）"},

	// --- model and providers ---
	// Prices and credit rates are read lazily by the usage ledger, but resolving a
	// provider is a per-session fact (SetResolvedProvider), so the row is a push.
	{Key: "provider", Mode: applyPush, Note: "SetResolvedProvider 是每个会话一份，要广播"},
	{Key: "providers", Mode: applyPush, Note: "同 provider；改的是同一个解析结果"},
	{Key: "provider_aliases", Mode: applyRestart, Note: "ExpandProviderAliases 只在 LoadFrom 末尾跑一次，别名早已展开成真名"},
	{Key: "max_tokens", Mode: applyLazy},
	{Key: "max_iterations", Mode: applyUnused, Note: "desktop 不传 MaxIterations（0 = 无限），只有 tachi -p 读它"},

	// --- sessions ---
	{Key: "session_cleanup_max_count", Mode: applyLazy, Note: "建新会话时读；已在运行的会话 manager 保留旧上限"},

	// --- tools ---
	{
		Key:  "web_search",
		Mode: applyPush,
		Note: "工具对象在 RegisterTools 里构建（含 sync.Once 的 provider 缓存），要重建再同名覆盖",
	},
	{Key: "web_fetch", Mode: applyPush, Note: "同 web_search"},
	// The second consumer is the trap here: the shared MCP manager snapshots max_chars and
	// file_dir at construction, so rebuilding the tools alone would leave MCP results
	// truncated by the old limits.
	{Key: "tool_result", Mode: applyPush, Note: "工具要重建；共享 MCP manager 还在构造期快照了这两个值"},
	{Key: "permissions.bash", Mode: applyPush, Note: "SetPermissionPolicy 是每个会话一份（策略在构造期建好）"},

	// --- MCP ---
	{Key: "active_mcp_profile", Mode: applyPush, Note: "全进程一份的事实，要广播（SwitchMCPProfile 只动一个会话）"},
	{
		Key:  "mcp_tool_search",
		Mode: applyPush,
		Note: "要重跑 PopulateFromConnect（= 重连所有 server）并重算 autoLoad；max_deferred_schema_bytes 全库 0 读点",
	},
	{Key: "mcp_tool_refresh", Mode: applyPush, Note: "StopRefresher + 重启；回调绑在触发它的那个 agent 上，要从 app 层驱动"},

	// --- per-session subsystems built at construction ---
	{
		Key:  "lsp",
		Mode: applyPush,
		Note: "先 Shutdown（否则留下孤儿的语言服务器进程）再新建 manager、重注册 2 个工具、换 reminder 的 provider",
	},
	{Key: "subagent", Mode: applyPush, Note: "executor 与 SubagentTool 在构造期建立，要重建并重注册（worktree 与并发信号量一起重来）"},
	{Key: "memory", Mode: applyPush, Note: "backend 与两个 memory 工具在构造期建立；reminder 里捕获的 backend 也要换"},
	{Key: "hooks", Mode: applyPush, Note: "dispatcher 构造期构建；新配置禁用时要显式置 nil，否则旧的留着"},

	// --- snapshots and bookkeeping ---
	{Key: "checkpoints.enabled", Mode: applyLazy},
	{
		Key:  "checkpoints.max_files",
		Mode: applyPush,
		Note: "ckptKey 未含 Options：今天改了不重建 manager（设计 §八之二 ① 修完后即真热改）",
	},
	{Key: "checkpoints.max_bytes", Mode: applyPush, Note: "同 checkpoints.max_files"},
	{Key: "checkpoints.retain", Mode: applyPush, Note: "同 checkpoints.max_files"},
	{Key: "oneoff", Mode: applyLazy},
	{Key: "compact", Mode: applyLazy},
	{Key: "review", Mode: applyLazy, Note: "ResolveReviewOptions(cfg) 每次命令现读"},
	{
		Key:  "system_reminder",
		Mode: applyRestart,
		Note: "git reminder 只在构造期注册进 collector；改成惰性开关（设计 §八之二）后即变 lazy",
	},

	// --- consumed once at startup ---
	{Key: "logs", Mode: applyRestart, Note: "level 差一层 LevelVar（设计 §五之三 2）；max_size/max_files 是构造期冻结的 writer"},
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
