# Skill 机制设计文档

> 版本: 1.0 | 日期: 2026-05-11 | 状态: 设计阶段

## 一、概述

Skill 是 Tachi 的可复用能力模块——每个 Skill 是一个包含 `SKILL.md` 的目录，声明一段专用的指令、工作流和上下文。Agent 按需加载 Skill，将其指令注入当前对话，从而获得特定领域的能力增强。

### 1.1 动机

当前 Tachi 的能力扩展主要依赖三种机制：

| 机制 | 粒度 | 局限 |
|------|------|------|
| System Reminders | 全局/会话级提示片段 | 太碎片化，无结构，无法发现和复用 |
| SubAgent | 隔离的一次性子代理 | 太重，创建/销毁开销大，无法跨会话复用 |
| MCP Tools | 外部工具接入 | 纯工具，缺操作指令和工作流指导 |

Skill 填补的是 **中粒度、可声明、可发现、可复用的横向能力单元** 这个空白。

### 1.2 设计目标

| 目标 | 说明 |
|------|------|
| 声明式定义 | 一个目录 + 一个 SKILL.md = 一个 Skill |
| 按需加载 | 不常驻 system prompt，触发时才注入完整指令 |
| 双重激活 | 用户显式 `/skill-name` + LLM 自主路由 |
| 缓存友好 | 注入为用户消息而非 system prompt，保护 prompt caching |
| 渐进式披露 | 元数据常驻（低 token 成本），完整内容按需加载 |
| 项目级共享 | `.tachi/skills/` 跟随仓库，团队协作复用 |

---

## 二、核心设计

### 2.1 Skill 文件格式

```
<skill-name>/
├── SKILL.md           # 必需：YAML frontmatter + Markdown 指令体
├── references/        # 可选：参考文档
├── templates/         # 可选：模板文件
└── scripts/           # 可选：可执行脚本
```

#### SKILL.md 完整格式

```markdown
---
name: code-review
description: Review code changes for bugs, security issues, and code style
tags: [review, security, quality]
requires_tools: [ReadFile, Grep, Glob, Bash]
---

# Code Review Skill

When the user requests a code review:

1. Identify changed files with `git diff --name-only`
2. For each file, read the full content
3. Check for: hardcoded secrets, injection risks, nil checks, error handling, naming
4. Output: 🔴 Critical / 🟡 Warning / 🟢 Suggestion

## Supporting references
- See `references/checklist.md` for the complete review checklist
```

#### Frontmatter 字段

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `name` | string | ✅ | 唯一标识符，小写字母+数字+连字符，≤64 字符 |
| `description` | string | ✅ | 一句话描述，≤1024 字符。LLM 用它决定是否激活此 Skill |
| `tags` | []string | 否 | 标签，用于分类和搜索 |
| `requires_tools` | []string | 否 | 声明依赖的工具名，文档性质（不强制门控） |
| `version` | string | 否 | 语义化版本号 |
| `author` | string | 否 | 作者信息 |

### 2.2 存放位置与优先级

| 优先级 | 位置 | 作用域 |
|--------|------|--------|
| 1 (最高) | `<workspace>/.tachi/skills/` | 项目级，跟随 git 仓库 |
| 2 | `~/.tachi/skills/` | 全局个人 Skill |

同名 Skill 按优先级覆盖。项目级优先于全局级，允许项目定制全局 Skill 的行为。

### 2.3 激活方式（双重路由）

**方式一：用户显式 `/skill-name`**

```
用户输入: /code-review main.go
       ↓
slash command 解析 → 查找 Skill → buildSkillMessage()
       ↓
构造用户消息注入 conversation
       ↓
LLM 在下一轮 API 调用中看到完整 Skill 指令
```

**方式二：LLM 自主路由**

```
所有 Skill 的 {name, description, tags} 压缩列表
注入 system prompt（约 100 token / 10 个 Skill）
       ↓
LLM 判断任务需要某个 Skill
       ↓
LLM 调用 skill_view(name) 工具
       ↓
完整 Skill 内容注入后续对话
```

两种方式互补：显式命令确定性强、即时生效；LLM 路由覆盖"帮我审查下这段代码"这类自然语言触发。

### 2.4 为什么注入为用户消息

参考 Hermes-Agent 的设计：Skill 指令作为 **用户消息**（`role: "user"`）注入，而非 system prompt。

