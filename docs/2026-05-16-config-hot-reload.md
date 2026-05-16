# Config Hot-Reload

> 版本: 1.0 | 日期: 2026-05-16 | 状态: 设计阶段
> 关联: [session 存储](./2026-05-10-session-replace-transcript.md),
>       [systemreminder 机制](../agent/systemreminder/reminder.go)

## 一、问题

当前 Tachi 的配置读取是一次性的：

```
main.go → config.Load() → AIAgent.Configure(cfg) → 启动
                                                      │
                                                      │ 改了 config.yaml？
                                                      ▼
                                                   只能重启
```

这意味著：

- **改模型**：退出 TUI → 改 YAML → 重启 → 重新加载 session → 等你输入
- **改 WebSearch key**：同上
- **开/关 MCP server**：同上
- **改提醒阈值**：同上
- **改语言**：同上

TUI 是一个常驻进程（可能跑几小时到几天），每次改配置都要重新启动整个进程，
既打断心流也浪费时间。

### 现有局部 reload 的参考

Skill 系统已经实现了一个局部热加载：

```go
// agent/agent.go
func (a *AIAgent) ReloadSkills() {
    a.unregisterSkillTools()
    a.initSkills()
    a.rebuildSkillCollector()
}
```

从 TUI 通过 `/skill reload` 触发。这说明**代码结构是支持动态重新初始化的**，
只是没有系统化的热加载入口。

---

## 二、设计目标

| 目标 | 说明 |
|------|------|
| 零停机 | 改 YAML 后不退出 TUI，配置实时生效 |
| 分层生效 | 不是全部配置都能热加载——区分 hot/cold/warm |
| 手动触发 | 不改文件监听——用 `/config reload` 显式触发 |
| 可回滚 | 加载失败不退进程，保持旧配置继续跑 |
| 增量更新 | 只重新初始化变化的部分，不重建整个 agent |

---

## 三、配置分层

不是所有配置都能无损热加载。按影响范围分三层：

### 🔥 Hot — 改完 /config reload 即生效

| 配置项 | 生效方式 |
|--------|---------|
| `language` | 重新 build system prompt，下一轮 LLM 调用生效 |
| `max_iterations` | 更新 `AIAgent.maxIterations` |
| `system_reminder.*` | 重建 `reminderCollector` |
| `web_search.key` | 重建 WebSearchTool 实例 |
| `tui.*` | TUI 重新读取（`input_history_limit` 等） |
| `title_generation` / `title_provider` | 重建 title 生成 provider |
| `commit_provider` | 重建 commit provider |

**原则**：这些配置项只影响 LLM 请求参数、工具配置、提醒策略，不涉及网络连接和进程状态。

### 🌡️ Warm — 改完需确认（可能中断当前对话）

| 配置项 | 生效方式 |
|--------|---------|
| `provider` | 切模型——重建 LLM provider，当前对话丢失 |
| `providers[].api_key` / `base_url` | 重建对应 provider 的连接 |
| `mcp_servers` / `active_mcp_profile` | 重连/断开 MCP server |
| `subagent.provider` / `subagent.model` | 重建子 agent 的 provider |

**原则**：涉及 API 连接和外部进程生命周期，现有上下文可能失效。

### ❄️ Cold — 必须重启

| 配置项 | 原因 |
|--------|------|
| `channel.weixin.*` | 微信通道基于长轮询，断开重连需要完整生命周期 |
| ``channel.channels`` | 同上的原因 |
| `cron.enabled` | cron scheduler 在 channel 模式下全局启动 |
| ``cron.store_path`` | 存储路径启动时固定 |

**原则**：涉及常驻后台 goroutine、网络监听、外部进程生命周期。

---

## 四、接口设计

### 4.1 `/config reload` 命令

```
/config reload              → 热加载所有 Hot 配置
/config reload --all        → 热加载 Hot + Warm（会有提示）
/config reload --warm       → 只重新连接 Warm 部分
```

TUI 中的行为：

```
输入: /config reload
        ↓
TUI: 阻止新用户输入
   ↓
Agent: config.Load() 重新读取 YAML
   ↓
Agent: diff 新旧配置
   ↓
Agent: 按 diff 类型执行不同热加载
   ↓
TUI: 通知栏显示 "✓ config reloaded (3 changes)"
   ↓
    恢复输入
```

### 4.2 `/config diff` 命令

```
/config diff     → 显示当前配置与磁盘上配置的差异
```

方便在 reload 前预览改了什么：

```
  language: "English" → "Chinese"
  web_search.key: "<old>" → "<new>"
  system_reminder.git_reminder: true → false
```

### 4.3 `/config` 命令（别名）

```
/config                  → 显示当前运行中的配置摘要
/config file             → 打印配置文件路径
```

---

## 五、核心实现

### 5.1 ReloadableConfig——配置快照对比

```go
// agent/config_reload.go

// ConfigSnapshot 保存一份配置的深层副本，用于 diff。
type ConfigSnapshot struct {
    Language          string
    MaxIterations     int
    SystemReminder    SystemReminderConfig
    WebSearch         WebSearchConfig
    TUI               TUIConfig
    Provider          string
    Providers         []ProviderConfig
    TitleGeneration   *bool
    TitleProvider     string
    CommitProvider    string
    MCPServers        []MCPServerConfig
    ActiveMCPProfile  string
    Subagent          SubagentConfig
}

// ConfigDiff 描述两份配置之间的差异。
type ConfigDiff struct {
    HasHotChanges   bool             // 至少有一个 hot 项变了
    HasWarmChanges  bool             // 至少有一个 warm 项变了
    HotChanges      []string         // 变化的 hot 项名称列表
    WarmChanges     []string         // 变化的 warm 项名称列表
}
```

