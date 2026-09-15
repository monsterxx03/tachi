# Desktop 设置页 — 设计

> 版本: 1.0 | 日期: 2026-09-14 | 状态: 设计阶段（未实现）
> 关联: [config hot-reload（TUI，未实现）](./2026-05-16-config-hot-reload.md)、
>       [desktop one-off panel](./2026-09-12-desktop-oneoff-panel-design.md)

## 一、问题

`desktop/frontend/src/App.tsx:1355` 的侧栏底部有一个**禁用**的设置入口：

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
  `/settings/billing`，注册在它自己的 settings router 里（证据见
  [issue #91208](https://github.com/anthropics/claude-code/issues/91208) 的 bundle 取证）。
  该 issue 的 bug 形态恰好证明这一点：路由错乱时 `/settings/usage` 把 **Code 面板的整个内容区
  替换掉了**——只有整页才可能这样。
- **UX 决策树**（Smashing Magazine 2026-03，Ryan Neufeld 框架）的判据：
  是否需要保留底层屏的上下文（否）→ 任务是否长/多步（是，30+ section）→ 是否需要来回参照
  底层（否）→ **"For more complex flows and multi-step processes, standalone pages work best."**
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

## 五、配置分层（已核实，不是推测）

热改的基础设施**已经存在一半**：`AIAgent` 上已有 27 个 setter，且
`SetMCPServerEnabled`（`desktop/agent_mcp.go:66`）就是「改内存配置 + 立即生效」的先例
（但它**只改内存不存盘**——设置页正好补上缺的那半）。

### 🔥 可热改

| 配置 | 生效机制 |
|---|---|
| `provider` / `providers` | `SetResolvedProvider` |
| 思考等级 | `SetThinking` |
| `title_generation` | `SetTitleGenEnabled` |
| `compact` | `SetCompactStrategy` |
| `permissions` | `SetPermissionPolicy` |
| `language` / `extra_system_prompt` | **清 `promptCache`** 即生效（见下） |
| `max_iterations` | 下一轮读取 |
| `session_cleanup_max_count` | 清理时读取，天然实时 |

`language` / `extra_system_prompt` 的性质值得记下：它们在 `systemPromptFor` 里
**每次现读 `d.cfg`**（`desktop/agent_driver.go:151`），只是结果被 `promptCache` 缓存
（key = cwd + id + roots + mode）。所以改完只需清空缓存，**不需要重建 agent**。

### ❄️ 需重启

| 配置 | 原因（已核实） |
|---|---|
| `logs` | `logger.Init` 的注释写明 "Must be called once"（`pkg/logger/logger.go:27`），全局单例 |
| `debug.pprof` | `startPprof` 启动即 `ListenAndServe`（`agent/bootstrap.go:48`），端口已绑 |
| `lsp.servers` | bootstrap 时拉起子进程 |
| `mcp` 新增/删除 server | 现只支持 enable/disable；加载走 `LoadMCPConfig` |
| `web.addr` | `tachi web` 是独立进程，端口已绑 |
| `channel` / `cron` / `dream` | channel 模式的后台循环；desktop 本不启用 |

> ⚠️ **不要照搬 `2026-05-16-config-hot-reload.md` 的分层**。那份文档是 TUI 场景的设计
> （`/config reload`、Hot/Warm/Cold），**从未实现**（`agent/config_reload.go` 不存在，
> `ReloadConfig` 全库无引用）。它的 Hot 层把 `provider` 归为 Warm，而 desktop 已有
> `SwitchProvider` 做运行时切换，所以这里的分类不同。可借鉴其**思路**，不可当既有实现。

## 六、核心挑战：保存不能毁掉用户手写的配置

**这是本功能最大的技术风险点。**

现状：

- `config.Save(cfg)`（`config/config.go:1055`）是 `yaml.Marshal` **全量覆盖**，
- 且**全库 0 处调用**——设置页将是它的第一个使用者。

用户的 `~/.tachi/config.yaml`（63 行）含注释与手工格式：

```yaml
  servers:
  - name: gopls
    args: []
    extensions: [".go"]     # 紧凑写法，Marshal 后会展开
```

全量 `Marshal` 一次就会丢掉全部注释与这些写法，**不可逆**。

### 方案：`yaml.Node` 读-改-写

```go
// config/config.go（新增）

// SavePartial 只更新指定的叶子路径，其余内容（注释、顺序、格式）原样保留。
// updates 的 key 是点分路径（"lsp.enabled"、"providers.0.model"）；
// 切片索引用数字段。
func SavePartial(path string, updates map[string]any) error
```

实现用 `yaml.Node` 定位目标节点并**只替换该节点的值**，序列化时 `yaml.v3` 会保留
未触碰节点的注释（`HeadComment` / `LineComment` / `FootComment`）与样式。

需要处理的边界：

- 路径不存在 → 追加（或返回错误，取决于是否需要「新增」语义）
- 值为 nil / 删除 → 明确不支持（第一版）
- 切片元素定位（`providers.0.model`）→ 索引越界要报错
- 写盘用 `fileutil.WriteFilePrivate`（0600），与 `Save` 一致
- **保存前备份**一份 `config.yaml.bak`，且只在验证通过后才替换

### 原子性

顺序必须是：

```
1. 校验 updates（类型、取值域）
2. SavePartial 到临时文件 → 成功
3. 应用变更到内存（d.cfg + 各 setter）
4. 原子 rename 临时文件 → config.yaml
```

第 3 步失败 → 放弃写盘，内存回滚到快照。第 4 步失败 → 内存已改是**可接受**的
（下次启动会读回旧值），但必须报错，不能静默。

## 七、实现结构

`App.tsx` 已有 1652 行，设置页**不写进去**。只加一个 view 状态：

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

参照 `oneoff.tsx`（762 行）的体量，这个规模可接受。

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

### 各入口的一致性

`AgentService` 的改动是「四个面」（见 `docs/agents/desktop.md`）：Go 方法、重新生成的
TS binding、前端、smoke driver。**不要**手改 binding，跑：

```sh
cd desktop && GOWORK=off wails3 generate bindings -clean=true -ts -i
```

## 八、分期

| 期 | 内容 | 风险 |
|---|---|---|
| **P0** | `config.SavePartial` + 单测（保住注释的读写回环） | ⚠️ **最高，先做先验证** |
| P1 | `App.tsx` view 状态 + `settings.tsx` 外壳 + 分类导航 + CSS 隐藏保状态 | 低 |
| P2 | 后端 `GetSettingsSchema` / `SaveSettings` + A 类表单 + 热改 | 中 |
| P3 | B 类表单 + `⟳` 徽标 + 保存后汇总提示 | 低 |
| P4 | desktop smoke 场景（改一项 → 断言 config.yaml 的值 + UI 反馈） | 低 |

P0 值得**单独先做原型**：它决定了「保存会不会毁掉用户手写的配置文件」这个不可逆后果。

## 九、不做的事

| 不做 | 原因 |
|---|---|
| ❌ 新增/删除 provider、MCP server | 需要表单的增删与排序，第一版只改已有项的值 |
| ❌ YAML 的原始文本编辑 | 那是编辑器的事；设置页只做结构化字段 |
| ❌ 密钥脱敏 | 已定：直接回显 |
| ❌ 文件监听 / 自动 reload | 用户显式保存；监听引入竞态 |
| ❌ 权限规则的图形化编辑 | `permissions.bash` 是规则列表，结构复杂，留到后续 |

## 十、待确认

- 保存后是否提供「立即重启」（`app.Restart()` 或退出重开）？第一版倾向只提示不提供。
- `permissions` 有项目级覆盖（`config.LoadProjectPermissions(projectRoot)`，
  `config.go:1218`）。设置页改的是**全局**那份，UI 上要说明。
