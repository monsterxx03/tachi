# skill_create 工具设计文档

> 日期: 2026-05-13 | 状态: 已实现 | 关联: [Skill 机制设计文档](./2026-05-11-skill-design.md)

## 一、动机

Skill 系统现有两个只读工具：`skills_list`（列出可用 Skill）和 `skill_view`（加载 Skill 内容）。LLM 想要创建 Skill 只能通过 `WriteFile` 手写 `SKILL.md`，存在以下问题：

1. **缺乏验证**：name 格式、description 长度等约束无法保证
2. **概念不对齐**：`skills_list` / `skill_view` 的 Skill 概念与 `WriteFile` 的路径操作没有直接联系
3. **容易出错**：LLM 需要知道路径约定（`.tachi/skills/<name>/SKILL.md`）和 frontmatter 格式，手动拼接容易出错

新增 `skill_create` 填补写入能力空白，让 LLM 用 Skill 的原生概念来创建 Skill。

---

## 二、设计决策

### 2.1 默认写入位置

**默认 `source: "project"`**（`.tachi/skills/`），可显式指定 `"global"`（`~/.tachi/skills/`）。

理由：项目级 Skill 是主要使用场景（跟随 git 仓库团队协作），全局 Skill 是辅助。

### 2.2 覆盖行为

**默认拒绝覆盖**（`overwrite: false`），需 LLM 显式传 `overwrite: true` 才会覆盖同名 Skill。

理由：Skill 是精心编写的指令集，意外覆盖可能丢失有价值的工作。

### 2.3 Store.Create 签名

**Store 接收散字段**（name / description / body / tags / source / overwrite），内部构造 `SKILL.md`，而非接收拼接好的 Markdown 字符串。

理由：
- Store 负责格式规范，确保 `SKILL.md` 的 frontmatter 格式始终正确
- Tool 层只负责参数校验和类型转换
- 分层清晰：Tool = 协议层，Store = 领域层

---

## 三、实现架构

### 3.1 文件变更清单

| 文件 | 变更 | 说明 |
|------|------|------|
| `agent/tools/skill_create.go` | **新文件** | `SkillCreateTool` + `SkillCreator` 接口 + `SkillCreateParams` / `SkillCreateResult` |
| `agent/skill/store.go` | **修改** | Store 新增 `source` 字段 + `Create()` 方法 + `buildSkillMarkdown()`；`List()`/`Load()` 改用 `s.source[i]` 替代 `i > 0` 判断 |
| `agent/skill/adapter.go` | **修改** | Store 实现 `tools.SkillCreator` 接口；新增 `CreateSkill()` 适配方法 |
| `agent/tools/tool.go` | **修改** | 新增 `ToolNameSkillCreate = "skill_create"`；统一注册 `ToolNameSkillsList`/`ToolNameSkillView` |
| `agent/tools/skills_list.go` | **修改** | 删除局部 `ToolNameSkillsList` 常量，改用 `tool.go` 统一常量 |
| `agent/tools/skill_view.go` | **修改** | 删除局部 `ToolNameSkillView` 常量，改用 `tool.go` 统一常量 |
| `agent/tools/skill_test.go` | **修改** | 新增 `TestSkillCreateTool` 测试 + `stubSkillCreator` |
| `agent/skill/store_test.go` | **修改** | 新增 `TestStoreCreate`、`TestStoreCreate_Validation`、`TestStoreCreate_NoProjectDir`、`TestBuildSkillMarkdown`；旧测试改用 `newStore()` 隔离全局路径 |
| `agent/agent.go` | **修改** | `registerSkillTools` 注册 `skill_create`；`ReloadSkills` 取消注册 |

### 3.2 接口分层（遵循现有模式）

```
tools 包定义接口 → skill/adapter.go 实现接口 → agent/agent.go 注册工具
```

```go
// tools 包
type SkillCreator interface {
    CreateSkill(params SkillCreateParams) (*SkillCreateResult, error)
}

// skill/adapter.go
func (s *Store) CreateSkill(params tools.SkillCreateParams) (*tools.SkillCreateResult, error) {
    sk, err := s.Create(params.Name, params.Description, params.Body,
        params.Tags, params.Source, params.Overwrite)
    ...
}
```

