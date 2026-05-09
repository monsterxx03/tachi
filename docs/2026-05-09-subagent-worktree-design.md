# SubAgent Git Worktree 支持 — 设计文档

> 版本: 1.1 | 日期: 2026-05-09 | 状态: 设计阶段

## 一、背景与动机

### 1.1 当前问题

SubAgent 目前共用主 agent 的文件系统工作目录。当多个 SubAgent 并行执行时，它们共享同一个 working directory（`agent/tools/bash.go` 中的 `workingDir` 全局变量），这意味着：

1. **写入冲突**：两个并行子 agent 可能编辑同一组文件，互相覆盖
2. **暂存区污染**：子 agent 在探索过程中创建的临时文件、git add/commit 等操作会污染主工作区的 git 状态
3. **上下文混乱**：子 agent 改动文件后，主 agent 的工作区状态发生了变化，难以追踪哪些改动来自哪个子 agent

### 1.2 设计目标

| 目标 | 说明 |
|------|------|
| 隔离性 | 启用后，每个 SubAgent 在独立的 git worktree 中运行，与主工作区完全隔离 |
| 向后兼容 | 默认不启用，不破坏现有行为 |
| 轻量 | 基于 `git worktree add`，开销接近零（hardlink 方式共享 .git 对象） |
| 可清理 | 子 agent 完成后自动清理临时 worktree（可配置保留以用于调试） |
| 可降级 | 非 git 仓库或 worktree 创建失败时，优雅降级到共享目录模式 |

---

## 二、核心概念

### 2.1 Git Worktree 简介

```
主工作区:  /Users/will/repos/tachi           (HEAD: main)
worktree:  /tmp/tachi-subagent-a3f2/         (HEAD: main, detached)
worktree:  /tmp/tachi-subagent-e9d1/         (HEAD: feat/experiment, 指定分支)
```

- `git worktree add --detach /tmp/tachi-subagent-xxx HEAD` — detached HEAD，最简单
- `git worktree add /tmp/tachi-subagent-xxx feat/some-branch` — 检出指定分支
- 每个 worktree 有独立的暂存区、工作目录、未跟踪文件
- `.git` 对象通过 hardlink 共享，不占用额外磁盘空间
- 完成后 `git worktree remove --force /tmp/tachi-subagent-xxx` 清理

### 2.2 关键行为对比

| | 主工作区 | SubAgent Worktree (detached) | SubAgent Worktree (指定分支) |
|---|---------|---------------------------|---------------------------|
| 路径 | `repos/tachi` | `/tmp/tachi-sub-xxx/` | `/tmp/tachi-sub-xxx/` |
| HEAD | main | main (detached) | feat/experiment |
| 分支所属 | 跟踪分支 | 无 (detached HEAD) | 跟踪指定分支 |
| Git 状态 | 正常开发 | 完全隔离 | 完全隔离 |
| 可提交 | ✅ | ✅ (提交后 28 天 git gc) | ✅ |
| 适用场景 | 正常开发 | 探索、分析、搜索 | 跨分支开发、cherry-pick、并行 PR |

---

## 三、架构设计

### 3.1 整体流程

```
                         ┌───────────────────────────────────┐
                         │         SubagentExecutor           │
                         │                                  │
                         │  RunSubagent(ctx, prompt,         │
                         │               tools, branch)      │
                         │     │                            │
                         │     ├─ worktree enabled? ── N ──► 共享目录（现状）│
                         │     │ YES                        │
                         │     ▼                            │
                         │  ┌──────────────────────────┐   │
                         │  │ branch 解析:              │   │
                         │  │  args.branch ||           │   │
                         │  │  config.worktree_branch ||│   │
                         │  │  "" (detached HEAD)       │   │
                         │  ├──────────────────────────┤   │
                         │  │ branch 非空:              │   │
                         │  │  git fetch origin <b>     │   │
                         │  │  (如本地不存在)            │   │
                         │  ├──────────────────────────┤   │
                         │  │ git worktree add          │   │
                         │  │   [--detach] <path> <b>   │   │
                         │  ├──────────────────────────┤   │
                         │  │ ctx = wdctx.WithDir(ctx,  │   │
                         │  │        worktreePath)      │   │
                         │  ├──────────────────────────┤   │
                         │  │ runChildAgent(ctx, ...)   │   │
                         │  │   → Bash cmd.Dir =       │   │
                         │  │     wdctx.Dir(ctx)        │   │
                         │  ├──────────────────────────┤   │
                         │  │ git worktree remove       │   │
                         │  │   --force (清理)          │   │
                         │  └──────────────────────────┘   │
                         └───────────────────────────────────┘
```

