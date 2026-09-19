# Desktop 设置页 — 设计

> 版本: 3.0 | 日期: 2026-09-19（v1.0: 2026-09-14，v2.0~v2.7: 2026-09-17~18）| 状态: P0-a、P0-b、
> **结构前置（§八之三）、P2 的后端**（`GetSettingsSchema` / `SaveSettings` + `config` 的路径读写）、
> **全部热改路径**（§五之四：97 个输入框里 91 个已能免重启生效）以及
> **P1/P2/P3 的前端**（§八之四：`App.tsx` 的 view 状态 + `settings.tsx` 外壳 + 分类导航 +
> Kind 驱动的通用表单 + ⟳ 徽标 + 保存汇总）与 **P4 的 smoke 场景**（`settings`：37 项断言，含
> 文件回读）均已实现
>
> v2.0 是 v1.0 的**全面复核**：热改分层按实际消费点重做，P0 的验收标准收紧
> （`yaml.Node` 回环实测发现 v1.0 承诺的「样式保留」不成立），原子性顺序修正。
>
> v2.1 是把 §五 的分层表**按代码逐条反查**的结果：两条判重了（`system_reminder.git_reminder`、
> `logs.level` 其实可热），两条漏了消费点（`tool_result.*` 还喂共享 MCP manager，
> `memory.type` 还要换 reminder 里捕获的 backend），一条判轻了（`mcp_tool_search.*`），
> 一条把「没接线」写成了「包一层即可」（desktop 侧没有任何 `Setup*` 调用点）。
>
> v2.2 是**开工记录**：§八之三 的三条结构前置已落地（应用表 `desktop/settings.go`、键覆盖守卫
> `desktop/settings_test.go`、广播原语 `applyToLiveAgents`），并顺手做了 §八之二 的 prompt 缓存清空。
> 落地与设想的三处出入写在 §八之三 里。
>
> v2.3 是 **P2 后端的落地记录**：`config` 多了一套与 `SavePartial` 同源的点分路径读写（§六）、
> `SettingFieldVO` 的形状按实现定稿（§七）、保存的顺序与回滚语义写进 §七；§六 里「一次保存只应用
> 一个叶子」作废。
>
> v2.4 是**把能不用重启的项做掉**：热改从 9 行涨到 24 行（表单 97 个输入框里 53 个），还剩 10 行
> 需重启；新增 `agent/agent_reload.go`（重建家族）与 `pkg/logger` 的共享 `LevelVar`，现状见 §五之四。
>
> v2.5 是**剩下 6 行也做掉**：`lsp` / `memory` / `subagent` / `active_mcp_profile` /
> `mcp_tool_search` / `mcp_tool_refresh`。现在的口径是 **30 行热 / 4 行需重启**（97 个输入框里
> 91 个能免重启生效），剩下 4 行全是真结构性（§五之四）。
>
> v2.6 是 **P1/P2/P3 前端的落地记录**（§八之四）：`settings.tsx` 是一个不认识任何配置键的
> 渲染器（分组、Kind、⟳、note 全部由后端给），为此后端多了 `Category` / `Options` /
> `Optional` / `IsSet` 四列、`settingChoices` 一张表与 `SettingsSchemaVO` /
> `SaveSettingsResultVO` 两个 VO；provider 名字与 provider 类型是下拉，配置列表
> （`providers` / `lsp.servers` / `web_search.providers`）的叶子折成一张张卡片，头上写
> `main · claude-x` 而不是 `providers.0`，`spec` 下那几个 `*bool` / `*int64` 有了「自动」
> （未设置）这一态。
> 开工后照例照出三处后端缺口——**时长**字段走桥是字符串、写盘只认整数，**列表**从 JS 过来是
> `[]any`、`renderScalar` 只认 `[]string`，以及**指针的 nil 态写不回去**（路径 API 从来没有
> "取消设置"这个操作）——都在 `config` 侧补上。
>
> v2.7 是**列表的增删**（§八之五）：设置页能往 `providers` / `web_search.providers` /
> `web_fetch.providers` / `lsp.servers` 里加一项、删一项；"新增一个键"这条产品决策被**收窄**
> （父块已存在的那一个键 + 列表末尾），没有推翻。剩余待办（其余枚举、`other` 的只读项）见
> §八之四 末尾。
>
> v3.0 是**把 §六 的文件侧推翻**：产品上真正要的只有「改完不用重启」（§五之四 的整张热改表、一行未动），
> 而"保留用户手写文件的注释与缩进"是这一页唯一还没有独立价值的成本 —— 它要求把 config.yaml 当成
> **独立一份文本**按路径寻址，由此才有 `SavePartial` / `ensureBlock` / `ListAppend.Index` /
> `updatesOutsideAppends` / 按幸存者验证删除这一整套，以及"文件拒绝某个键"这条只存在于两个半边之间的失败模式。
> 现在文件就是内存对象 Marshal 出来的（`config.Save`，原子写 + 保留 mode），`SaveSettings` 六步变五步：
> 键校验 → 归一 Unset → 整批写内存（含结构半边）→ **整份写盘** → 逐行推送。列表的增删 UX（暂存、保存时
> 才落盘）保留，但它不再需要索引声明与跨半边去重 —— 追加先进内存，条目的叶子就是普通 update。
> 代价实测在案：用户自己的 config.yaml 一次保存后由 65 行变 ~148 行（`defaults.Set` 的默认值被显式写出），
> 注释不可逆，故 `config.backupOnce` 在第一次整体重写前把原文件留成 `config.yaml.bak`；「自动」在文件里
> 从 `vision: null` 变成**那个键不出现**（`omitempty`），回读仍是 nil；`goccy/go-yaml` 随之从依赖里消失。
> 未改：`settingsTable` 的每一行、全部 `Apply`、`deferred_push`、`GetSettingsSchema` 与前端（schema 驱动，
> 签名未变）。
>
> v3.1 是**把列表增删的暂存区从后端抹掉**：v2.7 的「＋ 立刻写盘」在 v3.0 之后只剩一半理由，于是留了一份
> **跨请求存活**的暂存状态（`desktopApp.pendingAdds`/`.pendingRemoves`）。它与字段草稿分属两个主人，
> 于是会漂移 —— 实测抓到三处：＋ 两次给两个条目同一个 index（保存后列表多一条空元素、两条字段互相踩）、
> 同一条目录删两次（两次删除都执行，静默丢掉两条）、标了删除后离开设置页再保存别的字段（那一项跟着被删）。
> 现在**未保存状态只有一个主人：页面**。`SaveSettings(updates, appends, removes)` 一次带走全部待办，
> 后端删掉 `pendingAdds`/`pendingRemoves`/`AddSettingsItem`/`stagedFields`，`SettingsRemovalWarning`
> 只剩一个只读问题（能不能删、还有谁写着它的名字）；`GetSettingsSchema` 回到**配置的纯函数**，新条目靠
> 它给的 `SettingListVO.Length` + `.Element`（条目模板）由页面自己画。`ListEdits.Appends` 从列表路径收紧为
> **元素路径**（`providers.3`），保存因此能校验"表单看到的那个列表"还成立；删除去重（同一路径两次直接拒绝）。
> UX 随之简化：**删除一次点击**，卡片立刻移出列表、原位一行「已标记删除 · 撤销」（这是 v2.7 的二次确认
> 与 v3.0 那句「会立刻写进 config.yaml」的终点 —— 两句话都已经不成立）。
>
> 关联: [desktop one-off panel](./2026-09-12-desktop-oneoff-panel-design.md)、
>       [desktop project](./2026-09-14-desktop-project-design.md)

## 一、问题

`desktop/frontend/src/App.tsx:1856` 的侧栏底部有一个**禁用**的设置入口：

```tsx
<button className="footer-item" disabled title="暂未实现">… 设置</button>
```

它一直是占位符——桌面上改任何配置都必须手写 `~/.tachi/config.yaml` 然后重启 app。
这份文档定义把它填上：

1. **整页覆盖**的独立设置页（不是浮层，不是右侧第三列）
2. 保存后**实时生效**；个别必须重启的项，明确提示
3. 保存到 `~/.tachi/config.yaml`

## 二、已定的决策

| # | 决策 | 说明 |
|---|---|---|
| 1 | **保留 titlebar** | 设置页替换 titlebar 以下的内容。macOS 无边框窗口靠 `drag-region` 拖动，自造拖拽区容易出错 |
| 2 | **允许后台会话继续跑** | 与切换会话的行为一致——设置页只盖住视图，不干预进程 |
| 3 | **显式保存按钮** | 改动即存会让「需重启」的提示无处安放 |
| 4 | **需重启的项一起做** | 第一版即包含，用徽标区分 |
| 5 | **密钥直接回显** | 不做脱敏/掩码。配置页只对能读到该文件的人可见，掩码带来的「看不到当前值」成本高于收益 |

## 三、形态

保留 titlebar，其下替换为两栏：

```
┌─────────────────────────────────────────────────┐
│ titlebar（保留：品牌 / 侧栏开关 / 主题）          │
├───────────────┬─────────────────────────────────┤
│ ← 返回设置    │                                 │
│               │  当前分类的表单                  │
│ 通用          │  ┌───────────────────────────┐  │
│ 模型          │  │ 语言       [中文       ▾] │  │
│ 工具与权限    │  │ 最大迭代   [50]           │  │
│ 外观          │  │ 日志级别   [info      ▾] ⟳ │  │
│ 高级          │  └───────────────────────────┘  │
│               │                                 │
│               │            [ 保存 ]             │
└───────────────┴─────────────────────────────────┘
```