与 `SkillLister` / `SkillLoader` 接口模式完全一致。

### 3.3 Store.source 重构

为正确映射 source 到目录路径（特别是 `NewStore("")` 时仅有一个全局目录的场景），Store 新增 `source []string` 字段与 `dirs` 一一对应：

```go
type Store struct {
    dirs   []string // 目录路径
    source []string // 对应的 "project" 或 "global"
    logger *debuglog.Logger
}
```

`NewStore()` 构造时同步填充。`List()`、`Load()`、`Create()` 统一通过 `s.source[i]` 获取 source。

### 3.4 数据流

```
LLM calls skill_create(name, description, body, tags, source, overwrite)
  → SkillCreateTool.ExecuteContext()
    → skillCreator.CreateSkill(params)
      → Store.Create()
        1. ValidateName(name)
        2. description 长度 ≤ MaxDescriptionLen
        3. source 必须在 {"project", "global"} 中
        4. 遍历 s.dirs/s.source 找对应目录
        5. stat 检查同名 → overwrite=false 且存在 → 报错
        6. os.MkdirAll + buildSkillMarkdown() + WriteFile
        7. 返回 *Skill
  → 返回 JSON {success, skill:{name,description,tags,source,path}, message}
  → 下一条用户消息的 system-reminder 自动包含新 Skill
     （Store.List() 每次都读磁盘，无需 ReloadSkills）
```

---

## 四、API 规范

### 工具定义

| 属性 | 值 |
|------|------|
| `Name` | `"skill_create"` |
| `Parallel` | `false` |
| `Required` | `name, description, body` |

### 参数

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `name` | string | ✅ | Skill 名称（小写+数字+连字符，≤64 字符） |
| `description` | string | ✅ | 一句话描述（≤1024 字符） |
| `body` | string | ✅ | Skill 指令（Markdown 格式） |
| `tags` | []string | 否 | 分类标签 |
| `source` | string | 否 | `"project"`（默认）或 `"global"` |
| `overwrite` | boolean | 否 | 是否覆盖同名 Skill（默认 false） |

### 返回

```json
{
  "success": true,
  "skill": {
    "name": "code-review",
    "description": "Review code for bugs and security",
    "tags": ["review", "security"],
    "source": "project",
    "path": "/path/to/.tachi/skills/code-review/SKILL.md"
  },
  "message": "Skill \"code-review\" created at ..."
}
```

---

## 五、边界情况

| 情况 | 行为 |
|------|------|
| 同名存在，`overwrite: false` | 返回错误，提示路径和 overwrite 选项 |
| 同名存在，`overwrite: true` | 覆盖 `SKILL.md`（仅覆盖此文件，不动其他子文件） |
| name 格式非法 | 返回 `ValidateName` 错误 |
| description 超长 | 返回长度超限错误 |
| source 为非法值 | 返回 unknown source 错误 |
| 目标目录不可用 | 返回 "skill directory not available" 错误 |
| sub-agent 调用 | `skill_create` 在 sub-agent 中可用（不被 block） |

### 与 SubAgent 的关系

`skill_create` **不在** sub-agent 的排除列表中。这意味着子 agent 也可以创建 Skill。如果用户不希望子 agent 有这个能力，可以通过 `allowed_tools` 白名单排除。

### 与 `/skill reload` 的关系

`Store.List()` 每次调用都重新扫描磁盘，因此 `skill_create` 创建后无需显式 reload，下一轮 system-reminder 注入时自动出现。用户主动 `/skill reload` 也可以立即生效。

---

## 六、未来扩展

1. **skill_update 工具**：修改已有 Skill 的 description/body/tags（不用 overwrite）
2. **skill_delete 工具**：删除 Skill 目录
3. **确认机制**：高风险操作（如 delete）通过 `ConfirmationTool` 接口实现 TUI 确认
4. **skill_create 支持 references/**：创建时同时写入参考文件
5. **参数校验**：body 最小长度检查（防止空指令）