# ReadFile 重复读取去重计划

## 问题

LLM 在一个会话内多次使用 `ReadFile` 读取同一文件（且文件内容未变），每次都返回完整内容，浪费 context window token。

## 目标

`ReadFile` 跟踪已读取文件的 mtime，当检测到重复读取时，返回简短提示告知 LLM "文件未变，请参考之前的结果"，不再返回完整文件内容。

## 设计决策

### 状态归属：ReadTool 自维护缓存

`ReadTool` 从零值 struct 改为持有内部状态：

```go
type ReadTool struct {
    mu    sync.RWMutex
    cache map[string]cachedRead
}
```

**为什么不放在 Agent 上：**
- Agent 不关心单个工具的细粒度缓存
- 放在 Tool 内部更内聚，改造成本低
- 每个 Agent 实例会在 `RegisterTools()` 时创建自己的 `ReadTool`，自动随 Agent 生命周期消亡
- 会话 resume 时创建新 Agent → 新 `ReadTool` → 空缓存，这是合理行为（LLM 首次读到文件后会缓存后续重复）

**为什么不用 context 传递：**
- context 中传递可变状态是反模式
- 并行执行时 context 是共享的，需要额外同步

### 缓存 Key

Key 为 `path + offset + limit` 三元组，因为不同 offset/limit 返回的是不同内容：

```go
func cacheKey(path string, offset, limit int) string {
    return fmt.Sprintf("%s|%d|%d", path, offset, limit)
}
```

举例：
- 读整个文件 `("/foo/bar.md", 0, 0)` → key: `/foo/bar.md|0|0`
- 读前10行 `("/foo/bar.md", 1, 10)` → key: `/foo/bar.md|1|10`
- 读第50行起 `("/foo/bar.md", 50, 0)` → key: `/foo/bar.md|50|0`

### 缓存命中时返回什么

返回简短提示（不包含文件内容）：

```
[File unchanged since last read (mtime: 2026-05-08T10:30:00+08:00). 
 The content is identical to your earlier ReadFile result for this path. 
 Please refer to your previous tool call output.]
```

LLM 的消息历史中已经有之前的 `tool_result`，所以它可以回溯到之前的输出。这大幅节省 token。

### 缓存失效条件

以下情况不命中缓存，重新读取并更新缓存：
1. 文件 mtime 变化（用户或外部进程修改了文件）
2. 文件大小变化（快速检测，避免读后再比对）
3. 文件不存在之前的 stat 出错
4. 首次读取该 (path, offset, limit) 组合

### 线程安全

`ReadTool.Parallel()` 返回 `true`，可能在 `executeToolCallsParallel` 中被并发调用。使用 `sync.RWMutex`：
- 查询缓存（读操作）使用 `RLock`
- 更新缓存（写操作）使用 `Lock`

## 实现步骤

### Step 1: 修改 ReadTool 结构体

**文件：** `agent/tools/read.go`

增加缓存字段和构造函数：

```go
type cachedEntry struct {
    mtime  time.Time
    size   int64
    result string // 首次读取的结果（用于填充 session 记录）
}

type ReadTool struct {
    mu    sync.RWMutex
    cache map[string]cachedEntry
}

func NewReadTool() ReadTool {
    return ReadTool{
        cache: make(map[string]cachedEntry),
    }
}
```

需新增 import：`"sync"` `"time"`

### Step 2: 修改 ExecuteContext

在 `ExecuteContext` 中插入缓存检查逻辑，流程如下：

```
ExecuteContext(args):
  1. 解析 args (path, offset, limit)
  2. 安全校验（blocked device, size 等）
  3. stat 文件（获取 mtime, size）
  4. 计算 cacheKey
  5. 读锁查询缓存
     - 命中（mtime 和 size 都匹配）→ 返回简短提示
     - 未命中 → 继续
  6. 实际读取文件内容
  7. 写锁更新缓存 (cacheKey → {mtime, size})
  8. 返回内容
```

