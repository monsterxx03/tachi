# Desktop 设置页 — 设计

> 版本: 2.0 | 日期: 2026-09-17（v1.0: 2026-09-14）| 状态: 设计阶段（未实现）
>
> v2.0 是 v1.0 的**全面复核**：热改分层按实际消费点重做，P0 的验收标准收紧
> （`yaml.Node` 回环实测发现 v1.0 承诺的「样式保留」不成立），原子性顺序修正。
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

热改的基础设施**已经存在**：`AIAgent` 上已有 25 个 `Set*`/`Setup*` 方法，且
`desktop/agent_mcp.go:134` 的 `SetMCPServerEnabled` 就是「改内存配置 + 立即生效」的先例
（它同时把 enable 状态写回 mcp.json——设置页正好补上 config.yaml 那半）。

### 🔥 可热改

| 配置 | 生效机制 |
|---|---|
| `provider` / `providers` | `SetResolvedProvider`（同时重指 compact 策略） |
| `provider[].spec.thinking_level` | 随 `SetResolvedProvider` 一体切换 |
| `title_generation` | `SetTitleGenEnabled` |
| `title_provider` / `commit_provider` / `review.provider` / `run_provider` / `subagent.provider` | 各 `Setup*Provider`（已有现成路径，包一层导出即可） |
| `permissions.bash.*` | `SetPermissionPolicy`（**零重构**） |
| `compact.auto` / `compact.threshold` | `shouldAutoCompact()` **每轮**读 `FullConfig` |
| `compact.timeout` / `compact.max_tokens` | `doCompact` 现读 |
| `oneoff.enabled` / `retention_days` | `oneoffEnabled()` 现读 |
| `checkpoints.enabled` | `checkpointsEnabled()` 现读 |
| `session_cleanup_max_count` | 建新会话时读（`Manager.New` 内建 `cleanupLocked`） |
| `review.*` / `deep_research.*` | `ResolveReviewOptions(cfg)` / `NewDeepResearch(cfg)` 每次命令现读 |
| `active_mcp_profile` | `SwitchMCPProfile`（内存态，重启回退——既有先例） |
| `language` / `extra_system_prompt` | 清 `d.promptCache` 即可。`systemPromptFor` 是**每轮现读** `d.cfg` 的，cache key 只含 `(cwd,id,roots,mode)`，加一个清空入口就够，不必往 key 里加字段 |

### 🟡 小重构后可热（要做的是「重建 + 换入」）

| 配置 | 需要什么 |
|---|---|
| `web_search.*` / `web_fetch.*` / `tool_result.*` | 在 `RegisterTools` 里重建工具对象再 `Registry.Register`（同名覆盖）。`WebSearchTool` 的 `sync.Once` 缓存只随实例走，换实例即生效 |
| `checkpoints.max_files` / `max_bytes` / `retain` | ⚠️ `ckptKey` 只含 `sess.ID + roots`，**不含 Options**——必须把 Options 并进 key，否则改了不重建 |
| `mcp_tool_search.*` | 只在 `PopulateFromConnect` 读一次 |
| `mcp_tool_refresh.interval` | `StopRefresher()` + `StartRefresher()` |
| `lsp.*`（含 `servers`） | `LSPManager` **没有 Restart API**，只有 `Shutdown` → 新建 manager + 重注册 2 个工具 + 换 reminder collector |
| `hooks.*` | `HookDispatcher` 构造期一次性构建，重建即可 |
| `subagent.*` | `subagent.NewExecutor` + 重注册 `SubagentTool` |
| `memory.type` / `decay_half_life_days` | `TopicBackend` 构造期吃一半；换 backend 要重建 `MemoryState` + 重注册 memory 工具 |
| `memory.keyword_provider` | `resolveKeywordProvider(cfg)` 已是幂等独立函数，导出即可 |

### 🔴 必须重启