- `⟳` 徽标只出现在**需重启**的字段旁
- 保存后，页面顶部出一条**汇总**提示（不是每项一次）：
  「3 项已生效，2 项需重启后生效」+ 一个「立即重启」入口（可选）
- 保存失败（YAML 写失败 / 校验失败）：**不落盘**，错误就地显示，内存配置不动

## 四、为什么是整页

竞品与 UX 原则都指向整页：

- **Claude Code Desktop** 的设置是**路由**：`/settings/general`、`/settings/usage`、
  `/settings/billing`。该形态的证据是它自己的 bug：路由错乱时 `/settings/usage` 把
  **Code 面板的整个内容区替换掉了**——只有整页才可能这样。
- **UX 决策树**的判据：是否需要保留底层屏的上下文（否）→ 任务是否长/多步（是，30+ section）
  → 是否需要来回参照底层（否）→
  **"For more complex flows and multi-step processes, standalone pages work best."**
  该文同时明确警告 "tabbed navigation within modals doesn't work too well"。

### 整页的代价与应对

Tachi 是**单窗口多会话**，没有 Claude Code Desktop 那种 tab 层。设置页整页取代对话区后，
返回时必须自己保住：

| 要保的 | 归属 |
|---|---|
| 转写内容 | `@` 已有（会话从磁盘重读） |
| 滚动位置 | 由 `App.tsx` 的 view 状态切换保住——**组件不卸载**即可 |
| 折叠状态 / 输入草稿 | 同上 |

实现要点：**用 CSS 隐藏而非条件卸载**。若把 `<main>` 摘出组件树，所有本地状态都会丢。

⚠️ **键盘快捷键要显式屏蔽**：对话区的处理函数挂在元素上（chat 的 `onChatKey`），
composer 另有全局监听。设置页期间不屏蔽的话，输入会被下面的对话区吃掉。

## 五、配置分层（已核实）

热改的基础设施**已经存在**：`AIAgent` 上已有 **26** 个 `Set*`/`Setup*` 方法，且
`desktop/agent_mcp.go` 的 `SetMCPServerEnabled` 就是「改内存配置 + 立即生效」的先例
（它同时把 enable 状态写回 mcp.json——设置页正好补上 config.yaml 那半）。

⚠️ **但 desktop 侧一个 `Setup*` 都没调用过**（已 grep：`desktop/*.go` 里只有 `SetThinking`、
`SetResolvedProvider`、`SetMode` 与 MCP 那几个的调用点）。下表 🔥 里那些「包一层导出即可」的项，
实际要做的是**新增 fan-out**——它们今天只在 `NewAIAgentWithConfig` 构造期被调用一次。
这个 fan-out 是 §八之三 的第 ③ 条，也是 P2 所有热改项共用的形状。

### 🔥 可热改

| 配置 | 生效机制 |
|---|---|
| `provider` / `providers` | `SetResolvedProvider`（同时重指 compact 策略） |
| `provider[].spec.thinking_level` | 随 `SetResolvedProvider` 一体切换 |
| `title_generation` | `SetTitleGenEnabled`（方法已有；desktop 无调用点，要新写 fan-out） |
| `title_provider` / `commit_provider` / `review.provider` / `run_provider` / `subagent.provider` | 各 `Setup*Provider` 已存在，但 desktop **没有任何调用点**（见本节开头的 ⚠️）——要新写 fan-out，即 §八之三 ③ |
| `permissions.bash.*` | `SetPermissionPolicy`（方法本身**零重构**；同样要先有 fan-out） |
| `compact.auto` / `compact.threshold` | `shouldAutoCompact()` **每轮**读 `FullConfig` |
| `compact.timeout` / `compact.max_tokens` | `doCompact` 现读 |
| `oneoff.enabled` / `retention_days` | `oneoffEnabled()` 现读 |
| `checkpoints.enabled` | `checkpointsEnabled()` 现读 |
| `session_cleanup_max_count` | 建新会话时读（`Manager.New` 内建 `cleanupLocked`） |
| `review.*` / `deep_research.*` | `ResolveReviewOptions(cfg)` / `NewDeepResearch(cfg)` 每次命令现读 |
| `active_mcp_profile` | `SwitchMCPProfile`（内存态，重启回退——既有先例） |
| `language` / `extra_system_prompt` | 清 `d.promptCache` 即可——但清空入口是**前提**，不是锦上添花：`systemPromptFor` 确实每轮现读 `d.cfg`，可它只在**未命中**时重建，而 cache key 只含 `(cwd,id,roots,mode)`（不含语言），今天唯一的失效路径是容量满时整体丢弃（`desktop/agent_driver.go:167-172`）。加一个清空方法，不必往 key 里加字段 |
| `logs.level` | 只在 `pkg/logger` 改一处：标量全局 `cfgLevel` → `*slog.LevelVar`，`textHandler.minLevel` 从值改成指向同一个 `LevelVar` 的指针 → **所有已建 logger 立刻跟着变**，零调用点改动（约 10 行）。见 §五之三 |
| `system_reminder.git_reminder` | 给 `GitReminder` 加惰性开关 + **无条件注册**（照 `attachSharedMCPReminder` 的既有写法）。**不要**重建 collector，见 §五之三 |

### 🟡 小重构后可热（要做的是「重建 + 换入」）

| 配置 | 需要什么 |
|---|---|
| `web_search.*` / `web_fetch.*` | 在 `RegisterTools` 里重建工具对象再 `Registry.Register`（同名覆盖）。`WebSearchTool` 的 `sync.Once` 缓存只随实例走，换实例即生效。注意 `web_search` 在未配置时**根本没注册**，所以增减都要处理 |
| `tool_result.*` | ⚠️ 两条路都要动：web/bash 工具同上，**外加共享 MCP manager**——它在构造期把 `MaxResultChars()`/`ResultFileDir()` 快照进 `m.maxResultChars/resultFileDir`（`agent/mcp/manager.go:200-203`，每次 MCP 工具调用都用），改配置不碰它就等于只对一半工具有效。让它每调用现读最省事 |
| `checkpoints.max_files` / `max_bytes` / `retain` | ⚠️ `ckptKey` 只含 `sess.ID + roots`，**不含 Options**——必须把 Options 并进 key，否则改了不重建 |
| `mcp_tool_search.*` | **比 v2.0 写的重**：改它要重跑 `PopulateFromConnect`（＝重连所有 server）、重算 autoLoad、还要 unregister 差集；`autoLoadNames` 只 append 没有 reset（`agent/mcp/manager.go:468`），也没有"不重连就重算"的 API。第一版建议标 🔴 或只读展示 |
| `mcp_tool_refresh.interval` | `StopRefresher()` + `StartRefresher()`（`StartRefresher` 内部已会先停）。⚠️ shared manager 的 refresh callback 会绑到**触发它的那个 agent**（`agent/agent_configure.go:297` 的闭包捕获 `a`），所以要从 app 层驱动，别从某个会话的 agent 驱动 |
| `lsp.*`（含 `servers`） | `LSPManager` **没有 Restart API**，只有 `Shutdown`（`agent/lsp/manager.go:144`），且**必须先调**——它停掉每个 server 的子进程（`agent/lsp/server.go:302→541`），不调就是孤儿进程 + 泄漏的 goroutine。然后新建 manager + 重注册 2 个工具（`LSP`/`LSPDiagnostics`）+ 换掉 reminder 里的 provider（**不是**重建 collector，见 §五之三）。另：`enabled` false→true 时工具与 reminder 根本不存在，要**新增**注册 |
| `hooks.*` | `HookDispatcher` 构造期一次性构建，重建即可。⚠️ 新配置**禁用** hooks 时必须把 `a.Config.HookDispatcher` 显式置 nil——`initHookSystemFrom` 只在启用时赋值，否则旧的留着 |
| `subagent.*` | `subagent.NewExecutor` + 重注册 `SubagentTool`（`Registry.Register` 同名覆盖，`agent/tools/tool.go:144-152`）。worktree 也要重新 `EnableWorktree`；并发信号量随 executor 重建 |
| `memory.type` | 重建 `MemoryState` + backend、重注册 2 个 memory 工具，**并且**换掉 `MemoryRecallReminder` 里捕获的 `Backend`（`agent/agent_configure.go:500-510`）——v2.0 漏了这条 |
| `memory.decay_half_life_days` | `halfLifeDays` 是 `TopicBackend` 的私有字段，只在构造期赋值（`agent/memory/topic_backend.go:136`），**没有 setter** → 要么加一个，要么走上面的整份重建 |
| `memory.keyword_provider` | 其实**已经热**：`resolveKeywordProvider(cfg)` 幂等且现读（`agent/agent_configure.go:521`），接线即可 |

### 🔴 必须重启

| 配置 | 原因（已核实） |
|---|---|
| `logs.max_size` / `max_files` | `logger.Init`（`pkg/logger/logger.go:32`）写全局量，而 `rotatingWriter` 把两个上限与**打开的 fd** 一起冻结在构造期——热改要动那个 mutex 保护的 writer，不值当。`logs.level` 已单独提到 🔥，其余 `logs.*` 不存在（没有 format / file 键，别在表单里编出来） |
| `debug.pprof.*` | `startPprof` 启动即绑端口（`agent/bootstrap.go:48`）。**这是实现现状，不是约束**：留住 `net.Listener` + `cancel` 后 Stop/Start 是 ~30 行（`http.DefaultServeMux` 可复用），只是第一版不做 |
| `web.*` | `tachi web` 是独立进程，端口已绑 |
| `channel.*` / `cron.*` / `dream.*` | channel 模式的后台循环；desktop 本不启用。各 channel 构造期就固化了 token/proxy 等 |
| `herdr.enabled` | 由 `HERDR_ENV` 环境变量推导，启动期判定 |
| `tui.*` | TUI 专属，widget 构造期消费 |
| `provider_aliases` | `ExpandProviderAliases` 只在 `LoadFrom` 末尾跑，且别名已被展开成真名 |