伪代码：

```go
func (t ReadTool) ExecuteContext(ctx context.Context, args string) (string, error) {
    // ... 现有解析和校验逻辑 ...

    info, err := os.Stat(argsMap.Path)
    // ... 现有错误处理 ...

    key := cacheKey(argsMap.Path, argsMap.Offset, argsMap.Limit)

    // 检查缓存
    t.mu.RLock()
    if cached, ok := t.cache[key]; ok {
        if cached.mtime.Equal(info.ModTime()) && cached.size == info.Size() {
            t.mu.RUnlock()
            return formatCacheHit(info.ModTime()), nil
        }
    }
    t.mu.RUnlock()

    // 实际读取文件
    content, err := os.ReadFile(argsMap.Path)
    // ... 现有错误处理 ...

    // 构造结果（和现有逻辑一样：处理 offset/limit）
    result := strings.Join(lines[start:end], "\n")

    // 更新缓存
    t.mu.Lock()
    t.cache[key] = cachedEntry{
        mtime:  info.ModTime(),
        size:   info.Size(),
        result: result,
    }
    t.mu.Unlock()

    return result, nil
}
```

### Step 3: 添加辅助函数

```go
func cacheKey(path string, offset, limit int) string {
    return fmt.Sprintf("%s|%d|%d", path, offset, limit)
}

func formatCacheHit(mtime time.Time) string {
    return fmt.Sprintf(
        "[File unchanged since last read (mtime: %s). "+
        "The content is identical to your earlier ReadFile result for this path. "+
        "Please refer to your previous tool call output.]",
        mtime.Format("2006-01-02T15:04:05-07:00"),
    )
}
```

### Step 4: 更新 RegisterTools

**文件：** `agent/agent.go`

将 `tools.ReadTool{}` 改为 `tools.NewReadTool()`：

```go
func (a *AIAgent) RegisterTools() {
    a.toolRegistry.Register(tools.NewReadTool())  // 之前: tools.ReadTool{}
    // ... 其余不变
}
```

### Step 5: 更新现有测试

**文件：** `agent/tools/read_test.go`

现有测试使用 `ReadTool{}` 创建工具实例。改为 `NewReadTool()`：

```go
tool := NewReadTool()  // 之前: ReadTool{}
```

增加新的去重测试：

```go
func TestReadToolCachedHit(t *testing.T) {
    tool := NewReadTool()
    
    content := "Hello, World!"
    os.WriteFile("/tmp/test_read_cache.txt", []byte(content), 0644)
    defer os.Remove("/tmp/test_read_cache.txt")
    
    // 首次读取
    result1, err := tool.ExecuteContext(nil, `{"path": "/tmp/test_read_cache.txt"}`)
    assert.NoError(t, err)
    assert.Equal(t, content, result1)
    
    // 二次读取（文件未变）
    result2, err := tool.ExecuteContext(nil, `{"path": "/tmp/test_read_cache.txt"}`)
    assert.NoError(t, err)
    assert.Contains(t, result2, "File unchanged since last read")
    assert.NotContains(t, result2, content) // 不包含完整内容
}

func TestReadToolCacheMissAfterModify(t *testing.T) {
    tool := NewReadTool()
    
    content1 := "Hello"
    os.WriteFile("/tmp/test_read_modify.txt", []byte(content1), 0644)
    defer os.Remove("/tmp/test_read_modify.txt")
    
    // 首次读取
    tool.ExecuteContext(nil, `{"path": "/tmp/test_read_modify.txt"}`)
    
    // 修改文件（等待足够时间确保 mtime 变化）
    time.Sleep(10 * time.Millisecond)
    content2 := "Hello, World!"
    os.WriteFile("/tmp/test_read_modify.txt", []byte(content2), 0644)
    
    // 再次读取 — 应返回新内容
    result, err := tool.ExecuteContext(nil, `{"path": "/tmp/test_read_modify.txt"}`)
    assert.NoError(t, err)
    assert.Equal(t, content2, result)
}

func TestReadToolDifferentOffsetNotCached(t *testing.T) {
    tool := NewReadTool()
    
    // 10 行文件
    lines := make([]string, 10)
    for i := range lines {
        lines[i] = fmt.Sprintf("line%d", i+1)
    }
    content := strings.Join(lines, "\n")
    os.WriteFile("/tmp/test_read_offset_cache.txt", []byte(content), 0644)
    defer os.Remove("/tmp/test_read_offset_cache.txt")
    
    // 读全部
    tool.ExecuteContext(nil, `{"path": "/tmp/test_read_offset_cache.txt"}`)
    
    // 读 offset=5 — 不同 key，不应命中缓存
    result, err := tool.ExecuteContext(nil, `{"path": "/tmp/test_read_offset_cache.txt", "offset": 5}`)
    assert.NoError(t, err)
    assert.Equal(t, "line5\nline6\nline7\nline8\nline9\nline10", result)
}
```