### 3.2 工作目录隔离方案

**问题**：`tools/bash.go` 使用全局变量 `workingDir`，不适合多 SubAgent 并发场景。

**方案**：引入 context-based working directory。

```go
// agent/wdctx/workingdir.go — 新包
// 注意：不使用 agent/context/ 路径，避免与标准库 context 包名冲突

package wdctx

type contextKey struct{}

func WithDir(ctx context.Context, dir string) context.Context {
    return context.WithValue(ctx, contextKey{}, dir)
}

func Dir(ctx context.Context) string {
    if dir, ok := ctx.Value(contextKey{}).(string); ok && dir != "" {
        return dir
    }
    // fallback to tools.getWorkingDir() — 保留对 Bash cd 命令的兼容
    // （主 agent 通过 SetWorkingDir 追踪用户 cd 后的目录）
    return getWorkingDir()
}
```

**Bash tool 修改**（仅一处）：

```go
// agent/tools/bash.go — ExecuteContext
cmd.Dir = wdctx.Dir(ctx) // 替代 getWorkingDir()
```

**全局变量保留**：`SetWorkingDir` / `getWorkingDir` 不删除，`wdctx.Dir` 在其 fallback 路径中调用 `getWorkingDir()`，确保向后兼容。

**Worktree 模式下的 `cd` 语义**：当 context 中已设置 worktree 路径时，`wdctx.Dir(ctx)` 始终返回该固定值，不受 Bash `cd` 命令影响。这意味着 SubAgent 每次 Bash 调用都从 worktree root 开始执行。跨调用的目录切换需使用 `cd <dir> && <cmd>` 组合写法。这是有意为之的设计——避免并行 SubAgent 通过全局 `SetWorkingDir` 互相干扰。

### 3.3 文件工具的目录语义

| 工具 | 受 worktree 影响？ | 说明 |
|------|-------------------|------|
| `Bash` | ✅ 是 | `cmd.Dir` 指向 worktree |
| `ReadFile` | ✅ 是 | 相对路径基于 `wdctx.Dir(ctx)` 解析 |
| `WriteFile` | ✅ 是 | 同上 |
| `EditFile` | ✅ 是 | 同上 |
| `Glob` | ✅ 是 | 内部 exec `rg --files`，`cmd.Dir` + `resolveSearchPath` 均指向 worktree |
| `Grep` | ✅ 是 | 同上 |

**设计决策**：所有接受文件路径的工具都需要 context-aware 的路径解析。虽然 LLM 通常会在 Bash 探索后给出绝对路径，但当 LLM 使用相对路径（如 `ReadFile("main.go")`）时，`filepath.Abs` 会基于进程 CWD 而非 worktree 解析，导致读到主工作区的文件。因此：

- **Bash**：`cmd.Dir = wdctx.Dir(ctx)`
- **ReadFile/WriteFile/EditFile**：相对路径通过 `filepath.Join(wdctx.Dir(ctx), path)` 解析
- **Glob/Grep**：`resolveSearchPath` 增加 ctx 参数，非绝对路径基于 `wdctx.Dir(ctx)` 解析

```go
// agent/tools/rg.go — 修改后的 resolveSearchPath
func resolveSearchPath(ctx context.Context, path string) (string, error) {
    if path == "" { path = "." }
    if !filepath.IsAbs(path) {
        path = filepath.Join(wdctx.Dir(ctx), path)
    }
    abs, err := filepath.Abs(path)
    // ...
}
```