### 五之四、落地后的现状（v2.5）

**权威是代码**：`desktop/settings.go` 的 `settingsTable`，以及它的守卫测试与「这些键必须保持热」
那道围栏（`TestTheLikelyEditsStayHot`）。上面的 🔥/🟡/🔴 是设计期的分类，两批做完之后：

- **🔥 真热改 30 行**（表单 97 个输入框里的 **91 个**）；
- **仍需重启 4 行 / 6 个输入框**，且全是真结构性：`provider_aliases`（别名在 LoadFrom 里就展开成
  真名）、`logs.max_size` / `logs.max_files`（冻结在 `rotatingWriter` 与它打开的 fd 上）、
  `debug.pprof`（启动即绑端口）、`herdr`（hook 初始化时读 + `HERDR_ENV`）。

**第一批（v2.4，15 行）的形状**——接下来接任何一行都值得照抄：

- 能**惰性**的一律惰性（`checkpoints.*` 的三个上限只要把 Options 并进 manager 的 key；`git_reminder`
  改成一个每次生成现读的开关；`provider` 本来就是"新会话的默认值"）——没有推送就没有可忘的地方；
- 必须推送的走 `agent/agent_reload.go`：**幂等**、**只做推送那一半**、**绝不碰前端注入的对象**；
- 一次改动能复用既有路径的，就别新开一条：`permissions.bash` 用的是已有的 `SetPermissionRoot`
  （按会话自己的树重编译策略并继承会话审批），`providers` 用的是 `SwitchProvider` 的配方
  （按会话**自己**的 provider 名重解析，再放回该会话的 thinking 覆盖）。

**第二批（v2.5，那 6 行）的形状**——每个子系统一条重建路径，四条不变量是共用的：

- **reminder 只能被"指向"，不能被替换**：collector 不能删/换 reminder（`AddReminder` 只 append，
  重建会丢掉其它 reminder 的运行态），所以 `LSPDiagnosticsReminder` / `MemoryRecallReminder` 从
  构造起就被 agent 自己持有（`lspReminder` / `memoryReminder`），重建后调它们的
  `SetProvider` / `SetBackend` 换掉指向的对象——**"关掉"就是把指向置 nil**（各自的 Generate 会提前返回）。
- **前端注入的对象不动**：`hooksInjected` / `subagentInjected` 记录哪些是 `AgentConfig` 送进来的，
  重建只动 configure() 自己建的那些。
- 三个"要拆掉再建"的（LSP 的子进程、memory 的 backend、subagent 的 executor）都遵循
  **先拆后建、拆完能失败**：`ReloadLSP` 先 `Shutdown`（否则留下孤儿的语言服务器进程），
  `ReloadMemory` 先把 backend 卸下（工具注销 + reminder 置 nil）再建新的，建失败就停在"memory 关着"，
  而不是半个装上。
- MCP 那三条是**共享层**，所以由 app 驱动、只做一次：`RecomputeAutoLoad` 从**池里已有的工具**重算
  auto-load（不重连任何 server），并把在线会话的 discovered set 一起更新；`mcp_tool_refresh` 的
  poller 回调会绑到调用它的那个 agent 上，所以只能从一个 agent 重启。

### 两个边界情况

- **`max_iterations` 对 desktop 是「不适用」而非「需重启」**：desktop 走
  `NewAIAgentWithConfig` 不传 `MaxIterations` → 预算 0 = 无限（交互式前端不该被 config 卡住）。
  只有 `tachi -p` 真正读它（`main_agent.go:39,139`）。设置页要么不显示，要么标注「仅命令行模式」。
- **`max_tokens` 分层不同**：desktop 每轮现读 `d.cfg.MaxTokens`（`agent_turn.go:237`）→ 热；
  TUI 启动时拷进 `ChatOpts`（`main.go:283`）→ 冷。同一个配置项在不同入口分层不同。

### 五之三、两处结构性阻塞，以及它们的正确解法（v2.1 反查所得）

分层表里最难的两处不是"多写几行"，而是**对象换了、引用换不掉**。两条都已核实，解法都不是
"重建整个 collector"。

**1. reminder 侧的引用换不掉。** `systemreminder.Collector` 只有 `AddReminder`（只 append，
`agent/systemreminder/reminder.go:119-123`），接口也只有 `Collect` / `CollectPieces` /
`AddReminder` 三个方法（`agent/reminder_collector.go:14+`）；而 `LSPDiagnosticsReminder` 与
`MemoryRecallReminder` 都把对象**当字段捕获**（`agent/agent_configure.go:159`、`:500`）。于是：

- 给接口加 `Remove`/`Replace`：破 `disabledReminderCollector` 与一批测试 fake；
- 重建整个 collector：**丢状态**——`DeferredToolReminder` 的 `hasFired`/dirty、channel 的
  thread reminder、`SkillListReminder` 的 dirty 标记都是运行时注册进去的，重建等于清空它们
  "已经注入过"的记忆（表现为提醒重复出现）。

解法是第三种，而且**已有先例**：照 `SkillListReminder.SetProvider`
（`agent/systemreminder/skill_reminder.go:45`）给这两个 reminder 加一个可换的 provider 字段
（换完顺手 `dirty = true`）。collector 零改动，`lsp.*` 与 `memory.type` 都不需要重建它。

> ⚠️ 反向的坑也记在这里，免得下次又顺手写"重建就行"：`git_reminder` 在 v2.0 被判成
> 「需重建 `ReminderCollector`」，而这里**重建是有害的**（上面三个状态都会丢）。它现在是个
> **无状态空结构体**（`agent/systemreminder/git_reminder.go:15`），加一个惰性开关、改成
> **无条件注册**（照 `attachSharedMCPReminder` 的写法：注册永远发生，开关由 reminder 自己读）
> 即可从 🔴 直达 🔥。

**2. `logs.level` 只差一层间接。** 实测：`cfgLevel` 是标量全局、`textHandler.minLevel` 是
**构造期快照的值**、全库 `LevelVar` 出现 0 次（`pkg/logger/logger.go:17-24`、`:230-234`、
`:273-275`）。但所有 handler 都取自同一个来源，所以：

```go
cfgLevel = slog.LevelInfo        →  cfgLevelVar = new(slog.LevelVar)      // Init 写它
textHandler.minLevel slog.Level  →  minLevel *slog.LevelVar               // Enabled 里 .Level()
func SetLevel(s string)          // 热改入口，一处
```

`LevelVar.Level()` 是原子的，热路径不加锁；`New`/`With` 出来的所有 logger 共享这一个 `LevelVar`，
所以**改一次全进程立刻生效**，既不用重建 logger，也不用碰那几十处构造期持有 `*logger.Logger`
的字段。`logs.max_size`/`max_files` 仍然是 🔴（见上表）。

## 六、核心挑战：保存不能毁掉用户手写的配置

现状：

- `config.Save(cfg)`（`config/config.go:1064`）是 `yaml.Marshal` **全量覆盖**，
- 且**全库 0 处调用**（连测试都自己 `yaml.Marshal`）——设置页将是它的第一个使用者。

用户的 `~/.tachi/config.yaml` 含注释、引号与手工格式。全量 `Marshal` 一次就会丢掉全部注释，
**不可逆**。

### 方案：`goccy/go-yaml` 的 Path API（✅ spike 已做，方案已定）

**结论：用 `github.com/goccy/go-yaml`，不用 `gopkg.in/yaml.v3`。** 两个库都实测过，
用真实的 63 行 config.yaml 改 1 个叶子值：

| 方案 | 改 1 个值的行差异 | 注释 | 缩进 / 序列风格 | 流式写法 |
|---|---|---|---|---|
| `yaml.v3` + `SetIndent(2)` | **70 行**（全文件重排） | ✅ 保住 | ❌ 2→4 空格、序列被加宽 | ✅ |
| **`goccy` 的 `Path` API** | **3 行**（只改目标） | ✅ 保住 | ✅ **逐字节保住** | ✅ |

yaml.v3 的两个根因，都不是「小心一点」能绕过的：

1. **缩进是 encoder 的全局设置，不是节点属性。** `yaml.Node` 不记录缩进，默认
   `best_indent = 4`；`SetIndent(2)` 能修好标量/映射缩进，但**修不了序列**——
   序列缩进是「父级缩进 + `best_indent`」硬编码，yaml.v3 没有 indentless-sequence 开关。
2. **必须「就地改 `node.Value`」，不能替换 value 节点。** 替换会让挂在节点上的
   `LineComment` 一起消失（实测）。

goccy 走的不是这条路：它有 yaml.v3 完全没有的 **`Path` API**（`$.providers[0].model`），
直接对 AST 做外科手术式替换，不动其余节点。这是实测中最关键的发现。

### 已实现的接口（`config/save_partial.go` + `config/value_path.go`）

```go
func SavePartial(updates map[string]any) error
func SavePartialTo(path string, updates map[string]any) error  // 测试/显式路径
```

v2.2 起还有**同一套点分路径**的内存侧读写（`config/value_path.go`），它们与 `SavePartial` 共享
一个语法、一个叶子规则：

```go
func ReadPath(cfg *Config, key string) (any, error)          // 表单显示的"生效中的值"
func SetPath(cfg *Config, key string, value any) error       // 原地写一个叶子
func ValidateUpdates(updates map[string]any) error           // 写盘前的类型校验（复用 renderScalar）
func ApplyUpdates(cfg *Config, updates map[string]any) error // 一批，全有或全无
func SnapshotLeaves(cfg *Config, keys []string) ([]LeafValue, error) // 精确快照
func RestoreLeaves(snap []LeafValue)                          // 还原
func YAMLPaths() []string            // 结构上的全部叶子（`providers.*.model`），守卫测试用
func PathsFor(cfg *Config) []string  // 按值展开（`providers.0.model`），表单用
```