| 配置 | 原因（已核实） |
|---|---|
| `logs.*` | `logger.Init`（`pkg/logger/logger.go:32`）写全局量；且 `textHandler.minLevel` 是**每个 logger 构造时快照**的，改全局对已建实例无效 |
| `debug.pprof.*` | `startPprof` 启动即绑端口（`agent/bootstrap.go:48`） |
| `web.*` | `tachi web` 是独立进程，端口已绑 |
| `channel.*` / `cron.*` / `dream.*` | channel 模式的后台循环；desktop 本不启用。各 channel 构造期就固化了 token/proxy 等 |
| `herdr.enabled` | 由 `HERDR_ENV` 环境变量推导，启动期判定 |
| `system_reminder.git_reminder` | 需重建 `ReminderCollector` |
| `tui.*` | TUI 专属，widget 构造期消费 |
| `provider_aliases` | `ExpandProviderAliases` 只在 `LoadFrom` 末尾跑，且别名已被展开成真名 |

### 两个边界情况

- **`max_iterations` 对 desktop 是「不适用」而非「需重启」**：desktop 走
  `NewAIAgentWithConfig` 不传 `MaxIterations` → 预算 0 = 无限（交互式前端不该被 config 卡住）。
  只有 `tachi -p` 真正读它（`main_agent.go:39,139`）。设置页要么不显示，要么标注「仅命令行模式」。
- **`max_tokens` 分层不同**：desktop 每轮现读 `d.cfg.MaxTokens`（`agent_turn.go:237`）→ 热；
  TUI 启动时拷进 `ChatOpts`（`main.go:283`）→ 冷。同一个配置项在不同入口分层不同。

## 六、核心挑战：保存不能毁掉用户手写的配置

现状：

- `config.Save(cfg)`（`config/config.go:1064`）是 `yaml.Marshal` **全量覆盖**，
- 且**全库 0 处调用**（连测试都自己 `yaml.Marshal`）——设置页将是它的第一个使用者。

用户的 `~/.tachi/config.yaml` 含注释、引号与手工格式。全量 `Marshal` 一次就会丢掉全部注释，
**不可逆**。

### 方案：`yaml.Node` 读-改-写（已实测）

实测结论（用真实的 63 行 config.yaml，改 1 个叶子值）：

| 现象 | 结果 |
|---|---|
| 注释（`HeadComment`/`LineComment`/`FootComment`） | ✅ **保住** |
| 流式写法 `extensions: [".go"]` | ✅ **保住**（没被展开） |
| 引号 `command: "gopls"` | ✅ 保住 |
| **缩进** | ❌ **全文件被重排**：2 空格 → 4 空格 |
| **序列缩进风格** | ❌ `servers:\n- name:` → `servers:\n    - name:` |
| 段落空行 | ❌ 被压掉 |

**v1.0 说「保留未触碰节点的注释与样式」——注释对，样式错。** 两条根因，都不是「小心一点」能绕过的：

1. **缩进是 encoder 的全局设置，不是节点属性。** `yaml.Node` 不记录缩进，默认
   `best_indent = 4`；`yaml.NewEncoder` 的 `SetIndent(2)` 能修好标量/映射缩进，但**修不了序列**——
   序列缩进是「父级缩进 + `best_indent`」硬编码，yaml.v3 没有 indentless-sequence 开关。
2. **必须「就地改 `node.Value`」，不能替换 value 节点。** 替换会让挂在节点上的
   `LineComment` 一起消失（实测）：
   ```go
   // ❌ 替换 value 节点 → 行尾注释被丢掉
   cur.Content[j+1] = &yaml.Node{Kind: yaml.ScalarNode, Value: val}
   // ✅ 就地改值 → 节点（连同注释/样式）留在原地
   next.Value = val
   ```
3. 序列索引要单独分支：节点 `Kind == SequenceNode` 时没有 key，得按数字下标走。

### P0 验收标准（v1.0 的太松）

v1.0 只要求「保住注释的读写回环」——实测表明那条太容易通过，而缩进已经被毁。改成：

1. 改一个叶子值后，**除目标行外无内容差异**（注释、引号、流式写法）
2. **缩进差异被显式断言**：`SetIndent(2)` + 序列加宽「接受」并写进断言，或换用保真度更高的库
   （如 `goccy/go-yaml`，它对 `Node` 有序列缩进选项）——**这值得先做一次 spike 再定**
3. 空行/段落被压掉，写成已知限制或纳入断言
4. **失败路径**：校验失败时文件**字节级不变**

### 原子性