**理由**：

1. **Prompt caching**：修改 system prompt 会导致 Anthropic/OpenAI 的 prompt cache 全部失效。用户消息在缓存断点之后，不影响已有缓存。
2. **作用域清晰**：Skill 是"用户要求 Agent 遵循的指令"，语义上接近 user message
3. **多条 Skill 不冲突**：可以依次注入多个 Skill 作为多条 user message

```
消息序列：
  [system]  Tachi system prompt (cached ✓)
  [user]    "帮我审查最近的改动"        (cached ✓)
  [assistant] tool_calls...              (cached ✓)
  [user]    [SKILL: code-review]          ← Skill 注入，不影响上方缓存
           # Code Review Skill
           When the user requests a code review...
  [assistant] 好的，我来审查最近的改动...
```

---

## 三、与现有系统的关系

### 3.1 与 SubAgent 的对比

| 维度 | Skill | SubAgent |
|------|-------|----------|
| 本质 | 指令注入（扩展现有 agent 的能力） | 隔离执行（创建新 agent 实例） |
| 上下文 | 共享主 agent 对话 | 独立子对话 |
| 持久化 | Skill 文件跨会话复用 | 一次性，执行完销毁 |
| 适用场景 | 操作规范、审查清单、工作流指导 | 复杂多步骤任务、并行探索 |
| 工具访问 | 主 agent 的全部工具 | 可配置白名单 |
| 开销 | ~100 token (description) + 按需加载 | 独立 API round-trip + 迭代预算 |

两者互补，不是替代关系。Skill 解决的是"Agent 不知道该怎么做"，SubAgent 解决的是"Agent 自己搞不定，需要帮手"。

### 3.2 与 System Reminders 的对比

| 维度 | Skill | System Reminder |
|------|-------|-----------------|
| 激活方式 | 用户命令 / LLM 路由 | 自动注入（日期、git 状态等） |
| 内容 | 用户/团队编写的领域指令 | 系统自动生成的提示 |
| 持久化 | 文件系统，git 版本化 | 代码内定义 |
| 可发现性 | `/skill list` + LLM description 列表 | 无 |

Skill 激活后可作为 `<system-reminder type="skill">` 块的形式注入，复用现有 reminder 管道。

### 3.3 与 MCP Tools 的关系

Skill 可以通过 `requires_tools` 声明依赖 MCP 工具，形成 **"外部工具 + 操作指令"** 的完整能力包：

```yaml
name: github-pr-review
description: Review PRs with GitHub context
requires_tools: [mcp__github__list_prs, mcp__github__get_pr_diff, ReadFile, Grep]
```

这比单纯的 MCP tool 提供了更高层次的操作指导。

---

## 四、模块设计

### 4.1 SkillStore — `agent/skill/store.go`（新文件）

负责扫描、加载、索引所有 Skill：

```go
package skill

// Store manages skill discovery, loading, and caching.
type Store struct {
    dirs []string // 搜索目录列表，优先级从高到低
}

// NewStore creates a Store scanning both project-level and global skill dirs.
func NewStore(projectRoot string) *Store

// List returns {name, description, tags, source} for all discovered skills.
func (s *Store) List() []SkillMeta

// Load reads and parses a skill's SKILL.md, returns full content + metadata.
func (s *Store) Load(name string) (*Skill, error)

// ResolveCommand maps "/skill-name" to the canonical skill name.
func (s *Store) ResolveCommand(cmd string) (string, bool)
```

#### 扫描逻辑

```
List():
  for each searchDir (优先级从高到低):
    for each SKILL.md in searchDir (递归):
      parse frontmatter → {name, description, tags}
      if name 已存在 → 跳过（高优先级已覆盖）
      加入结果
  return 结果列表
```

**注意**：只扫描第一层子目录中的 `SKILL.md`（`skills/<name>/SKILL.md`），不递归深层目录，避免复杂性和性能问题。

#### 缓存策略

`List()` 返回轻量元数据（name + description + tags），启动时扫描一次，之后文件变更通过 watcher 或手动 `/skill reload` 刷新。`Load()` 按需读取完整文件，不做预加载。

### 4.2 SkillMeta 与 Skill 类型