**叶子规则**（两处必须一致，否则表单会给出 `SavePartial` 渲染不了的路径）：
**标量集合是一个叶子**（`review.allowed_tools` 整条列表就是一个值，`renderScalar` 接受 `[]string`），
**结构体集合是每个元素一个叶子**（`providers.0.model`）。

两个落地时才看清的细节：`config.LoadFrom` 会跑 `defaults.Set`，所以**带 `default:` tag 的指针字段
在加载后已经被 materialize**（`ReadPath` 的"nil 读作默认"只对没有 tag 的字段与代码里构造的 config 生效）；
而 `map[string][]T` 里的**元素是可写的**（切片头拷贝、底层数组共享），`hooks.events.<event>.<i>.command`
在 `SetPath` 下真的能写。

- **路径形式统一为点分**：`"language"`、`"lsp.enabled"`、`"providers.0.model"`。
  goccy 要的是 `providers[0]`，翻译在 `yamlPath` 里一处完成，调用方与错误信息统一说点分。
- **值按类型渲染**：`string` / `bool` / `int` / `int64` / `float64` / `[]string`，
  以及**指针形式**（`*bool` 等）——配置里那些可选字段本身就是指针，传指针进来就不用在
  调用方拼格式化。渲染出的字面量再**解析成节点**（而不是包成 `ast.String`），
  这样标量、列表走同一条路。
- **字符串只在必要时加引号**（`"12345"` 必须保住是字符串，普通文本保持裸写，
  否则每次保存都会改写用户的写法风格）。
- **`nil` 与未知类型直接报错**，不会写成 `null` 悄悄清掉一项设置。

### 两个实测陷阱（v1.0 完全没提）

1. **不存在的路径 `ReplaceWithNode` 会静默"成功"。** 返回 nil 却什么也没写。
   所以 `SavePartial` **写盘前回读校验**每个路径的实际值——这个校验同时覆盖了
   "路径不存在"与"值没生效"两种情况。**只更新已存在的键**是明确的产品决策：
   新增一个键是另一个功能（放在哪个块下？），不在这里猜。
   （v2.7 把这条**收窄但没有推翻**：唯一被允许的新增是"父块已经在文件里"的那一个键
   （`spec:` 在、`context_window` 不在），以及列表末尾追加/删除一项——见 §八之五。
   两者都没有"放哪儿"的问题：父块写了位置，列表的末尾是唯一的末尾。新的顶层块、
   中间插一项、越界下标，仍然一律报错。）
   **v2.8 修正：这条规则放宽成"路径点名的 mapping 块缺失就建出来"**（含顶层块）。
   原因就是上面那条"父块在不在"的判据：路径已经点名了块放在哪儿，"该放哪个块下"从来
   不是问题，而"块本身没写进文件"（`omitempty` 的 `spec:` 是典型）会让设置页把**它自己
   展示的字段**拒绝掉——叶子在内存里有、文件里没有，用户看到的是一个"坏掉的表单"。
   仍然一律报错的是路径答不出来的那些：缺失的序列元素（新条目的骨架与"只能在末尾"是
   另一个入口 `AppendListItem` 的事）、穿过标量/列表的路径、文件里没有的 map 键。
   （实现：`ensureBlock`；测试：`TestSavePartial_CreatesTheBlocksThePathNames`、
   `TestSavePartial_RefusesWhatItCannotCreate`。）
2. **行尾注释前的对齐填充会被规范化。** `main          # 注释` 变成 `main # 注释`。
   注释内容活着，对齐空格没了。这**不是**被改行才有的——**整个文件的所有行尾注释**
   都会。所以本包的承诺是「**文件内容被保留**」，不是「文件逐字节不变」，
   并且用 `TestSavePartial_TrailingCommentPaddingIsNormalized` 钉住。

### ⚠️ 依赖版本有硬下限：`goccy/go-yaml >= v1.19`

**v1.18.0 会静默丢掉行尾注释**（实测：同一个文件、同一个编辑，v1.18 输出
`extensions: [".go"]`，v1.19 输出 `extensions: [".go"] # 紧凑写法`）。

更麻烦的是它的触发条件：**只在文件里有多条注释时出现**（单注释的最小文件两个版本都正常），
所以一个只看小 fixture 的测试会漏掉它，而真实用户的配置文件一定有注释。
`go.mod` 现在钉 `v1.19.2`；**降级这个依赖等于悄悄开始毁用户的配置**，不要降。

> 追查记录：这个 bug 一开始表现成"我的测试 fixture 有问题"，因为 spike 用 `@latest`（v1.19.2）
> 而 go.mod 里已是 v1.18.0（ginkgo→go-snaps 带进来的间接依赖）。**同一份 fixture、
> 同一份代码、两个版本、不同结果**——这才定位到版本。升级依赖后测试立刻全绿。

### P0 验收标准（✅ 已实现于 `config/save_partial_test.go`）

1. 改一个叶子值后，**行数不变**、目标行带新值、**其余行除对齐填充外逐字不变**
2. 注释（顶部 / 行尾 / 独立）、缩进、**indentless 序列**、流式写法、引号全部保住
3. 嵌套路径与序列索引都可达（`providers.0.spec.vision`、`lsp.servers.0.command`）
4. **失败路径**：不能按路径创建的键（缺失的序列元素 / 穿过标量或列表 / 文件里没有的 map 键）、
   越界索引、`nil`、未知类型都报错，且**文件字节级不变**
5. 无编辑时不重写文件；文件 mode 保持（0600 不会变 0644）

### 原子性

顺序必须是（v1.0 把前两步写反了，会导致「文件已写、内存回滚」的矛盾状态）：

```
1. 校验 updates（类型、取值域）
2. 应用变更到内存（d.cfg）—— 失败即回滚，此时文件未动
3. 序列化到临时文件 → 失败即放弃，内存回滚到快照
4. 原子 rename 临时文件 → config.yaml
5. 各 setter 的运行时推送（v2.5 起在写盘成功之后，见 §七「保存的顺序」）
```

第 4 步失败：内存已改是**可接受**的（下次启动会读回旧值），但必须报错，不能静默。第 5 步
排在最后是**不可调换**的：推送没有撤销路径（它换日志级别、重建工具与 backend、重连 MCP server），
在写盘之前跑，一次写盘失败就会让运行态停在新值上，而回答却承诺「什么都没变」。

**写盘是原子的**（`SavePartial` 内部用 `fileutil.AtomicWriteFile`，沿用文件原有的 mode）。
理由与 session 的 meta.json 相同：config.yaml 有并发读者（另一个 Tachi 进程启动时
`config.Load`），而 `Save` 的 `WriteFilePrivate` 是就地重写，读者可能读到截断文件。

### 内存侧的并发（✅ 已实现）

文件那一半之上还有一半：`d.cfg` 今天在启动后就**只读**，所以无锁也安全——但设置页的
全部意义就是运行时改它，那一刻 `d.cfg` 就被跨 goroutine 写了（turn goroutine 读
`MaxTokens` / `Language`，MCP 后台 goroutine 读 `MCPServers`，每个 agent 把它当
`FullConfig` 持有）。

已建好的地基（`desktop/agent.go`）：

```go
func (d *desktopApp) config() *config.Config            // RLock，只读访问
func (d *desktopApp) withConfig(fn func(*config.Config)) // Lock，唯一写入口
func (d *desktopApp) setConfig(cfg *config.Config)       // 仅 initAgent
```

⚠️ **铁律：只能原地改字段，永不换 `d.cfg` 指针。** 换指针看起来更干净，但会静默出错——
指针被这些东西**长期持有**：

- 每个 provider 被 `wrapForUsage` 包过，它的 price/credit resolver **闭包捕获**了
  `*config.Config`，每次 LLM 调用才解引用 → 换指针后**账本继续按旧价格记账**；
- 每个会话的 agent 把它当 `AgentConfig.FullConfig`（26 个读点）；
- 共享 MCP manager 及其后台 refresher 也持有它。

这些持有者是**惰性读取**的，这正是"原地改一处、到处生效"的原因，也正是
"换指针则到处失效、且不报错"的原因。所以：

- **不能用 `atomic.Pointer[config.Config]`**：它为"换指针"提供安全，而换指针在这里是错的
  （代价是 12 个闭包 + MCP manager + 26 个读点全部改成"每次去取"）。
- **一次保存是一批叶子，不是一个。** v2.0 写的是"一次保存只应用一个叶子值"——那是按当时的
  假设（表单逐个字段提交）写的，而 UI 是**每个分类一个保存按钮**，所以实际会一次带上多个字段。
  落地时改成：`config.ApplyUpdates(cfg, updates)` 在**一把锁内**全有或全无地改（失败即把已改的
  原地还原），所以"多字段撕裂"依然不可能被读到；写盘仍然是**一次** `SavePartial`（它本来就吃 map）。
- **回滚存的是原始值，不是表单读到的值。** `config.SnapshotLeaves` 在改之前抓一份**精确**内存快照
  （`LeafValue`），写盘失败时 `RestoreLeaves` 用它还原。不能拿 `ReadPath` 的结果来还原：它把
  **带 `default:` tag 的 nil 指针**读成默认值，于是"还原"会把一个未设置的字段写成显式值——
  一个没人要求过的配置改动，由失败路径写出来。
- 唯一例外的写：`persistMCPServerEnabled` 就地改 `MCPServers[i].Enabled`（一个 bool），
  在 `mcpServerByName` 的注释里写明了为什么可接受。