### 5.2 AIAgent 新增方法

```go
// agent/agent.go

// ReloadConfig 重新读取 config.yaml，对比当前配置，按需执行热加载。
// 返回本次实际 reload 了哪些部分。
func (a *AIAgent) ReloadConfig(ctx context.Context) (*ConfigDiff, error) {
    oldSnapshot := a.takeConfigSnapshot()
    newCfg, err := config.Load()
    if err != nil {
        return nil, fmt.Errorf("reload config: %w", err)
    }
    diff := compareConfig(oldSnapshot, newCfg)

    if diff.HasHotChanges {
        a.applyHotChanges(ctx, newCfg)
    }

    // Warm 需要额外确认，调用方（TUI）先展示 diff 再决定是否执行
    return diff, nil
}

// applyHotChanges 执行所有 hot 级别的配置更新。
// 不涉及网络连接重建，不会中断当前对话。
func (a *AIAgent) applyHotChanges(ctx context.Context, cfg *config.Config) {
    // 1. language → system prompt 更新
    //    不用立即重建，标记为"下一轮对话使用"
    //    通过让系统提醒机制在下一次 user message 时使用新语言

    // 2. max_iterations → 更新字段
    a.maxIterations = cfg.GetMaxIterations()

    // 3. system_reminder → 重建 reminder collector
    a.rebuildReminders(cfg)

    // 4. web_search → 重建 tool（如果 key 变了）
    a.rebuildWebSearchTool(cfg)

    // 5. title/commit provider → 重建（如果变了）
    a.rebuildTitleProvider(cfg)
    a.rebuildCommitProvider(cfg)

    // 6. subagent → 更新配置（下一个 subagent 生效）
    a.rebuildSubagentConfig(cfg)
}

// reloadWarmConfig 执行 warm 级别的配置更新。
// 可能中断当前对话——调用前应确认用户同意。
func (a *AIAgent) reloadWarmConfig(ctx context.Context, cfg *config.Config) error {
    // 1. provider/模型切换 → 重建 provider 实例
    //    当前对话中的消息历史会丢失（不同 model 的 context window 不同）

    // 2. MCP server 变更 → 断开旧的，连接新的

    // 3. subagent provider 变更 → 重建 subagent provider
}
```

### 5.3 TUI 集成

```go
// tui/commands.go

func (m *Model) handleConfigReload(args string) {
    diff, err := m.agent.ReloadConfig(ctx)
    if err != nil {
        m.addMessage("❌ " + err.Error())
        return
    }

    if !diff.HasHotChanges && !diff.HasWarmChanges {
        m.addMessage("✓ 配置无变化")
        return
    }

    m.addMessage(formatDiff(diff))

    if diff.HasWarmChanges {
        // 如果只变更了 hot，直接应用
        // 如果涉及 warm，展示选项：
        //   "检测到 provider/MCP 配置变更，应用需要中断当前对话。"
        //   "输入 /config reload --warm 确认，或按 Esc 取消。"
    }
}

func (m *Model) handleConfig() {
    // /config 显示当前配置摘要
    // /config file 显示路径
    // /config diff 对比磁盘
    // /config reload 热加载
}
```

---

## 六、实现优先级

### Phase 1：Hot reload（核心价值最高）

```go
// 改动量：~150 行
// 文件：agent/config_reload.go（新增）+ tui/commands.go
```

支持的变更：

```
language, max_iterations, system_reminder.*,
web_search.*, tui.*, title_*, commit_*
```

用户场景：

```
你: /config diff
我:   language: "English" → "Chinese"
     web_search.key: "<old>" → "<new>"

你: /config reload
我: ✓ Config reloaded — 2 hot changes applied
   下一轮回复起使用新配置。
```

### Phase 2：Warm reload（需要用户确认）

```go
// 改动量：~100 行
```

支持的变更：

```
provider, providers[].api_key/base_url,
mcp_servers, active_mcp_profile,
subagent.provider/model
```

用户场景：

```
你: /config reload --all
我: ⚠ 检测到 provider 变更，当前会话将丢失。
   输入 /confirm 继续，或取消。
```

### Phase 3：Cold（不做）

原因：通道和 cron 涉及后台 goroutine 生命周期，
热加载引入的状态复杂度远超收益。改为启动时检测并输出提示：

```
tachi: config.yaml 已变更（cron.enabled），
       重启以使新配置生效。
```

---

## 七、安全边界

| 场景 | 行为 |
|------|------|
| YAML 语法错误 | 加载失败，旧的配置继续运行，报错信息展示给用户 |
| 新配置的 provider 不存在 | 回退到旧 provider，报错但不退出 |
| MCP server 重连失败 | 跳过该 server，其他 server 正常 |
| `/config reload` 时正在 LLM 调用 | 排队等待当前回合结束后执行 |

---

## 八、不做的事

| 不做 | 原因 |
|------|------|
| ❌ 文件监听（fsnotify） | TUI 场景下用户更习惯手动触发，文件监听增加复杂的竞态处理 |
| ❌ 自动 reload | 改一个字母就 reload 太敏感，用户应该显式确认 |
| ❌ YAML 编辑器的 TUI | 太复杂，`/config diff` + 外部编辑器就够了 |
| ❌ 配置版本回滚 | diff 里如果用户发现改错了，手动改回来再 /config reload 更简单 |