---

## 四、配置设计

### 4.1 配置扩展

```go
// config/config.go — SubagentConfig 新增字段

type SubagentConfig struct {
    // ... 现有字段 ...
    
    Worktree         bool   `yaml:"worktree"`          // 启用 git worktree 隔离（默认 false）
    WorktreeDir      string `yaml:"worktree_dir"`      // worktree 存放目录（默认 os.TempDir()）
    WorktreeCleanup  *bool  `yaml:"worktree_cleanup"`  // 完成后清理（默认 true）
    WorktreeBranch   string `yaml:"worktree_branch"`   // worktree 默认检出分支（默认空=detached HEAD）
}
```

```go
// agent/subagent.go — hardcoded fallback 常量
const defaultSubagentWorktreeCleanup = true
// 空字符串表示 detached HEAD
```

### 4.2 分支指定策略

分支可以在两个层面指定：

| 层面 | 方式 | 优先级 |
|------|------|--------|
| 配置级 | `subagent.worktree_branch: "feat/xxx"` | 低（全局默认） |
| 调用级 | LLM 通过 `SubAgent` tool 的 `worktree_branch` 参数覆盖 | 高（单次覆盖） |

**为什么需要调用级覆盖？** 主 agent 可能并行派发两个子 agent 到不同分支：
- 子 agent A 在 `main` 分支搜索现有实现
- 子 agent B 在 `feat/new-api` 分支基于已有改动继续开发

### 4.3 SubAgent Tool Schema 扩展

在 `agent/tools/subagent.go` 的 `SubagentTool.Properties()` 中新增参数：

```go
"worktree_branch": {
    Type:        "string",
    Description: "Optional: git branch to checkout in the sub-agent's isolated worktree. " +
                 "When empty, the worktree starts at detached HEAD (current commit). " +
                 "Only meaningful when worktree mode is enabled. " +
                 "Use this when the sub-agent needs to work on a specific branch " +
                 "(e.g., cross-branch analysis, parallel PR development).",
},
```

### 4.4 配置示例

```yaml
subagent:
  # 启用 worktree，默认分支为 detached HEAD
  worktree: true
  worktree_dir: "/tmp/tachi-worktrees"
  worktree_cleanup: true

  # 可选：所有子 agent 默认检出此分支
  # worktree_branch: "develop"
```

LLM 调用时可按需覆盖分支：

```json
{
  "prompt": "搜索 feat/new-api 分支中的 handler 实现",
  "allowed_tools": ["ReadFile", "Grep", "Glob"],
  "worktree_branch": "feat/new-api"
}
```

---

## 五、实现方案

### 5.1 WorktreeManager — `agent/subagent_worktree.go`（新文件）

```go
// WorktreeManager 管理 git worktree 的创建和清理。
type WorktreeManager struct {
    worktreeDir    string
    defaultBranch  string   // 空 = detached HEAD
    cleanup        bool
    logger         *debuglog.Logger
}

func NewWorktreeManager(cfg *config.Config, logger *debuglog.Logger) *WorktreeManager

// Create 在临时 worktree 中执行回调。回调的 ctx 已注入 worktree 路径。
// branch 为空时创建 detached HEAD；不为空时检出指定分支。
// 回调执行完毕后自动清理（根据配置）。worktree 创建失败时降级为共享目录。
func (wm *WorktreeManager) Create(
    ctx context.Context,
    branch string,
    fn func(ctx context.Context, worktreePath string) (string, error),
) (string, error)
```

**核心逻辑**：

```
1. branch 为空 → git worktree add --detach <tmpdir>/tachi-subagent-<uuid8> HEAD
   branch 非空 → git worktree add --detach <tmpdir>/tachi-subagent-<uuid8> <branch>
     （统一使用 --detach，避免同一分支多个 worktree 的 git 限制；
      自动 git fetch origin <branch> 确保分支存在）
2. ctx = wdctx.WithDir(ctx, worktreePath)
3. result, err = fn(ctx, worktreePath)
4. patch = collectPatch(worktreePath)  // 新增：收集变更 patch
5. if cleanup: git worktree remove --force <worktreePath>
6. return result + patch, err
```