## 七、实现结构

`App.tsx` 已有 2300 行，设置页**不写进去**。只加一个 view 状态：

```tsx
// App.tsx
const [view, setView] = useState<'chat' | 'settings'>('chat')
```

```
App.tsx
  └─ view === 'chat'     → 现有布局（titlebar + sidebar + main）  ← CSS 隐藏，不卸载
  └─ view === 'settings' → <SettingsPage onBack={…} />
```

新文件：

| 文件 | 职责 |
|---|---|
| `desktop/frontend/src/settings.tsx` | 设置页外壳：分类导航 + 表单渲染 + 保存 |
| `desktop/settings.go` | `AgentService` 的新方法：`GetSettingsSchema` / `SaveSettings` |

### 数据流

后端**声明**可配置项（而不是前端硬编码），每条带元信息。**✅ 已实现**（`desktop/settings.go`），
字段比 v2.0 的设想少两个（`Label` / `Help`）：`Key` 就是路径、也是前端能显示的名字，`Note`
就是帮助文本。v2.6 加回来的四列都不是"先摆着"的空列，每一列都对应一个前端**否则会画错**的
事实：`Category` 是"这一行画在哪"（§八之四 ①），`Options` 只在该叶子真的有封闭取值集时才有值
且**从配置或常量算出**（④⑥），`Optional` / `IsSet` 是"unset 是不是一个含义独立的状态"（⑤）：

```go
type SettingFieldVO struct {
    Key          string   // 点分路径，如 "language"；也是 SaveSettings 收回去的键
    Kind         string   // "string" | "bool" | "int" | "float" | "string_list" | "other"
    Value        any      // 生效中的值（未设置的指针读作它的 default）
    NeedsRestart bool     // ← 后端派生（settingApply.needsRestart()），前端只读
    Category     string   // ← 后端声明（settingApply.Category）：画在哪一组
    Options      []string // ← 有封闭取值集时的可选项；空 = 自由文本
    Optional     bool     // ← 后端派生（config.LeafInfo）：这个叶子可以「未设置」，而且那是一种状态
    IsSet        bool     // ← 同上：它现在有没有值（Optional && !IsSet = 自动）
    Note         string   // 为什么要重启 / 这个键属于哪个入口模式
}
```

`Kind` 由值的**动态类型**推导，不逐行声明：行的 `Key` 已经固定了 Go 字段，第二处声明就是第二个
会写错的地方。schema 也不是"一行一个字段"——遍历的是**config 的叶子**（`config.PathsFor`），用表
过滤：`lsp` 一行管十一个叶子，表单要的是十一个输入框，各自带这一行的 mode 与 note。

「要不要重启」由后端**代码保证**——新增一个配置项时，忘了标 `NeedsRestart` 会是最典型的
错误。**✅ 已落地**（§八之三）：它不是 SettingField 上要人记的字段，而是由应用表的
`needsRestart()` **派生**出来的一条：`Apply == nil` 且该行不是 lazy/unused，就等于"今天只能靠重启"。
没有标记可忘。

```go
// desktop/settings.go（已实现，这里只是形状）
type settingApply struct {
    Key   string                                       // 与 SettingField.Key 同一份
    Mode  applyMode                                    // applyLazy / applyPush / applyRestart / applyUnused
    Apply func(*desktopApp, *config.Config) error      // 只做"推送"那一半，nil = 代码还没写
    Note  string                                       // restart / unused / push-未实现 必须说明理由
}
func (s settingApply) needsRestart() bool
func settingFor(key string) (settingApply, bool)       // 最具体的一行胜出
```

`SettingFieldVO` 由这张表派生（`Key` 来自表项、`Kind` 由值的类型推导、`Value` 来自 `config.ReadPath`、
`NeedsRestart` 取 `needsRestart()`、`Note` 取表项的 Note），**reflect 守卫测试**则断言
`config.Config` 的每个 yaml 叶子都有处置（§八之三 ②）。没有这道守卫，漏标迟早发生一次。

### 保存的顺序（✅ 已实现于 `AgentService.SaveSettings`）

`SaveSettings(updates map[string]any) string` —— 返回的是给页面显示的**一句话汇总**，不是错误
（desktop 的绑定惯例：前端不翻译 Go 错误）。每一步都是因为上一步会留下什么，**推送排在写盘
之后**（v2.5 起）：

1. **逐个键解析**（`settingFor`）：未知键、`applyUnused` 的键 → **整单拒绝**，一个字节都不动；
2. `config.ValidateUpdates` —— 写盘会用的那张类型表，**在改内存之前**跑；
3. `config.SnapshotLeaves` 抓精确快照 → 一把锁内 `config.ApplyUpdates`（全有或全无）；
4. `config.SavePartial`。写盘失败 → `RestoreLeaves` 精确还原内存，回答里明说
   「没能写入 config.yaml……（这次改动已撤销，内存与文件都没变，运行中的进程也未被通知）」——
   这句现在为真，因为**推送还没有发生**；
5. **最后**逐行 `Apply` 推送（失败按会话收进汇总，不致命；值已经存进文件了）。
   顺序不可调换：推送没有撤销路径（日志级别、工具与 backend 重建、MCP 重连），先在写盘之前跑，
   一次写盘失败就会让运行态停在新值上，而回答承诺「什么都没变」。整个序列由 `d.settingsMu` 串行化，
   两个并发保存不会交错各自的快照与推送。
   推送**不落在正在跑的回合上**：`pushing` 期间扇出遇到 `r.running` 的会话就入队
   （`desktop/deferred_push.go`），由 `endTurn` 执行——那正是这个会话没有回合在飞的时刻。
   汇总把它单算一类（`N 项等这一轮结束生效（键名）：会话「甲」正在运行`），不计入「已生效」。
   进程级与共享层的行不延后（日志级别、prompt 缓存清理、MCP manager 自己的上限、profile 切换、
   轮询器）：它们不换回合裸读的字段，目标也是共享层——那里的"会话"是显示中的那个，不是运行中的那个。

汇总的形状：`N 项已生效` / `M 项需重启后生效（键名…）` /
`K 项等这一轮结束生效（键名…）：会话「甲」正在运行` / `有会话没能跟着更新：…`，按实际发生拼接。

⚠️ **多会话注意**：`d.cfg` 是全进程唯一的一份，而 `AIAgent` 是**每会话**一个。
热改一个配置要遍历会话（先例见 `desktop/agent_mcp.go` 的 MCP toggle），不能只改当前显示的那一个。
这条已落地为 `d.applyToLiveAgents(fn) (failures []string)`（`desktop/agent.go`）：它对每个**已建出
agent 的**会话跑 `fn`，把失败收成「「标题」：原因」的行（无标题时用 session id），并按 id 排序，
所以保存后的汇总能一次说清是哪一个会话没吃下改动。MCP 的两处 fan-out 也已改走它——原来那个
返回 `[]*agent.AIAgent` 的 `liveAgents()` 因此删掉了（同一个形状留一个）。

另一件同源的事：**`Apply` 只做"推送"那一半**。改值本身由 `SaveSettings` 在 `withConfig` 里完成，
表里的 `Apply` 只负责让已经构造出来的东西跟着变（重建工具、清 prompt 缓存、广播到各会话）——
否则同一个叶子会有两个写者。

**唯一的例外是 `OwnsWrite`**（目前只有 `active_mcp_profile` 一行）：这一行的推送自己就是那个写者，
批次必须跳过它。理由是"改了没生效"的反面——profile 的切换可能失败（profile 文件读不出来），
而批次先写、推送后失败就会让字段停在一个服务器并未加载的 profile 上（面板会把它显示成 active）。
`OwnsWrite` 的行从 `d.savingUpdates` 取自己的新值，并在**切换成功之后**才写（`switchMCPProfileTo`，
走 `cfgMu`）；失败时字段保持旧值，文件里有的是用户存的意图，汇总里报的是这次推送失败。
守卫测试（`TestSettingsTableRules`）要求 `OwnsWrite` 必须是带 `Apply` 的 push：没有代码就等于没有写者。

### 各入口的一致性

`AgentService` 的改动是「四个面」（见 `docs/agents/desktop.md`）：Go 方法、重新生成的
TS binding、前端、smoke driver。**不要**手改 binding，跑：

```sh
cd desktop && GOWORK=off wails3 generate bindings -clean=true -ts -i
```

## 八、分期

| 期 | 内容 | 风险 |
|---|---|---|
| **P0-a** | ✅ **已完成**：`config.SavePartial` + `config/save_partial_test.go`（§六的验收五条，全部通过） | — |
| **P0-b** | ✅ **已完成**：`d.cfg` 的 `cfgMu` + `config()` / `withConfig()`，全部读点改访问器 | — |
| **P1** | ✅ **已完成**（§八之四）：`App.tsx` view 状态 + `settings.tsx` 外壳 + 分类导航 + CSS 隐藏保状态 + 屏蔽对话快捷键 | 低 |
| **P1.5** | ✅ **结构前置已完成**（§八之三：应用表 + 键覆盖守卫 + 广播原语）；其余小改见 §八之二 | 低 |
| **P2** | ✅ **已完成**：后端 `GetSettingsSchema` / `SaveSettings`（读 `settingsTable`，写 `SavePartial`）+ 🔥 类表单 + 逐项补 `Apply`（前端见 §八之四） | 中 |
| **P3** | ✅ **已完成**：🟡 / 🔴 类表单 + `⟳` 徽标（由 `settingApply.needsRestart()` 派生）+ 保存后汇总提示 | 低 |
| **P4** | ✅ **已完成**：`itest/desktop` 的 `settings` 场景（进页面 → 分组/卡片/下拉 → 改一项保存 → 「自动」保存 → 返回；Go 侧读 config.yaml 并用 `config.LoadFrom` 装载） | 低 |