```go
// SkillMeta is the lightweight metadata returned by List().
// Used for LLM routing and /skill list display.
type SkillMeta struct {
    Name        string   `json:"name"`
    Description string   `json:"description"`
    Tags        []string `json:"tags"`
    Source      string   `json:"source"` // "project" | "global"
}

// Skill is the full parsed skill, returned by Load().
type Skill struct {
    Meta     SkillMeta       `json:"meta"`
    Body     string          `json:"body"`      // SKILL.md body (minus frontmatter)
    RawContent string        `json:"raw_content"` // complete SKILL.md text
    Dir      string          `json:"dir"`       // absolute path to skill directory
    Files    map[string]string `json:"files"`   // supporting files (references/*.md etc.)
}
```

### 4.3 skill_view 工具 — `agent/tools/skill_view.go`（新文件）

Agent 用来按需加载 Skill 内容的工具，遵循渐进式披露模式：

```go
// SkillViewTool allows the agent to load skill content on demand.
type SkillViewTool struct {
    store *skill.Store
}

func NewSkillViewTool(store *skill.Store) *SkillViewTool
```

**Schema**:

| 属性 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `Name()` | — | — | `"skill_view"` |
| `Description()` | — | — | "Load a skill's full instructions. Use skills_list first to see available skills." |
| `Parallel()` | — | — | `false` |
| `name` | string | yes | Skill 名称 |
| `file_path` | string | no | Skill 内子文件路径，如 `references/checklist.md` |

**行为**：

1. 首次调用（无 `file_path`）：返回 `SKILL.md` 完整内容 + 可用子文件列表
2. 后续调用（带 `file_path`）：返回指定子文件内容
3. 调用 `bump_use` / `bump_view` 记录使用统计（为未来 Curator 预留）

### 4.4 skills_list 工具 — `agent/tools/skills_list.go`（新文件）

```go
// SkillsListTool lists all available skills with metadata.
type SkillsListTool struct {
    store *skill.Store
}
```

**Schema**:

| 属性 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `Name()` | — | — | `"skills_list"` |
| `Description()` | — | — | "List available skills with name and description. Use skill_view(name) to load full content." |
| `Parallel()` | — | — | `false` |
| `tag` | string | no | 按标签过滤 |

**返回格式**：

```json
{
  "success": true,
  "skills": [
    {"name": "code-review", "description": "Review code for bugs...", "tags": ["review"], "source": "project"},
    {"name": "git-commit", "description": "Generate conventional commit...", "tags": ["git"], "source": "global"}
  ],
  "count": 2
}
```

### 4.5 Skill 消息构造 — `agent/skill/message.go`（新文件）

```go
// BuildActivationMessage constructs the user message injected when a skill
// is activated (via slash command or LLM routing).
//
// Format:
//
//   [IMPORTANT: The user has activated the "code-review" skill.
//    Follow its instructions below.]
//
//   # Code Review Skill
//   When the user requests a code review...
//
//   [Skill directory: /Users/.../skills/code-review]
//   [Supporting files: references/checklist.md → ...]
//   Load with skill_view(name="code-review", file_path="references/checklist.md")
func BuildActivationMessage(sk *Skill, userInstruction string) string

// BuildSkillListPrompt constructs the compact skill catalog injected into
// the system prompt for LLM-based routing.
//
// Format (XML-like, ~100 token / 10 skills):
//
//   <available_skills>
//     <skill name="code-review" description="Review code for bugs and security" tags="review,security"/>
//     <skill name="git-commit" description="Generate conventional commit messages" tags="git"/>
//   </available_skills>
//
//   To use a skill, call skill_view(name) or the user can type /skill-name.
func BuildSkillListPrompt(metas []SkillMeta) string
```

### 4.6 斜杠命令 — `tui/commands_skill.go`（修改）

复用现有斜杠命令注册机制（`commands.go` 中的 `CommandDef`），新增：

| 命令 | 说明 |
|------|------|
| `/skill` | 列出可用 Skill |
| `/skill <name>` | 激活指定 Skill |
| `/skill reload` | 重新扫描 Skill 目录 |

**实现方式**：不在 `COMMAND_REGISTRY` 中静态注册每个 Skill，而是动态拦截。当用户输入 `/xxx` 不在已知命令中时，查询 `skill.Store.ResolveCommand("xxx")`，匹配成功则走 Skill 激活流程。