**变更收集（Patch 输出）**：回调执行完毕、cleanup 之前，自动检测 worktree 中的文件变更并生成 patch：

```go
// collectPatch 在 worktree 中检测改动并生成统一 diff
func (wm *WorktreeManager) collectPatch(worktreePath string) string {
    // 1. git add -A（确保 untracked 文件也被 diff 捕获）
    // 2. git diff --cached --stat（快速检查是否有改动）
    // 3. 无改动 → 返回空字符串
    // 4. 有改动 → git diff --cached（生成完整 unified diff）
    // 5. 如果 patch 超过 maxPatchSize（默认 32KB）→ 截断 + 追加摘要
    //    "... patch truncated (total: 128KB). Use worktree_cleanup=false to inspect."
    // 6. 包裹为结构化输出:
    //    "\n\n---\n[WORKTREE_PATCH]\n<patch content>\n[/WORKTREE_PATCH]"
    return patchBlock
}
```

**Patch 输出格式**：

```
<子 agent 的文本返回>

---
[WORKTREE_PATCH]
diff --git a/main.go b/main.go
index 1234567..abcdefg 100644
--- a/main.go
+++ b/main.go
@@ -10,3 +10,5 @@
 func main() {
+    fmt.Println("hello")
 }
[/WORKTREE_PATCH]
```

**主 agent 消费 Patch 的方式**：

- Patch 作为 SubAgent tool 返回值的一部分传回主 agent 的 conversation context
- 主 agent 可通过 Bash 执行 `git apply` 将 patch 应用到主工作区
- 或主 agent 读取 patch 内容后使用 EditFile 精确修改文件
- **不做自动 apply** —— 让主 agent（或用户）决定是否采纳

**为什么不自动 apply？**

1. 主 agent 可能只需要 patch 中的部分改动
2. 主工作区可能已经有变更，需要人工决策冲突
3. 保持 SubAgent 的纯"建议"语义，主 agent 保留控制权

**分支检查与获取**：如果指定分支在本地不存在，自动执行 `git fetch origin <branch>:<branch>` 拉取远程分支。

**降级处理**：

```
step 1 失败 → log warning → fn(原始ctx, "")  → 返回（无 worktree 隔离）
```

### 5.2 SubagentArgs 扩展 — `agent/tools/subagent.go`

```go
type SubagentArgs struct {
    Prompt          string   `json:"prompt"`
    AllowedTools    []string `json:"allowed_tools"`
    MaxIterations   int      `json:"max_iterations"`
    WorktreeBranch  string   `json:"worktree_branch"`  // 新增：worktree 检出分支
}
```

### 5.3 SubagentExecutor 集成 — `agent/subagent.go`（修改）

```go
type SubagentExecutor struct {
    parentAgent *AIAgent
    logger      *debuglog.Logger
    sem         chan struct{}
    worktreeMgr *WorktreeManager  // 新增：nil = 不启用
}

func (e *SubagentExecutor) SetWorktreeManager(wm *WorktreeManager) {
    e.worktreeMgr = wm
}

func (e *SubagentExecutor) RunSubagent(ctx context.Context, args SubagentArgs) (string, error) {
    // ... 获取信号量、确定 provider/model/budget ...
    shortID := uuid.New().String()[:8]
    branch := args.WorktreeBranch  // 调用级优先；为空则由 WorktreeManager 使用 defaultBranch

    if e.worktreeMgr != nil {
        return e.worktreeMgr.Create(ctx, branch, func(worktreeCtx context.Context, wtPath string) (string, error) {
            e.logger.Log("[subagent:%s] worktree created at %s (branch=%s)", shortID, wtPath, orEmpty(branch, "detached"))
            return e.runChildAgent(worktreeCtx, shortID, args, provider, model, thinking)
        })
    }
    return e.runChildAgent(ctx, shortID, args, provider, model, thinking)
}

// runChildAgent 从原 RunSubagent 抽取内聚方法，worktree 和共享目录路径复用
func (e *SubagentExecutor) runChildAgent(
    ctx context.Context, shortID string, args SubagentArgs,
    provider llm.Provider, model string, thinking bool,
) (string, error) {
    // ... 创建 child AIAgent, 注册工具, RunOneOffStream, 消费事件 ...
}
```