顺序必须是（v1.0 把前两步写反了，会导致「文件已写、内存回滚」的矛盾状态）：

```
1. 校验 updates（类型、取值域）
2. 应用变更到内存（d.cfg + 各 setter）—— 失败即回滚，此时文件未动
3. 序列化到临时文件 → 失败即放弃，内存回滚到快照
4. 原子 rename 临时文件 → config.yaml
```

第 4 步失败：内存已改是**可接受**的（下次启动会读回旧值），但必须报错，不能静默。

**写盘用 `fileutil.AtomicWriteFilePrivate`，不要沿用 `Save` 的非原子路径。**
理由与 session 的 meta.json 相同：config.yaml 有并发读者（另一个 Tachi 进程启动时
`config.Load`），而 `WriteFilePrivate` 是就地重写，读者可能读到截断文件。

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

后端**声明**可配置项（而不是前端硬编码），每条带元信息：

```go
type SettingField struct {
    Key          string   // 点分路径，如 "language"
    Label        string
    Kind         string   // "string" | "int" | "bool" | "enum" | "string_list"
    Value        any      // 当前值
    Options      []string // enum 用
    NeedsRestart bool     // ← 由后端保证，不靠人记
    Help         string
}
```

「要不要重启」由后端**代码保证**——新增一个配置项时，忘了标 `NeedsRestart` 会是最典型的
错误，所以这个字段必须和热改的 `apply` 函数放在一起。

⚠️ **多会话注意**：`d.cfg` 是全进程唯一的一份，而 `AIAgent` 是**每会话**一个。
热改一个配置要遍历 `liveAgents()`（先例见 `desktop/agent_mcp.go` 的 MCP toggle），
不能只改当前显示的会话。

### 各入口的一致性

`AgentService` 的改动是「四个面」（见 `docs/agents/desktop.md`）：Go 方法、重新生成的
TS binding、前端、smoke driver。**不要**手改 binding，跑：

```sh
cd desktop && GOWORK=off wails3 generate bindings -clean=true -ts -i
```

## 八、分期

| 期 | 内容 | 风险 |
|---|---|---|
| **P0** | `config.SavePartial` + 单测（保住注释的读写回环 + §六的 P0 验收四条） | ⚠️ **最高，先做先验证** |
| P1 | `App.tsx` view 状态 + `settings.tsx` 外壳 + 分类导航 + CSS 隐藏保状态 + 屏蔽对话快捷键 | 低 |
| P2 | 后端 `GetSettingsSchema` / `SaveSettings` + 🔥 类表单 + 热改（含 `liveAgents()` 广播） | 中 |
| P3 | 🟡 / 🔴 类表单 + `⟳` 徽标 + 保存后汇总提示 | 低 |
| P4 | desktop smoke 场景（改一项 → 断言 config.yaml 的值 + UI 反馈） | 低 |

P0 值得**单独先做原型**：它决定了「保存会不会毁掉用户手写的配置文件」这个不可逆后果。

## 九、不做的事

| 不做 | 原因 |
|---|---|
| ❌ 新增/删除/重排 provider、MCP server | 表单复杂度之外，还有一条硬理由：`provider_aliases` 的展开与各 `Setup*Provider` 的重解析，漏一处就静默沿用旧 provider |
| ❌ YAML 的原始文本编辑 | 那是编辑器的事；设置页只做结构化字段 |
| ❌ 密钥脱敏 | 已定：直接回显 |
| ❌ 文件监听 / 自动 reload | 用户显式保存；监听引入竞态 |
| ❌ 权限规则的图形化编辑 | `permissions.bash` 是规则列表，结构复杂，留到后续 |

## 十、待确认

- 保存后是否提供「立即重启」（`app.Restart()` 或退出重开）？第一版倾向只提示不提供。
- `permissions` 有项目级覆盖（`config.LoadProjectPermissions`，`config/config.go:1227`）。
  设置页改的是**全局**那份，UI 上要说明。
- **运行时切换的持久化策略不一致**：`/model` 的 provider 切换**会**持久化到会话 meta，
  而 `SwitchMCPProfile` 明确是「in-memory only, reverts on restart」。两条同类操作行为不同，
  第一版不统一，但要在 UI 上分别说清哪条会留下、哪条不会。

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