**P2 拆成两半，后端那一半 ✅ 已完成**（2026-09-18）：`config.ReadPath`/`SetPath`/`ApplyUpdates`/… +
`GetSettingsSchema`/`SaveSettings` + 重生成的 binding；前端那一半随 P1 一起落地（§八之四）。
表里 22 行 push 中仍有 19 行 `Apply == nil`，它们落在「需重启」那一档，直到逐项补上。

P0 已先做完，理由不变：它决定了「保存会不会毁掉用户手写的配置文件」这个不可逆后果，
而且 P0-b 的锁约定决定了 P2 的 `SaveSettings` 怎么写（唯一写入口 `withConfig`；唯一例外是
`persistMCPServerEnabled` 那处就地写，见 §六）。

## 八之二、开工之前：小改（每条都让 P2 少一个特例）

都不阻塞 P1（纯前端），但做了能让 P2 的 apply 表少一类特例。前两条是 v2.0 列出的，
其余来自 v2.1 的逐条反查：

| 项 | 为什么值得先做 | 规模 |
|---|---|---|
| `ckptKey` 纳入 `checkpoint.Options` | ✅ **已完成**：key 现在含三个 Options，manager 在下次用到时按新值重建（`TestCheckpointManagerKeyCarriesItsOptions` 钉住"改了要重建、无关改动不重建"） | — |
| `promptCache` 清空入口 | ✅ **已完成**：`desktopApp.invalidateSystemPrompts()`（`desktop/agent_driver.go`），`language` / `extra_system_prompt` 两行真的热了（`TestLanguageRowClearsThePromptCache` 钉住） | — |
| reminder 换 provider 的先例 | ✅ **已完成**：`LSPDiagnosticsReminder.SetProvider` / `MemoryRecallReminder.SetBackend` + agent 自己持有那两个实例——collector 层零改动，`lsp` / `memory` 两行因此可热改（§五之三 1） | — |
| `GitReminder` 惰性开关 + 无条件注册 | ✅ **已完成**：`GitReminder.Enabled` 每次生成现读，注册无条件（`agent/systemreminder/git_reminder_test.go`）——避开了"重建 collector 会丢状态"的坑 | — |
| `logs.level` 的 `LevelVar` | ✅ **已完成**：`pkg/logger` 的级别改成一个共享 `LevelVar` + `SetLevel`，**已建的 logger 跟着变**（`TestSetLevelReachesLoggersBuiltBeforeIt`、`TestSaveSettingsMakesALogLevelChangeLive`） | — |
| `tool_result.*` 让 MCP manager 现读 | ✅ **已完成**：`Manager.SetToolResultLimits`（原子字段，避免与工具调用竞态），与工具重建一起推送 | — |
| `hooks.*` 重建时显式置 nil | ✅ **已完成**：`ReloadHooks`（禁用时显式清空、前端注入的 dispatcher 不动、**"从没有 → 新增"这条路径也覆盖**） | — |
| MCP profile 的**加载侧**按会话树 | ✅ **已完成**：`reloadMCPForActiveTree`（`desktop/agent_mcp.go`）复用 `SwitchMCPProfile` 按会话树重读配置并做 diff 调和，挂在会话激活/载入、会话换目录、项目改根与删项目、以及 bootstrap 连接结束（`MarkInitDone` 之后）这些"显示中的树可能移动"的入口上；`TestActivateSessionLoadsTheProjectsMCPServers` 钉住"开会话即读到该树的 mcp.json"。共享 manager 的胜出树是**显示中的会话**（与查询侧一致），按项目拆开仍是 `desktop-project-design.md` §P4 的事 | — |

## 八之三、开工之前：结构前置（三条，零 UI 依赖）

动机只有一句：让 P2 的 `SaveSettings` 是**一次循环**（校验 → `withConfig` → `SavePartial`
→ 广播），而不是二十个特例。

**✅ 三条都已落地**（2026-09-18，`desktop/settings.go` + `desktop/settings_test.go` +
`desktop/agent.go`）。落地时与下面的设想有三处不同，都是动手才看清的：

1. **应用表放在 desktop，不在 agent。** 设想里倾向 agent 侧（便于 tui/acp/channel 复用），
   但多数行的运行期那一半是 desktop 独有的：prompt 缓存、共享 MCP manager、跨会话广播。
   放进 agent 就得反过来给它一个 app 级别的钩子接口。表里的 `Apply` 因此收的是
   `func(*desktopApp, *config.Config)`；纯 agent 知识的那些写成闭包，将来要下沉不必动表的形状。
2. **豁免不是一份 `notConfigurable` 名单，而是第 4 种 mode（`applyUnused`）。** 与
   `applyRestart` 分开：前者是"重启后才能生效"，后者是"这个进程根本不读它"（tui/channel/acp/web 的键），
   两者对读者的要求不同（重启 app vs 去改另一个进程读的文件）。
3. **形态比设想的多一个 `applyLazy`。** "什么都不用推"和"推不动"是两件事：`compact.*`、
   `oneoff.*`、`checkpoints.enabled` 的读点本来每次现读，标成 push 会凭空多出一个不存在的动作。
   于是 mode 有四种，而 `NeedsRestart` **不是字段**——它由 `needsRestart()` 派生：
   `Apply == nil` 且不是 lazy/unused，就是今天只能靠重启（含"push 的代码还没写"这一状态）。
   没有可以忘记标的标记。

落地的东西：

| 件 | 位置 |
|---|---|
| `settingsTable`（41 行，覆盖 166 个 yaml 叶子）+ `settingFor`（最具体的一行胜出） | `desktop/settings.go` |
| `applyMode` / `settingApply` / `needsRestart()` / `offered()` / `pushError` | 同上 |
| `yamlLeafPaths`（reflect 遍历器）+ 5 个守卫测试 | `desktop/settings_test.go` |
| `liveRuns` / `applyToLiveAgents`（按 id 排序、失败带会话名） | `desktop/agent.go` |
| `invalidateSystemPrompts`（§八之二 的第 2 条，顺带做了） | `desktop/agent_driver.go` |

守卫测试钉住的是四件事：每个叶子都有处置（新配置项不可能悄悄进来）；没有空行（键写错会红）；
每种 mode 的规则（lazy 不许带 Apply、push 没代码就必须有 Note、restart/unused 不许有代码且必须
有理由）；`settingFor` 取最具体的一行（`checkpoints.enabled` 是 lazy 而 `checkpoints.max_bytes`
是 push；前缀按**段**匹配，`provider` 不会吃掉 `provider_aliases`）。

`liveAgents()`（返回 `[]*agent.AIAgent`，没有 session 身份、没有错误聚合）在 MCP 两处改走
`applyToLiveAgents` 之后被删掉了——同一个形状只留一个。MCP 的 fan-out 本身没有变化，只是换了入口。

## 八之四、前端的落地记录（v2.6）

新增两个文件，改了三处既有的：

| 文件 | 职责 |
|---|---|
| `desktop/frontend/src/settings.tsx` | 页面：分类导航 + Kind 驱动的表单 + dirty 跟踪 + 保存 + 汇总 |
| `desktop/frontend/public/settings.css` | 它的样式（`index.html` 多引一行） |
| `desktop/settings.go` | 多 `Category` 一列、`settingChoices` 一张表、`SettingsSchemaVO`、`SaveSettingsResultVO`、`normalizeUnsets` |
| `config/value_path.go` | 多 `Unset` / `LeafInfo` / `DescribeLeaf` / `KindOf`（`kindOf` 从 desktop 搬过来，config 才是类型的归属） |
| `config/provider_defs.go` | 多 `ProviderTypes()`：三个类型常量与 llm 的构造 switch 共用一份 |
| `desktop/frontend/src/App.tsx` | `view` 状态、footer「设置」启用、`app-body.is-hidden`、门禁 window keydown |
| `desktop/frontend/public/{base,layout}.css` | `--app-bg` / `--titlebar-h` 两个变量、`.app-body.is-hidden` |
| `itest/desktop/{drivers/settings.js,scenarios.go}` | P4 的 smoke 场景：真机点进设置页，Go 侧回读 config.yaml |

**这个页面不认识任何一个配置键。** 名字（`key`）、值、输入框形态（`kind`）、要不要 ⟳
（`needsRestart`）、归到哪一组（`category`）、以及那句解释（`note`）全部来自后端；前端只做
三件它该做的事：把草稿变成值（`parseField`）、判断哪几行改过、以及保存被拒时**保住用户刚敲的字**。
加一个配置项因此仍然只改 Go（§八之三 ①的口径）。

### 七处与本文档设想不同的决定

1. **分类进了后端表**（`settingApply.Category` + `settingsCategoryOrder`）。文档 §三 只画了
   五个分类名，没说它从哪来；放进表里才和 `Mode`/`Note` 同源——前端那份映射会是第二份需要
   手工同步的配置键清单，而守卫测试看不见它。守卫测试现在双向要求：**offered 的行必须有
   分类、unoffered 的行必须没有**（并且分类必须在有序列表里、每条分类至少有一行、
   覆盖同一键的两行必须同分类——`logs` 与 `logs.level` 是今天唯一一对）。
   七个分类：通用 / 模型 / 工具与权限 / MCP / 会话与上下文 / 扩展能力 / 高级。
   文档原来写的「外观」没有落点（主题是 desktop 的 UI 状态，不在 config 里），所以它不存在
   而不是空着——nav 里出现一个打不开任何东西的入口比没有更糟。