**分支解析逻辑**：`RunSubagent` 中的 `branch` 取值为 `SubagentArgs.WorktreeBranch || WorktreeManager.defaultBranch`。

### 5.4 SubagentTool.ExecuteContext 适配 — `agent/tools/subagent.go`（修改）

```go
func (t *SubagentTool) ExecuteContext(ctx context.Context, args string) (string, error) {
    var sa SubagentArgs
    // ... 解析 ...

    result, err := t.runner.RunSubagent(ctx, sa)
    // ... 截断 + 错误包装 ...
}
```

同步更新 `SubagentRunner` 接口（使用 args struct 而非逐个参数，便于未来扩展且不破坏接口兼容）：

```go
type SubagentRunner interface {
    RunSubagent(ctx context.Context, args SubagentArgs) (string, error)
    AvailableToolNames() []string
    MaxOutputChars() int
}
```

### 5.5 AIAgent 新增字段 — `agent/agent.go`（修改）

```go
type AIAgent struct {
    // ... 现有字段 ...
    subagentWorktree        bool
    subagentWorktreeDir     string
    subagentWorktreeCleanup bool
    subagentWorktreeBranch  string   // 新增：默认检出分支
}

func (a *AIAgent) SubagentWorktree() bool       { return a.subagentWorktree }
func (a *AIAgent) SubagentWorktreeDir() string   { return a.subagentWorktreeDir }
func (a *AIAgent) SubagentWorktreeCleanup() bool { return a.subagentWorktreeCleanup }
func (a *AIAgent) SubagentWorktreeBranch() string { return a.subagentWorktreeBranch }
```

### 5.6 Configure 集成 — `agent/agent.go`（修改）

> **注意**：当前 `Configure` 中存在两条路径（有 MCP 配置和无 MCP 配置），两处都注册了 SubagentTool。下面的修改需要在**两条路径**中都生效，确保无论是否启用 MCP，worktree 都正确初始化。

```go
func (a *AIAgent) Configure(ctx context.Context, cfg *config.Config) (*mcp.Manager, error) {
    // ... 现有 MCP 和工具注册 ...
    
    a.SetupSubagentProvider(cfg)
    a.subagentWorktree = cfg.Subagent.Worktree
    a.subagentWorktreeDir = cfg.Subagent.WorktreeDir
    a.subagentWorktreeBranch = cfg.Subagent.WorktreeBranch
    if cfg.Subagent.WorktreeCleanup != nil {
        a.subagentWorktreeCleanup = *cfg.Subagent.WorktreeCleanup
    } else {
        a.subagentWorktreeCleanup = true
    }

    executor := NewSubagentExecutor(a)
    if a.SubagentWorktree() {
        executor.SetWorktreeManager(NewWorktreeManager(cfg, a.logger))
    }
    a.RegisterTool(tools.NewSubagentTool(executor))
    return mgr, nil
}
```

---

## 六、SubAgent System Prompt 调整

当 worktree 启用时，在子 agent system prompt 末尾动态追加：

```
You are working in an isolated git worktree. Your working directory is a
temporary checkout of the repository — changes here will NOT affect the main
working tree unless you push or create a PR from this branch.

- All file paths are relative to your worktree directory.
- Use Bash to run git commands — they operate on this worktree in isolation.
- You are on branch <branch> (or detached HEAD). You can commit, push, and
  create branches as needed without affecting the main worktree.
- When done, the worktree will be automatically cleaned up.
- Any file modifications you make will be automatically collected as a patch
  and returned to the parent agent. You do NOT need to output diffs manually.
- If you need to persist changes beyond the patch, push to remote.
- IMPORTANT: In detached HEAD mode, commits not attached to a branch will be
  garbage collected after ~28 days. Always push or create a branch to persist.
```