### Step 6: 并发安全测试

```go
func TestReadToolConcurrentCache(t *testing.T) {
    tool := NewReadTool()
    
    content := "concurrent test"
    os.WriteFile("/tmp/test_read_concurrent.txt", []byte(content), 0644)
    defer os.Remove("/tmp/test_read_concurrent.txt")
    
    var wg sync.WaitGroup
    for i := 0; i < 10; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            result, err := tool.ExecuteContext(nil, `{"path": "/tmp/test_read_concurrent.txt"}`)
            assert.NoError(t, err)
            assert.NotEmpty(t, result)
        }()
    }
    wg.Wait()
}
```

## 影响范围

| 文件 | 变更类型 |
|------|----------|
| `agent/tools/read.go` | 核心修改：加缓存逻辑、构造函数 |
| `agent/tools/read_test.go` | 测试适配 + 新增测试用例 |
| `agent/agent.go` | 一行改动：`ReadTool{}` → `NewReadTool()` |

## 边界情况

1. **文件被删除后重读**：`os.Stat()` 失败 → 不命中缓存 → 返回错误（现有行为）
2. **文件被另一个进程修改**：mtime 变化 → 自动失效，重新读取
3. **文件在大文件检查中被拒绝**：不会进入缓存
4. **二进制文件**：不会进入缓存（在读取前就被拒绝）
5. **读取 block device 被拒绝**：不会进入缓存
6. **Session resume**：新 Agent → 新 ReadTool → 空缓存，LLM 首次看到文件后缓存生效
7. **Channel 模式**：每个消息创建新 Agent，缓存不跨消息共享（这是合理行为，因为频道消息之间会话可能很长）

## Token 节省估算

假设一个典型的 200 行 Go 文件（约 5KB），在一个会话中被 LLM 读取 3 次：
- **无缓存**：5KB × 3 = 15KB tool result
- **有缓存**：5KB × 1 + ~150B × 2 ≈ 5.3KB
- **节省**：约 65%

对于更大的文件（接近 256KB 上限），节省效果更加显著。

## 备选方案（已排除）

### 方案 A：返回缓存内容（不推荐）

缓存命中时仍然返回文件内容，但在前面加提示。这没有实际节省 token，只是增加了元信息噪音。

### 方案 B：Agent 级缓存映射

在 `AIAgent` 上维护 `readFileCache`，通过 context 或字段传递给 `ReadTool`。增加了 Agent 和 Tool 之间的耦合，且 `ReadTool` 需要额外的接口来接收缓存。

### 方案 C：单独的去重后处理器

在 `executeToolCalls` 中增加一个后处理步骤，检查 tool result 是否与之前的 result 重复。这会修改热路径代码，且需要维护额外的去重逻辑，不如在 Tool 内部解决来得干净。