### 4.7 AIAgent 集成 — `agent/agent.go`（修改）

```go
type AIAgent struct {
    // ... 现有字段 ...
    skillStore *skill.Store
    activeSkills map[string]bool // 当前会话已激活的 skill 名称
}

// SetSkillStore sets the skill store for this agent.
func (a *AIAgent) SetSkillStore(store *skill.Store)

// ActivateSkill injects a skill's instruction into the current conversation.
func (a *AIAgent) ActivateSkill(name string, userInstruction string) error
```

**Configure() 修改**：

```go
func (a *AIAgent) Configure(ctx context.Context, cfg *config.Config) (*mcp.Manager, error) {
    // ... 现有初始化 ...

    // 初始化 Skill Store
    projectRoot, _ := os.Getwd()
    a.skillStore = skill.NewStore(projectRoot)

    // 注册 skill 工具
    a.RegisterTool(tools.NewSkillsListTool(a.skillStore))
    a.RegisterTool(tools.NewSkillViewTool(a.skillStore))

    // ... 其余现有逻辑 ...
}
```

### 4.8 System Prompt 扩展 — `agent/systemreminder/`

`BuildSkillListPrompt()` 的输出在 `Collector.Collect()` 中作为一条 reminder 注入，放在 project context 之后、iteration warning 之前。

**注意**：只有当 `len(metas) > 0` 时才注入，避免额外 token 消耗。

---

## 五、执行流程

### 5.1 用户斜杠命令流程

```
1. TUI 收到 "/code-review main.go"
2. resolveCommand("/code-review"):
     → 查 COMMAND_REGISTRY → miss
     → 查 skillStore.ResolveCommand("code-review") → hit
     → 返回 skill name "code-review"
3. TUI 切换到 skill 激活路径:
     a. skillStore.Load("code-review") → *Skill
     b. BuildActivationMessage(skill, "main.go") → message string
     c. 注入为 user message（通过 sendMessage 的常规路径）
     d. activeSkills["code-review"] = true
4. LLM 下一轮 API 调用看到完整 Skill 指令 → 按指令执行
```

### 5.2 LLM 自主路由流程

```
1. system prompt 中包含 <available_skills> 列表
2. User: "帮我审查一下最近的改动"
3. LLM 看到 skill list，决定调用 skill_view("code-review")
4. skill_view 工具执行:
     → skillStore.Load("code-review")
     → 返回完整 SKILL.md 内容
     → activeSkills["code-review"] = true
5. LLM 将 Skill 指令应用于当前任务
6. 后续对话中无需再次加载（已在 activeSkills 中）
```

### 5.3 Skill 加载缓存

同一会话中，已激活的 Skill **不会重复注入**：

```
activeSkills 集合跟踪已加载 Skill
  ├── 斜杠命令 → 检查 activeSkills[skillName] → 已激活则跳过注入
  └── skill_view → 返回内容但不再注入 activation message
```

`/new` 命令清空 `activeSkills`。

---

## 六、文件变更清单

| 文件 | 变更 | 说明 |
|------|------|------|
| `agent/skill/store.go` | **新文件** | `Store` — Skill 扫描、索引、加载 |
| `agent/skill/message.go` | **新文件** | `BuildActivationMessage()` + `BuildSkillListPrompt()` |
| `agent/skill/types.go` | **新文件** | `SkillMeta`、`Skill` 类型定义 + 测试用 mock 接口 |
| `agent/tools/skills_list.go` | **新文件** | `SkillsListTool` — 列出可用 Skill |
| `agent/tools/skill_view.go` | **新文件** | `SkillViewTool` — 按需加载 Skill 内容 |
| `agent/agent.go` | 修改 | 新增 `skillStore`、`activeSkills` 字段 + `SetSkillStore()` + `ActivateSkill()` + `Configure()` 中注册 skill 工具 |
| `agent/systemreminder/collector.go` | 修改 | `Collect()` 中注入 `BuildSkillListPrompt()` 输出（如有 Skill） |
| `tui/model.go` | 修改 | 收到 Skill 激活结果后显示确认消息 |
| `tui/commands.go` | 修改 | 新增 `/skill` 斜杠命令（list / reload） |
| `tui/input.go` | 修改 | 未匹配已知 command 时 fallback 到 skillStore.ResolveCommand |
| `agent/skill/store_test.go` | **新文件** | Store 单元测试（扫描、同名覆盖、优先级） |
| `agent/tools/skills_list_test.go` | **新文件** | SkillsListTool 单元测试 |
| `agent/tools/skill_view_test.go` | **新文件** | SkillViewTool 单元测试 |