实现方式：`runChildAgent` 中根据是否 worktree 拼接。

---

## 七、前置条件与优雅降级

### 7.1 前置检查

| 条件 | 不满足时的行为 |
|------|---------------|
| 当前目录是 git 仓库 | 降级到共享目录模式 |
| `git worktree add` 执行成功 | 降级到共享目录模式 |
| 磁盘空间充足 | 由 OS 报错自然处理 |

> **注意**：主工作区是否"干净"（有无未提交修改）**不影响** worktree 创建。`git worktree add --detach <path> HEAD` 从 git object store 中签出文件，与主工作区的暂存/未跟踪状态完全无关。脏工作区场景仅在日志中记录 info 级别提示，不做降级。

### 7.2 降级矩阵

| 场景 | 行为 |
|------|------|
| `worktree: true` 但不在 git 仓库 | 共享目录模式，日志 warning |
| `worktree: true` 但 `git worktree add` 失败 | 共享目录模式，日志 warning |
| `worktree: true` 但清理失败 | 已返回结果，清理失败不影响，日志 error |
| `worktree: false`（默认） | 不做任何操作 |

---

## 八、文件变更清单

| 文件 | 变更 | 说明 |
|------|------|------|
| `config/config.go` | 修改 | `SubagentConfig` 新增 `Worktree`/`WorktreeDir`/`WorktreeCleanup`/`WorktreeBranch` 字段 |
| `agent/agent.go` | 修改 | `AIAgent` 新增 worktree 相关字段 + getter；`Configure` 两条路径均需适配 |
| `agent/subagent.go` | 修改 | `SubagentExecutor` 新增 `worktreeMgr`；抽取 `runChildAgent`；`RunSubagent` 改为接收 `SubagentArgs` |
| `agent/subagent_worktree.go` | **新文件** | `WorktreeManager` + `Create()` + 分支 fetch 逻辑 + 降级逻辑 |
| `agent/wdctx/workingdir.go` | **新文件** | `wdctx.WithDir()` / `wdctx.Dir()`（fallback 到 `getWorkingDir()`） |
| `agent/tools/bash.go` | 修改 | `cmd.Dir` 改为 `wdctx.Dir(ctx)` |
| `agent/tools/rg.go` | 修改 | `resolveSearchPath` 增加 ctx 参数，相对路径基于 `wdctx.Dir(ctx)` 解析 |
| `agent/tools/glob.go` | 修改 | 调用 `resolveSearchPath` 时传入 ctx |
| `agent/tools/grep.go` | 修改 | 同上 |
| `agent/tools/read.go` | 修改 | 相对路径通过 `filepath.Join(wdctx.Dir(ctx), path)` 解析 |
| `agent/tools/write.go` | 修改 | 同上 |
| `agent/tools/edit.go` | 修改 | 同上 |
| `agent/tools/subagent.go` | 修改 | `SubagentArgs` 新增 `WorktreeBranch`；`SubagentRunner` 接口改为 `RunSubagent(ctx, SubagentArgs)`；schema 新增 `worktree_branch` 参数 |
| `agent/subagent_worktree_test.go` | **新文件** | WorktreeManager 单元测试（含分支场景） |
| `agent/subagent_test.go` | 修改 | 适配新接口签名 + worktree 集成测试用例 |
| `docs/subagent-design.md` | 修改 | 新增 worktree 章节引用 |
| `docs/subagent-worktree-design.md` | **新文件** | 本文档 |

---

## 九、测试策略

### 单元测试