2. **`SaveSettings` 回的是一对值，不是一句话**：`{ok, summary}`。理由是页面要拿这个答案做
   两件不同的事——**写成**了要重读一遍（值已经是新的）并清空草稿，**被拒**了必须原样留着
   读者的输入（他正要按那句话去改）。文档 §七 定的「返回给页面显示的一句话，不是错误」
   仍然成立：`summary` 还是一句话，前端仍然不翻译 Go 错误，`ok` 只是让页面知道该做哪件。

3. **schema 回一个 VO**：`{categories, fields}`。顺序随字段一起给，前端不自己声明一份。
   字段比 §七 的设想多两列（`Category`、`Options`，两者都在下面单列）；仍然没有 `Label` 与
   `Help`——`Key` 就是路径、也是它能显示的名字，`Note` 就是帮助文本。分类不在"可选元数据"
   之列：它不是装饰，是"这一行画在哪"。

4. **选项集是第二张表**（`settingChoices`），不是 `settingApply` 上的一列。粒度不同：一行管一个
   **块**（`subagent` 覆盖十一个叶子、用同一个 `Apply` 推），而"可选值"属于**单个叶子**
   （`subagent.provider` 是 provider 名字，`subagent.max_concurrency` 是数字）。挂在行上就得为了
   一个下拉把块行拆成叶子行，还得把 `Apply` 闭包抄一份。表的**值是函数**（`func(*config.Config)
   []string`），因为可选值就是这份配置定义的东西——今天 `providerNames` 读 `providers.*.name`，
   写死的名字表会是"有哪些 provider"的第二个答案，加一个 provider 就错。
   七个键：`provider` / `title_provider` / `commit_provider` / `run_provider` /
   `subagent.provider` / `review.provider` / `memory.keyword_provider`。
   键可以是**列表项里的叶子**，用 `*` 代表一个下标或 map key——就是 `config.YAMLPaths` 写的那种
   写法（`providers.*.type`），于是守卫测试变成"这个键必须是 config 自己的结构叶子之一"的
   集合成员判断，而不是另一份清单；一个决定，不管配置里有几个 provider。
   守卫测试还要求键指向的行是 offered（认不出来的键会是没人看得见的下拉）。

5. **配置列表的叶子折成卡片**，而且是**按路径形状**折的，不是按键名。`providers.0.api_key` 是
   *路径寻址一个列表项*的方式，当地标签读不了（读者看的是"第二个 provider"，不是下标 1）。
   两条规则都只用到形状：
   - 元素边界 = 路径里**最靠右**的数字段（最内层列表），所以 `hooks.events.pre_tool.0.command`
     属于 pre_tool 列表的第 0 项，而不是某个 `hooks.events.pre_tool` 项里的 `0` 字段；
   - 标题 = 元素自己的 `name`（没有就是 `type`）+ `model`，逗号那半边是因为"main"单独出现时
     说不出它指向哪个模型。这三者是配置自己的词汇（它 yaml 里的词），所以可以是前端常量；
     一个都没有的元素退回显示自己的路径——丑，但那是它唯一的名字。
   折叠的默认值按大小：超过 3 个叶子才折（一个 hook 命令是命令 + 超时，折起来多花一次点击），
   带**填错**的元素强制展开（藏在折叠里的错误没法处理）。这条同样落在 `lsp.servers.N` 与
   `web_search.providers.N` 上——同一个丑法，同一个修法。
   同一个"路径不当标签"的问题还有两层：一个分类里**一个顶层 key 的叶子归到一个标题下**
   （`web_search` / `web_fetch` / `debug.pprof`），标题取它们共享的最长段前缀，块内的行只写余下的
   路径；卡片的副标题是**列表项定位**（`web_fetch.providers.0` → `providers[0]`，去掉标题已经说过
   的前缀，但绝不丢掉给列表命名的那一段——`[0]` 单独出现是"往一个没有名字的东西里取第 0 项"）。
   标题必须**换一种排印**（界面字体 15px/640，不是键名那套 mono 13.5px）：第一版用 mono 画标题，
   结果 `web_fetch` 在页面上就是"一条没有输入框的配置项"，读者会去找它的输入框。没有这一层，`web_search.*` 与 `web_fetch.*` 在一条平面列表里只能靠读者自己去认前缀，
   而列表卡片还插在中间（这正是"web_search.xxxx、web_fetch.xxxx 能不能放在子标题下"的由来）。
   只有一个条目的块不出标题——那会是一行噪音，此时那一条照旧写全路径。

6. **`providers.N.type` 是封闭集**，不是自由文本：`openai` / `openai-res` / `anthropic`，
   取自 `config.ProviderTypes()`（与 llm 的构造 switch 同一批常量）。这个字段**装载时不校验**，
   拼错照样 parse 成功，然后在某个会话解析 provider 时才炸（`unsupported provider type: …`）
   ——正是下拉能挡掉的那类失败。它与 ④ 的 provider 名字不同：一个是配置算出来的，一个是常量，
   但两者都走同一个"值是函数"的形状，调用方不必知道是哪种。

7. **「未设置」是一等状态**（`config.Unset` + `config.DescribeLeaf`）。`providers.N.spec` 下的
   `context_window` / `max_retries` / `vision` 是 `*int64` / `*int` / `*bool`，它们的 nil 不是
   "空"，而是一个有含义的第三态（`vision: nil` = 按模型能力表自动判断）。要能在界面上改，缺的
   不只是控件：**路径 API 没有任何办法把指针写回未设置**。所以：
   - `config.Unset` 是一个**具名值**，写进文件是 `null`（装载回来就是 nil，已实测回环）。
     不用裸 nil 是有意的——nil 正是"忘了解引用指针"的样子，而把它写进用户的文件会悄悄清掉一个
     没人想动的设置，`SavePartial` 一直拒绝裸 nil（`TestSavePartial_NilIsRefused` 钉着）。
   - `DescribeLeaf` 回答值的形态答不出的两件事：叶子**声明**成什么（未设置的 `*bool` 也是个开关，
     不能因为读出来是 nil 就画成"改不了"），以及它现在有没有值（`Optional && !IsSet` = 自动）。
     `Optional` 只对"没有 `default:` 标签的指针"为真：带默认值的指针，默认值说明了 unset 的含义，
     两者是同一件事，画成普通复选框就好（`lsp.enabled` 就是这种）。
   - 表单那边，一个 optional 叶子第一件事就是「自动」：bool 变成三选一（复选框放不下第三个位置），
     其余保留自己的控件 + 一个「自动」勾选。空值一路以 `null` 传到文件。

### 开工后照出的三处后端缺陷（都在 `config` 侧修掉）

写这个页面之前，`GetSettingsSchema` 的字段没有任何调用点——**这两条路径从来没有被端到端
走过一次**，一走就露了两个洞（第三个是能力缺失，见 ⑦）：

- **时长**：配置里有两个时长类型，跨 binding 时给的是两种答案。`time.Duration` 没有
  `MarshalJSON`，出去是 `30000000000`（纳秒，没有人会那样编辑）；`config.Duration` 有，出去是
  `"30s"`，然后**根本存不回去**——写路径只认整数。现在路径 API 两边都按时长在文件里的字符串
  形态走（`ReadPath` 返回 `"30s"`，`assignValue` 用 `time.ParseDuration` 收下），
  所以 `Timeout` 是个普通的文本框，没有自己的 Kind。
- **列表**：JS 数组过来是 `[]any`，而 `renderScalar` 只认 `[]string`——`SetPath` 的注释一直
  写着"列表以 `[]any` 到达"，只有写文件那半边没跟上。四个 `string_list` 字段
  （`permissions.bash.{allow,ask,deny}` / `review.allowed_tools`）之前一个都存不了。

- **未设置写不回去**：见 ⑦。回环那一半有 `config/save_partial_test.go` 的
  `TestUnsetWritesNullAndTheLoaderReadsItBackAsUnset` 钉着——写 `null` 而装载不回 nil 的话，
  整个「自动」都是假的。

三条各有测试钉住（另外还有 `config/value_path_test.go` 的
`TestDurationLeavesReadAndWriteAsTheFileHolds`、`TestDescribeLeafTellsAnUnsetLeafFromAnEmptyOne`，
`config/save_partial_test.go` 的 `TestSavePartial_AcceptsTheShapeABoundMethodReceives`，
以及 desktop 侧的 `TestTheFormSeesAndSavesWhatTheFileHolds`——它同时断言**页面上显示的是文件里
那个形态**、`TestProviderTypeOptionsAreAClosedSet`、`TestSaveSettingsUnsetsAnOptionalLeaf`）。

### 八之五、列表的增删（v2.7）

设置页现在能**往列表里加一项、也能删一项**（`providers` / `web_search.providers` /
`web_fetch.providers` / `lsp.servers`：`config.ListItemPaths()` —— 一组「按字段」的列表，
按路径可达、不带 map key；`[]string` 是一个叶子，`hooks.events.<event>` 的 key 得由调用方
编一个名字，两者都不算"加一项"）。

四个决定：

1. **"加一项"是点 ＋ 立刻写入文件**，不是暂存在表单里等保存。理由是更新路径本身：
   字段要能存进去，文件里得先有那个条目。暂存的话，保存时得同时造结构、还得把所有叶子
   一次写完（保存只发改过的），那是另一套更大的机制。回执里明说"填好字段后点保存"。
   代价：加了不填，文件里会留一条空条目——所以删除是这一期的一部分。
2. **写入的条目 = 结构体 marshal 出来的块**（字段顺序、`omitempty` 照旧），所以新块和它旁边
   那些长得一样。`defaults.Set` 先跑一遍：`web_search.providers` 的新条目自带 `type: exa`，
   是一个"填个 key 就能用"的起点，而不是一堆零值。