未变更文件：
- `config/config.go` — MVP 无需 Skill 配置项（目录路径硬编码，后续可加）
- `agent/subagent.go` — Skill 不改变 SubAgent 行为（Skill 激活状态不传递给子 agent）
- `llm/provider.go` — 无需新增消息角色（Skill 作为 user message 注入，与 Steer 不同）

---

## 七、测试策略

### 单元测试

| 测试对象 | 测试内容 |
|---------|---------|
| `Store.List()` | 扫描 results 正确、项目级覆盖全局级、空目录 |
| `Store.Load(name)` | 正确解析 frontmatter + body、不存在的 skill 报错 |
| `Store.ResolveCommand(cmd)` | 连字符匹配（`code-review` → `code-review`）、大小写不敏感 |
| `SkillsListTool` | Schema 正确、返回 JSON 格式正确 |
| `SkillViewTool` | 首次加载返回完整内容 + linked files、带 file_path 加载子文件 |
| `BuildSkillListPrompt()` | 空列表返回空字符串、XML 格式正确、转义特殊字符 |
| `BuildActivationMessage()` | 包含 activation note、skill body、supporting files 提示 |

### 集成测试

- 真实 `.tachi/skills/code-review/SKILL.md` → agent 通过 skills_list 可发现
- `/code-review main.go` → TUI 显示 `[user] [SKILL: code-review] ...` → LLM 按指令执行
- `/new` → activeSkills 清空 → 相同 Skill 可再次激活
- 项目级 Skill 覆盖全局同名 Skill → List() 返回项目级版本

---

## 八、边界情况

### 8.1 同名 Skill 冲突

项目级（`<workspace>/.tachi/skills/`）优先于全局级（`~/.tachi/skills/`）。`Store.List()` 按优先级遍历目录，先加入的 name 跳过后续同名项。

### 8.2 SKILL.md 格式错误

- 缺少 frontmatter：以目录名作为 name，body 全部作为描述
- YAML 解析失败：记录 warning 日志，跳过该 Skill
- `name` 字段缺失：以目录名作为 fallback

### 8.3 Skill 目录不存在

`Store.List()` 返回空列表，`BuildSkillListPrompt()` 返回空字符串（不注入 `<available_skills>` 块）。斜杠命令 fallback 直接报告 "Unknown command"，不单独提示 Skill 不存在。

### 8.4 Skill 与 SubAgent

Skill 激活状态（`activeSkills`）不传递给 SubAgent。子 agent 有独立的 system prompt，不含 Skill catalog。如果子 agent 需要某个 Skill，主 agent 应在 `prompt` 参数中包含相关指令。

### 8.5 Skill 与 Channel

Channel 会话与 TUI 会话共享同一 `skillStore`。Channel 中 `/skill-name` 通过 message text 解析而非斜杠处理函数，需要在 `channel/` 的消息预处理中添加 Skill 命令识别。

### 8.6 Skill 与 Session Resume

Session resume 时，`activeSkills` 从 session metadata 恢复（记录在 `meta.json` 中）。Resume 后的对话不会自动重新注入 Skill 内容——LLM 依赖已有上下文记忆 Skill 指令。如果 LLM "忘记"了 Skill，可以再次 `/skill-name` 重新注入。

---

## 九、未来扩展

1. **Curator 式后台维护**：跟踪 Skill 使用频率，自动标记过时 Skill，提醒用户更新
2. **Subagent-mode Skill**：`mode: subagent` 的 Skill 由 SubAgent 隔离执行，适用于高风险操作
3. **Skill 依赖**：`requires_skills: [base-skill]` 支持 Skill 间组合
4. **Skill 模板**：`/skill create <name>` 从模板生成 SKILL.md 骨架
5. **Auto-skill 触发**：配置 `auto: true` 的 Skill 始终激活（类似 project context）
6. **Skill 版本管理**：支持 semver 和更新检查，特别是项目级 Skill 随 git 更新时