| 测试目标 | 内容 |
|---------|------|
| `WorktreeManager.Create` 正常 (detached) | worktree 存在 → 回调获取正确路径 → 回调后清理 |
| `WorktreeManager.Create` + 指定分支 | 分支检出成功 → 子 agent 在该分支上工作 |
| `WorktreeManager.Create` + 远程分支自动 fetch | 本地无分支 → fetch → 创建 worktree 成功 |
| `WorktreeManager.Create` + 分支不存在 | 降级到共享目录 + warning 日志 |
| `WorktreeManager.Create` + cleanup=false | worktree 保留不删除 |
| `WorktreeManager.Create` + 回调报错 | worktree 仍然被清理 |
| `WorktreeManager.Create` + git add 失败 | 降级到共享目录，返回结果正常 |
| `collectPatch` 无改动 | 返回空字符串，不追加 patch block |
| `collectPatch` 有改动 | 返回 `[WORKTREE_PATCH]...[/WORKTREE_PATCH]` 格式 |
| `collectPatch` 超大 patch | 截断到 maxPatchSize + 追加摘要信息 |
| `collectPatch` 含 untracked 文件 | `git add -A` 后 untracked 也被收入 patch |
| `WorktreeManager.Create` 并发 + 同分支 | 2 个 goroutine 指定相同 branch，均 `--detach` 成功互不冲突 |
| `WorktreeManager.Create` 并发 + 不同分支 | 2 个 goroutine 各自独立 worktree + 互不影响 |
| 分支优先级解析 | `args.WorktreeBranch` > `config.WorktreeBranch` > detached |
| `wdctx.WithDir/Dir` | context 传递 + fallback 到 `getWorkingDir()` |
| `SubagentExecutor.RunSubagent` worktree=true | child agent 在 worktree 运行 |
| `SubagentExecutor.RunSubagent` worktree=false | 行为不变 |
| Bash tool + context workdir | `cmd.Dir` 指向正确 |
| ReadFile/WriteFile/EditFile + context workdir | 相对路径基于 `wdctx.Dir(ctx)` 解析 |
| Glob/Grep + context workdir | `resolveSearchPath(ctx, ".")` 返回 worktree 路径 |
| 子 agent worktree 内 cd 后 | Bash cd 到子目录，后续 Glob/ReadFile 仍基于 worktree 正确解析 |
| `SubagentArgs` 新字段 | `worktree_branch` 正确序列化/反序列化 |

### 集成测试

- 真实 git 仓库中 "列出所有 `.go` 文件" → 验证 worktree 内容 = 主仓库
- `worktree_cleanup: false` → `/tmp` 保留 worktree 目录
- 脏工作区 → worktree 仍正常创建（不降级），日志输出 info 提示
- 两个并行子 agent 指定同一分支 → 均成功（`--detach` 避免冲突）
- 非 git 仓库目录 → 降级 + warning 日志

---

## 十、注意事项

| 问题 | 处理方式 |
|------|---------|
| 脏工作区 | 不影响 worktree 创建（detached HEAD 从 object store 签出，与工作区状态无关）。仅日志记录 info 提示 |
| 指定分支不存在 | auto `git fetch origin <branch>:<branch>`；仍失败则降级 |
| 并发 worktree 的 git 锁 | `git worktree add` 原子操作，安全 |
| 同一分支多个 worktree | 统一使用 `git worktree add --detach <path> <branch>` 而非 `git worktree add <path> <branch>`，避免 git "分支已被占用"的限制。detached 模式下同一分支可创建任意多个 worktree |
| Worktree 与 Git Hooks | 继承 hooks，pre-commit 等会正常触发（预期行为） |
| Push 到远程 | 子 agent 在 worktree 中可正常 push，需用户自行配置 remote |
| Git Submodule | 不处理，worktree 不会自动初始化子模块 |
| macOS/APFS 兼容 | hardlink 语义完全兼容 |

---

## 十一、未来扩展

1. **Worktree 保留模式**：完成后保留，主 agent 可手动检查
2. **自动 stash/unstash**：支持脏工作区创建 worktree
3. **基于 worktree 的并行测试**：`go test` 收集覆盖率
4. **多 commit worktree**：子 agent 在 worktree 中创建多个 commit，push 为新分支或 PR
5. **Worktree 资源池**：预创建 N 个 worktree 降低延迟
6. **子 agent 间 diff 对比**：两个在不同分支的 worktree 完成后，主 agent 对比 diff