3. **列表的块不在文件里就建出来**（`web_search` 从没配过 → 新建 `web_search:` +
   `providers:`）。位置由路径给出，仍然没有"放哪儿"的问题。AST 操作踩了两个坑，都写进了
   代码注释：从别的 buffer 解析出来的节点 token 位置是错的（键会打成乱码 `ext_window`），
   键的列号要从同级/上级的键借（`keyPosition`）；空 flow 映射（骨架里的 `spec: {}`）还要
   顺便改回块样式。
4. **删除要确认第二次**（写盘立刻生效且没有撤销），并且回执里**警告谁还在引用这个名字**
   （`settingChoices` 里那些"值是一个选择"的键：`provider` / `title_provider` / 各
   `*_provider`）。它们不是列表项，删掉条目会让它们指向不存在的东西——说出来是警告，
   不说就是一份悄悄解析不了的配置。
   增删都**不推送**：新条目还没有内容；删掉的条目对运行态的影响（引用的键）由用户按警告处理。

smoke 场景 `settings` 把它跑了一遍（真机 67 项断言）：＋ → 文件里有、新卡片出现且展开 →
删 → 文件里没有，并断言了"文件里没有 web_search 块时也能加"。

### 已知待办（都不阻塞 P4）

- **还有哪些枚举该进 `settingChoices`**：provider 名字（④）与 provider 类型（⑥）已经做了。
  同一个"自由文本框拼错不报错"的问题还剩这些键：`logs.level`、`memory.type`、
  `active_mcp_profile`（值来自 mcp profile 文件，得另找来源）、`*.thinking_level`
  （`providers.N.spec` 与 `review` 各一个）。`web_search.providers.*.type`（exa / brave）也
  在名单里——它今天**故意**不是 provider 类型（结构键 `providers.*.type` 只匹配两段，匹配不到
  四段的它），要做就为它加一个自己的键。它们的可选值大多是写死的短名单。
- **`other` 的七项**（`provider_aliases` 与几个"unset = 自动"的指针）在页面上是只读的，
  并写明了"请直接编辑 config.yaml"。它们不是漏掉，是**没有单一标量可编辑**；
  给它们编一个"自动"的表示是另一件事。列表项里的同类叶子（`lsp.servers.0.env`、
  `hooks.events.pre_tool.0.env` 这些 map）也一样。
- 草稿在页面里，不在 localStorage：关掉窗口就没了。这是有意的（保存才是承诺），
  但值得记住。
- §三 的图里画了「保存后……一个『立即重启』入口（可选）」——没做。汇总句里说清了哪几项要重启，
  重启按钮涉及"怎么重启一个 desktop 进程"（连托盘、未保存的会话都在里面），是另一件事。

## 九、不做的事

| 不做 | 原因 |
|---|---|
| ❌ 新增/删除/重排 provider、MCP server | 表单复杂度之外，还有一条硬理由：`provider_aliases` 的展开与各 `Setup*Provider` 的重解析，漏一处就静默沿用旧 provider |
| ❌ YAML 的原始文本编辑 | 那是编辑器的事；设置页只做结构化字段 |
| ❌ 密钥脱敏 | 已定：直接回显 |
| ❌ 文件监听 / 自动 reload | 用户显式保存；监听引入竞态 |
| ❌ 权限规则的图形化编辑 | `permissions.bash` 是规则列表，结构复杂，留到后续 |
| ❌ MCP server 的 enable 开关 | 它**不在 config.yaml 里**：`config.Config.MCPServers` 是 `yaml:"-"`（`config/config.go:814`），内容来自 `mcp.json`（`LoadMCPServers`）。设置页若要暴露，只能走既有的 `SetMCPServerEnabled`（写 `SourceFile` 指向的那个 json），不能走 `SavePartial` |
| ❌ （暂时）死配置 `mcp_tool_search.max_deferred_schema_bytes` | 全库 **0 个读点**（只在 `config/config.go:1169` 声明）。放进表单 = 用户改了没反应；要么先接上，要么不显示 |

## 十、待确认

- 保存后是否提供「立即重启」（`app.Restart()` 或退出重开）？第一版倾向只提示不提供。
- `permissions` 有项目级覆盖（`config.LoadProjectPermissions`，`config/config.go:1227`）。
  设置页改的是**全局**那份，UI 上要说明。
- **运行时切换的持久化策略不一致**：`/model` 的 provider 切换**会**持久化到会话 meta，
  而 `SwitchMCPProfile` 明确是「in-memory only, reverts on restart」。两条同类操作行为不同，
  第一版不统一，但要在 UI 上分别说清哪条会留下、哪条不会。
- **`logs.level` 第一版就给热改吗？** 它是用户最想要的一项（排查问题时要开 debug），
  代价是 `pkg/logger` 的一次小改（§五之三 2）——比让用户改完重启便宜得多。
- **`mcp_tool_search.*` 显示为只读，还是整个不显示？** 它的热改要重连所有 MCP server
  （见 🟡 表），第一版做不划算；而显示一个改不动的开关比不显示更糟。
- **`session_cleanup_max_count` 的措辞**：它读进的是**新建会话**的 manager
  （`desktop/agent_driver.go:229`），不能说"实时生效"，只能写"对新建会话生效"。

---

## 附：v1.0 → v2.0 的修正清单

| 项 | v1.0 | v2.0 |
|---|---|---|
| `yaml.Node` 保真承诺 | 「保留注释与样式」 | 注释 ✅ / 缩进与序列风格 ❌，附实测表 |
| P0 验收 | 「保住注释的读写回环」 | 四条（含缩进断言与失败路径的字节级不变） |
| 原子性顺序 | 写盘 → 应用内存 | 应用内存（可回滚）→ 序列化 → 原子 rename |
| 写盘方式 | 未提 | `fileutil.AtomicWriteFilePrivate` |
| setter 数量 | 27 个 | 25 个 |
| `session_cleanup_max_count` | 「天然实时」 | 建新会话时读（`Manager.New` 内建清理） |
| `max_iterations` | 列为可热改 | desktop 走「不适用」（只有 `-p` 读它） |
| `config.Save` | 「全库 0 处调用」 | 成立，且是死代码 |
| 行号 | `App.tsx:1355` / `agent_driver.go:151` / `config.go:1055,1218` / `agent_mcp.go:66` | `1856` / `129` / `1064,1227` / `134` |
| 实现方案 | 「`yaml.Node` 读-改-写」 | **`goccy/go-yaml` 的 `Path` API**（spike 实测：v3 改 1 值动 70 行，goccy 动 3 行） |
| 依赖下限 | 未提 | **`goccy/go-yaml >= v1.19`**：v1.18.0 会在多注释文件上静默丢行尾注释 |
| 内存并发 | 未提 | §六「内存侧的并发」：`cfgMu` + `config()`/`withConfig()`，**永不换指针**（12 个闭包捕获 + 26 个读点） |

---

## 附：v2.0 → v2.1 的修正清单

反查方式：把 §五 的每一行拿回代码里找它的消费点（构造期快照 / 惰性读取 / 有几处持有），
不采信"应该可以"。下表每一格都能在文件里指到行。

| 项 | v2.0 | v2.1 |
|---|---|---|
| `system_reminder.git_reminder` | 🔴「需重建 `ReminderCollector`」 | 🔥：惰性开关 + 无条件注册。**重建反而有害**（会丢 `DeferredToolReminder` / thread / skill 的运行态），§五之三 1 |
| `logs.level` | 🔴（与 `max_size`/`max_files` 同列） | 🔥：`*slog.LevelVar` 一处改动，所有**已建** logger 跟着变；只有 `max_size`/`max_files` 留 🔴，§五之三 2 |
| `tool_result.*` | 🟡（与 `web_search` 合一条，只说重建工具） | 🟡：它还喂**共享 MCP manager**（构造期快照）——漏了这条就只有一半工具有效 |
| `memory.type` | 🟡「重建 `MemoryState` + 重注册工具」 | 🟡：还要换掉 `MemoryRecallReminder` 捕获的 `Backend`；`decay_half_life_days` 无 setter，另立一行 |
| `mcp_tool_search.*` | 🟡「只在 `PopulateFromConnect` 读一次」 | 🟡 偏 🔴：要重连所有 server + 重算 autoLoad + unregister 差集，`autoLoadNames` 无 reset。另 `max_deferred_schema_bytes` 是**死配置**（0 读点） |
| `lsp.*` | 「换 reminder collector」 | `Shutdown` **必须先调**（否则孤儿 LSP 进程）；reminder 换的是 provider，collector 不动；`enabled` false→true 还要**新增**注册 |
| `hooks.*` / `subagent.*` | 「重建即可」 | 各补一个坑：禁用 hooks 要显式置 nil；executor 重建要带上 worktree 与并发信号量 |
| `mcp_tool_refresh.interval` | 「`StopRefresher()` + `StartRefresher()`」 | 补：shared manager 的回调会绑到**触发它的那个 agent**，要从 app 层驱动 |
| 「包一层导出即可」那几行 | 5 个 `Setup*Provider` + `SetTitleGenEnabled` 被写成现成路径 | desktop 侧**零调用点**（已 grep）——实际是新增 fan-out，即 §八之三 ③ |
| setter 数量 | 25 个 | **26** 个（`grep -c '^func (a \*AIAgent) \(Set\|Setup\)'`） |
| MCP server 的 enable | 未提 | 它不在 config.yaml（`MCPServers` 是 `yaml:"-"`），只走 mcp.json → 写进 §九 不做 |
| 开工前的活 | §八之二 三条小项 | §八之二 八条小改 + §八之三 三条结构前置（应用表 / 键覆盖守卫 / 广播原语） |
