# Hashline Edit Mode 设计文档

## 概述

Hashline 是一种**基于内容 hash 锚定的代码编辑协议**，旨在显著提高 LLM 编辑文件的成功率。它将传统的"匹配旧字符串→替换为新字符串"模式，升级为"用 hash 标签验证版本→用行号锚定精确位置→用行内容做二次确认"的三段式协议。

本文档参考 [oh-my-pi](https://github.com/yejia/oh-my-pi) 项目中已落地验证的 hashline 实现，提出在 Tachi 中引入 hashline 编辑模式的方案。

---

## 为什么 Hashline 能提高成功率

LLM 编辑文件失败通常有三个原因：

| 失败模式 | 传统 EditFile 的表现 | Hashline 的解法 |
|---|---|---|
| **位置错位** | `old_string` 找不到（缩进/换行微妙差异） | 用行号锚定位置，每行独立匹配，不受前后文干扰 |
| **版本过期** | LLM 读了一个文件，但编辑时内容已变 | hash tag 校验版本，不匹配则报错阻止覆盖 |
| **歧义匹配** | `old_string` 在文件中出现多次，LLM 选了错的 | 行号 + 行内容双重确认，上下文唯一性保障 |

### 核心洞察

LLM **精确复制文本**的能力很弱（容易丢空格、改换行符），但**读行号**和**理解行内容语义**的能力很强。Hashline 扬长避短——不再要求 LLM 精确写出要替换的整段原文，而是让它用行号指出在哪里改，用行内容做语义确认。

---

## 协议格式

### Read 输出（hashline 模式）

```
¶greet.py#A1B2
1:def greet(name):
2:    msg = "Hello, " + name
3:    print(msg)
4:greet("world")
```

- `¶greet.py#A1B2` — **hashline 头部**：`¶` + 显示路径 + `#` + **4 位 hex hash**（参考实现使用 4 hex，与文档 3 hex 的差异见下文说明）
- `1:content` — **编号行**：行号（左对齐）+ `:` + 行内容

> **4-hex vs 3-hex**：oh-my-pi 使用 4-hex tag（如 `A1B2`），因为 4 hex = 65536 种可能，碰撞概率更低。文档中的实现可选 3-hex 作为 Tachi 的初始版本以简化实现，后续可升级到 4-hex。

### Edit 输入（hashline 模式）

LLM 通过 `input` 参数传入一个**带操作指令的 patch 语言**，不是裸行号替换。每个
文件 section 以 `¶PATH#TAG` 开头，后续行是操作指令和 payload。

**基本操作列表：**

| 操作 | 格式 | 说明 |
|---|---|---|
| 替换 | `replace N..M:` + body | 将原文第 N-M 行替换为 body 内容 |
| 删除 | `delete N..M` | 删除原文第 N-M 行，无 body |
| 插入在前 | `insert before N:` + body | 在第 N 行之前插入 body 行 |
| 插入在后 | `insert after N:` + body | 在第 N 行之后插入 body 行 |
| 插入头部 | `insert head:` + body | 在文件最开头插入 body 行 |
| 插入尾部 | `insert tail:` + body | 在文件末尾插入 body 行 |
| 块替换 | `replace block N:` + body | 替换第 N 行开始的整个语法块（AST 感知） |
| 块删除 | `delete block N` | 删除第 N 行开始的整个语法块 |

body 行格式：每行以 `+` 开头表示一个字面行。`+` 单独一行表示空行。

**示例 1：替换单行**

原始文件：
```
¶greet.py#A1B2
1:def greet(name):
2:    msg = "Hello, " + name
3:    print(msg)
4:greet("world")
```

将第 2 行替换为两行：
```
¶greet.py#A1B2
replace 2..2:
+    greeting = "Hi"
+    msg = f"{greeting}, {name}"
```

注意：body 行数是新的行数（2 行），但 range 始终是原始行号（`2..2`）。body 只管"最终长什么样"，range 管"删掉原来的哪些行"。

**示例 2：插入行**

在第 1 行后插入 guard：
```
¶greet.py#A1B2
insert after 1:
+    if not name: name = "stranger"
```

**示例 3：删除行**

```
¶greet.py#A1B2
delete 3
```

**示例 4：多操作编辑同一个文件**

加头加尾：
```
¶greet.py#A1B2
insert head:
+# generated header
insert tail:
+greet("everyone")
```

**示例 5：块编辑**

替换整个 `greet` 函数（`replace block 1:` 自动解析第 1 行到函数结束的整个语法块）：
```
¶greet.py#A1B2
replace block 1:
+def greet(name):
+    print(f"Hello, {name}")
```

### 多个文件

不同文件之间用空行分隔，每个文件有自己的 `¶path#tag`：

```
¶src/main.go#A1F
replace 6..6:
+    fmt.Println("hello world")

¶src/utils.go#B3C
delete 10..12
```

### 关键设计

- **Range 指原始行号**：所有行号都指向原始文件的版本。即使前面的 hunk 删了 5 行，后续 hunk 的行号也不受影响——它们是批量解析后统一执行的。
- **一次性准备，统一提交**：多 section / 多 hunk 先全部在内存中验证，再写磁盘。任何一步失败则全不执行。
- **Hash tag 校验版本**：如果文件自上次 read 后被外部修改，tag 不匹配，编辑被拒绝。
- **没有 `-` 删减行和裸上下文行**：body 只包含最终内容（`+TEXT`），range 负责删。没有 diff 格式那种 `-old` 行。
- **编辑后立即失效**：每次 apply 后 mint 新 tag，旧 tag + 行号立即作废。LLM 必须从编辑响应或重新 read 中获取新锚点。

---

## 架构设计

### 组件总览

```
                    ┌───────────────────────┐
                    │   配置文件 config.yaml   │
                    │   edit.mode: hashline  │  ← ReadFile 无独立配置
                    └──────────┬────────────┘
                               │ 读取配置
                               ▼
┌──────────────────────────────────────────────────┐
│                  Agent Configure()                │
│  ┌────────────┐  ┌────────────┐  ┌────────────┐  │
│  │  ReadFile  │  │  EditFile  │  │  WriteFile │  │
│  │ (hashline  │  │ (hashline  │  │ (不变)     │  │
│  │  输出格式)  │  │  input解析) │  │           │  │
│  └────────────┘  └────────────┘  └────────────┘  │
└──────────────────────┬───────────────────────────┘
                       │
        ┌──────────────┼──────────────┐
        ▼              ▼              ▼
┌──────────────┐ ┌──────────┐ ┌──────────────┐
│ FileSnapshot │ │ Hashline │ │ Hashline     │
│ Store        │ │ Parser   │ │ Patcher      │
│ (hash→path   │ │ (input   │ │ (apply edits │
│  映射)       │ │  解析)   │ │  + validate) │
└──────────────┘ └──────────┘ └──────────────┘
```

### 1. FileSnapshotStore

**职责**：维护文件内容 ↔ hash tag 的映射，验证版本。

```go
type SnapshotStore struct {
    mu       sync.RWMutex
    snapshots map[string]snapshotEntry // key: absolute path
}

type snapshotEntry struct {
    tag       string    // 3-hex hash (e.g. "a1f")
    hash      string    // full SHA256 (for collision detection)
    content   string    // LF-normalized content (only kept for latest)
    timestamp time.Time
}
```

关键操作：

```go
// 记录文件快照，返回 3-hex tag
func (s *SnapshotStore) Record(path string, content string) string

// 验证文件是否与快照匹配
func (s *SnapshotStore) Verify(path string, tag string) error

// 使快照失效（文件被外部修改后）
func (s *SnapshotStore) Invalidate(path string)
```

### 两层 hash 设计

Hashline 使用两层 hash：3-hex 短 tag 给 LLM 看，完整 SHA256 在内部做校验。

**第一层：3-hex tag（通讯层）**

取文件内容 SHA256 的前 3 个 hex 字符（如 `a1f`），作为 LLM 可见的友好标签。
3 hex = 4096 种可能，对一个文件在一轮会话内产生的版本数（通常 < 10）足够用。
优势是短，LLM 不容易在 `¶foo#a1f` 中写错。

**第二层：FullHash（校验层）**

完整的 SHA256（64 hex 字符）才是真正的版本校验凭据。3-hex tag 只是一个
引用编号——就像 Git 的 short SHA 和完整 SHA 的关系。

FullHash 在两个场景起作用：

1. **碰撞检测**：3-hex 可能碰撞（比如一个文件的两个不同版本恰好都有 `a1f` 前缀）。
   Verify() 查到多条候选时，用 FullHash 精确匹配当前文件内容的 SHA256 来消除歧义。

2. **快照链索引**：SnapshotStore 按文件路径维护所有历史版本。FullHash 是真正的 key，
   3-hex 只是索引上的友好标签：
   ```
   SnapshotStore["/repo/src/main.go"] = {
     "a1f": { fullHash: "e3b0c44298fc1c149afbf4c8996fb924...",
              content: "package main\n...", timestamp: T1 },
     "b3c": { fullHash: "d7a8fbb307d7809469ca9abcb0082e4f...",
              content: "package main\n...", timestamp: T2 },
   }
   ```
   当 Patcher 收到 `¶path#a1f` 时：
   1. 通过 3-hex tag 查到候选快照
   2. 用 FullHash 比对当前文件内容 → 匹配 ✅ 才允许编辑
   3. 不匹配 ❌ → 文件被外部修改，拒绝编辑

### 2. Hashline Parser

**职责**：解析 LLM 传入的 hashline 格式 input，提取 section 列表。

```go
type Section struct {
    Path    string    // 显示路径 (例如 "src/main.go")
    Tag     string    // 3-hex hash tag (例如 "a1f")
    Entries []LineEntry
}

type LineEntry struct {
    LineNum int    // 1-indexed 行号
    Content string // 行内容（无行号前缀）
}

// Parse 解析 hashline 格式文本为 section 列表
func Parse(input string, cwd string) ([]Section, error)
```

解析流程：
1. 按空行分割多个 section
2. 每 section 第一行匹配 `^¶(.+)#([0-9a-f]{3})\s*$`
3. 后续行匹配 `^\s*(\d+)\|(.*)$`
4. 验证行号递增（允许重复行号表示替换）
5. 无行号前缀的纯文本行被视为注释/分隔符，附加到前一条 entry 的 metadata 中

### 3. Hashline Patcher

**职责**：对 section 列表执行 prepare → commit 两阶段编辑。

```go
type Patcher struct {
    fs        FileSystem
    snapshots *SnapshotStore
}

// 阶段一：预检（prepare）
// 在内存中验证所有 section 并计算编辑结果，不写磁盘
func (p *Patcher) Prepare(section Section) (*PreparedSection, error)

// 阶段二：提交（commit）
// 将预检通过的编辑结果写入磁盘
func (p *Patcher) Commit(prepared *PreparedSection) (*SectionResult, error)

// 批量编辑：先全部 prepare，再依次 commit
func (p *Patcher) Apply(sections []Section) ([]SectionResult, error)
```

Prepare 逻辑：
1. 读取文件，标准化行尾为 LF，去除 BOM
2. 用 Snapshots.Verify() 校验 tag
3. 按行号分组：保留行、修改行、新增行
4. 对修改行：验证原始行内容与文件匹配（模糊匹配，容忍度由 `fuzzy_threshold` 控制）
5. 在内存中应用所有修改，生成新文件内容
6. 返回 PreparedSection（含新旧内容、差异统计）

Commit 逻辑：
1. 将 PreparedSection 中的新内容写回文件
2. 调用 fs.Write()（可集成 LSP 格式化）
3. 记录新的快照 tag

### 4. FileSystem 接口

```go
type FileSystem interface {
    Read(path string) (string, error)
    Write(path string, content string) (string, error) // 返回写入后的内容（可能被转换）
    Exists(path string) (bool, error)
}
```

### 5. ReadFile 工具改动

在 hashline 模式下，ReadFile 返回格式化的 hashline 内容。

```go
type ReadTool struct {
    mu            sync.RWMutex
    cache         map[string]cachedEntry
    snapshotStore *SnapshotStore  // NEW: hashline 快照存储
    hashlineMode  bool            // NEW: 是否启用 hashline 模式
}

func (t *ReadTool) formatHashlineOutput(path string, content string, offset int) string {
    tag := t.snapshotStore.Record(path, content)
    lines := strings.Split(content, "\n")
    
    var sb strings.Builder
    // 头部
    fmt.Fprintf(&sb, "¶%s#%s\n", displayPath, tag)
    // 编号行
    for i, line := range lines {
        lineNum := offset + i + 1
        fmt.Fprintf(&sb, "%4d│%s\n", lineNum, line)
    }
    // 末尾统计
    fmt.Fprintf(&sb, "\n[%d lines, snapshot: %s]\n", len(lines), tag)
    return sb.String()
}
```

**缓存策略**：hashline 模式下，缓存命中（文件 mtime 未变）时，直接返回上次的 hashline 输出（包括 tag），避免重复计算 hash。

### 6. EditFile 工具改动

支持两种模式切换：

```go
type EditTool struct {
    // 现有字段不变
    hashlineMode bool  // NEW: true = hashline 模式
}

func (t *EditTool) ExecuteContext(ctx context.Context, args string) (string, error) {
    if t.hashlineMode {
        return t.executeHashline(ctx, args)
    }
    return t.executeLegacy(ctx, args)
}
```

Hashline 模式的参数 schema：

```go
// hashline 模式使用 input 字段，不再使用 old_string/new_string
type HashlineArgs struct {
    Input string `json:"input"` // 包含 ¶path#tag + 行内容的编辑文本
}

// 传统模式保持现有 schema 不变
type LegacyArgs struct {
    Path       string `json:"path"`
    OldString  string `json:"old_string"`
    NewString  string `json:"new_string"`
    ReplaceAll bool   `json:"replace_all,omitempty"`
}
```

---

## 配置文件接口

在 `config.yaml` 中新增编辑模式配置。**ReadFile 的输出格式由 `edit.mode` 隐式决定**，无需独立的 read 配置：

```yaml
# edit 工具配置
edit:
  # 编辑模式：hashline（默认） | replace
  # hashline: 基于内容 hash 锚定的行级编辑（推荐）
  # replace: 传统 old_string/new_string 精确替换
  mode: hashline

  # hashline 模式下的模糊匹配阈值（0.0-1.0，默认 0.95）
  # LLM 写的内容与文件实际内容之间的可接受差异度
  fuzzy_threshold: 0.95
```

> **设计决策**：ReadFile 不做独立配置。`edit.mode=hashline` 时 ReadFile 自动输出 `¶path#tag` + 行号格式；`edit.mode=replace` 时输出纯文本。ReadFile 的输出格式是 EditFile 的输入协议的一部分，两者天然耦合。

### Config 结构体变更

```go
// config/config.go 新增字段

type EditConfig struct {
    Mode           string  `yaml:"mode" default:"hashline"`       // hashline | replace
    FuzzyThreshold float64 `yaml:"fuzzy_threshold" default:"0.95"`
}

type Config struct {
    // ... 现有字段
    Edit EditConfig `yaml:"edit"`
    // ReadFile 无独立配置——输出格式由 Edit.Mode 隐式决定
}
```

### FuzzyThreshold 详解

`fuzzy_threshold` 控制 Patcher 在 **Prepare 阶段做行级内容校验时的模糊匹配容忍度**。

**为什么需要它？** Hashline 的定位是"扬长避短"——承认 LLM 精确复制文本的能力弱（容易丢空格、改引号），所以用行号锚定位置，用近似匹配降低摩擦。`fuzzy_threshold` 就是控制这个"摩擦容忍度"的旋钮。

**校验逻辑**（在 Prepare 中，对每一行修改）：

```
similarity := levenshteinSimilarity(llmContent, actualContent)
if similarity < fuzzy_threshold {
    return error("行内容不匹配：预期「...」与实际差异过大")
}
```

**阈值效果**：

| 阈值 | 行为 | 典型场景 |
|------|------|---------|
| `1.0` | 完全精确匹配 | 强迫症模式，或 LLM 极其可靠时 |
| `0.95`（默认） | 允许 1-2 字符差异或空白微差 | 大多数 LLM（Claude/GPT-4o）的稳妥选择 |
| `0.85` | 允许较多差异 | 对齐较弱的小模型，或频繁出现引号归一化问题 |

选择建议：从 `0.95` 开始用。如果 LLM 频繁因"行内容不匹配"报错，再适度下调。不需要频繁调整，这是"设一次就安心"的参数。

---

## 配置与工具联动的逻辑

核心联动规则——**ReadFile 的输出格式与 EditFile 的输入格式配对**，由 `edit.mode` 统一决定：

```
edit.mode=hashline  →  ReadFile 输出 hashline 格式（¶path#tag + 行号）
                       EditFile 接受 hashline input 格式

edit.mode=replace   →  ReadFile 输出纯文本
                       EditFile 使用 old_string/new_string 传统模式
```

实现位置：在 `agent.Configure()` 中，根据 `cfg.Edit.Mode` 创建对应行为的 tool 实例：

```go
func (a *AIAgent) Configure(cfg *config.Config) {
    // ... 现有逻辑
    isHashline := cfg.Edit.Mode == "hashline"

    readTool := tools.NewReadTool()
    readTool.SetHashlineMode(isHashline)
    
    editTool := tools.NewEditTool()
    editTool.SetHashlineMode(isHashline)
    
    a.toolRegistry.Register(readTool)
    a.toolRegistry.Register(editTool)
    // ...
}
```

**为什么不让 ReadFile 自己查配置？** 工具实例在 `Configure()` 中注入时就已经确定了模式。同一会话期间 `edit.mode` 不会动态变更（变更需要重启或 `/reload`），所以将模式作为工具结构体字段比每次从 context 查配置更直接高效。如果需要支持运行时切换，改用 context 方案即可：

```go
// 备选方案：通过 context 获取配置（支持热切换）
func (t *EditTool) ExecuteContext(ctx context.Context, args string) (string, error) {
    cfg := config.FromContext(ctx)
    if cfg.Edit.Mode == "hashline" {
        return t.executeHashline(ctx, args)
    }
    return t.executeLegacy(ctx, args)
}
```

---

## 实现计划

### Phase 1 — 基础设施（SnapshotStore + Hash 算法）

**文件**: `agent/edit/snapshot.go`, `agent/edit/hash.go`

- [ ] 实现 `SHA256Prefix(content) → string`（返回前 3 hex 字符）
- [ ] 实现 `SnapshotStore`（线程安全、支持 Record/Verify/Invalidate）
- [ ] 单元测试：快照记录、校验、失效、碰撞检测

### Phase 2 — 解析器

**文件**: `agent/edit/parser.go`

- [ ] 实现 `Parse(input, cwd) → []Section`，支持：
  - `¶path#tag` 头部解析
  - `LINE│content` 行条目解析
  - 多 section 支持（空行分隔）
  - 错误处理：格式错误、路径解析失败
- [ ] 单元测试：各种合法/非法格式、边界情况

### Phase 3 — Patcher

**文件**: `agent/edit/patcher.go`

- [ ] 实现 `Prepare(section) → PreparedSection`
  - 文件读取、BOM 去除、行尾标准化
  - 行号验证（是否在文件范围内）
  - 修改行的内容模糊匹配（±空白差异）
  - 在内存中计算编辑结果
- [ ] 实现 `Commit(prepared) → SectionResult`
  - 写入文件
  - 记录新快照
- [ ] 实现 `Apply(sections) → []SectionResult`（先全部 prepare，再依次 commit）
- [ ] 单元测试：单行替换、多行替换、新增行、上下文行保留、tag 不匹配拒绝、模糊匹配

### Phase 4 — ReadFile 改造

- [ ] 新增 `hashlineMode` 字段，由 `Configure()` 根据 `cfg.Edit.Mode` 注入
- [ ] hashline 模式：输出 `¶path#tag` 头部 + 行号内容
- [ ] 记录快照、缓存 hashline 输出
- [ ] 集成现有缓存命中逻辑（mtime 未变时复用上次输出）

### Phase 5 — EditFile 改造

- [ ] 根据配置选择执行路径
- [ ] hashline 模式：解析 input → Patcher.Apply → 返回结果
- [ ] 保留现有传统模式作为 fallback
- [ ] 冲突检测：如果 LLM 用混了格式（hashline 头 + old_string），给出清晰错误

### Phase 6 — 配置集成

- [ ] Config 结构体新增 `EditConfig`
- [ ] `Configure()` 按 `edit.mode` 注入对应模式的 ReadFile/EditFile 实例
- [ ] 热重载支持（如果编辑模式在会话中切换，read/edit 的下一轮调用生效）
- [ ] 系统 prompt 根据模式渲染不同的 tool description

---

## 参考实现

oh-my-pi 项目（`packages/hashline/`）中的核心模块：

| 模块 | oh-my-pi 文件 | 说明 |
|---|---|---|
| SnapshotStore | `snapshots.ts` | 快照存储，Record/Verify/Invalidate |
| Parser | `input.ts` + `parser.ts` + `tokenizer.ts` | 将 hashline 文本解析为 sections |
| Patcher | `patcher.ts` + `apply.ts` | 两阶段编辑（prepare/commit） |
| FileSystem | `fs.ts` | 文件读写接口 |
| Diff Preview | `diff-preview.ts` | 生成紧凑的 diff 摘要 |
| Recovery | `recovery.ts` | tag 不匹配时的恢复策略 |
| Block Resolver | `block.ts` | 基于 tree-sitter 的块级锚定（可选高级特性） |
| Format | `format.ts` | hash 计算、header 格式化 |

oh-my-pi 的 coding-agent 集成（`packages/coding-agent/src/edit/`）：

| 模块 | 文件 | 说明 |
|---|---|---|
| EditTool 入口 | `index.ts` | 四种模式切换（hashline/replace/patch/apply_patch） |
| Executor | `hashline/execute.ts` | hashline 模式的主执行逻辑 |
| FileSystem 适配 | `hashline/filesystem.ts` | 对接 LSP writethrough |
| Block Resolver | `hashline/block-resolver.ts` | 对接 tree-sitter native |
| Diff 工具 | `diff.ts` | diff 生成、hunk 解析 |
| 渲染器 | `renderer.ts` | TUI 显示编辑结果 |

---

## 附录：与传统 EditFile 模式的对比

| 维度 | 传统 replace 模式 | Hashline 模式 |
|---|---|---|
| LLM 需要提供 | 精确的 old_string（字节级匹配） | 行号 + 近似行内容 |
| 匹配策略 | 精确匹配 → 智能引号归一化 | 行号锚定 → 行级模糊匹配 |
| 版本校验 | 无（直接覆盖） | Hash tag 校验 |
| 歧义处理 | 报错"找到 N 处匹配" | 行号唯一锚定，几乎无歧义 |
| 多文件编辑 | 多次工具调用 | 一次 input 编辑多个文件 |
| 上下文要求 | 需要完整 old_string | 只需要受影响的几行 |
| 实现复杂度 | 低 | 中高 |
| 编辑成功率 | ~60-80%（依模型/语言而定） | ~90-95%（oh-my-pi 实测） |
