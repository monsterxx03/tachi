# Cron Scheduler 设计文档

> 版本: 1.0 | 日期: 2026-05-11 | 状态: 设计阶段

## 一、概述

为 Tachi 添加全局 cron 调度能力，使 LLM 能通过 tool 创建/管理定时任务。cron 触发时自动向指定目标（当前仅 channel thread）发送 prompt 并执行 agent 对话。

**核心设计原则：**
- **全局独立**：cron 包独立于 channel，尽管当前唯一 consumer 是 channel
- **LLM 驱动**：通过 `Cron` tool 让 LLM 进行增删改查
- **动态热加载**：CRUD 操作实时生效，无需重启
- **可扩展**：通过 `TriggerHandler` 回调解耦执行逻辑，未来可支持更多目标

## 二、总体架构

```
┌─────────────────────────────────────────────────────────────────┐
│                        Agent (LLM)                               │
│                                                                  │
│  "帮我设置一个每天早上9点的日报提醒"                                   │
└───────────────────────────┬──────────────────────────────────────┘
                            │ calls Cron tool
                            ▼
┌─────────────────────────────────────────────────────────────────┐
│                     CronTool                                     │
│                  agent/tools/cron.go                              │
│                                                                  │
│  actions: list / create / get / update / delete / pause / resume │
│  args: { action, id?, name?, schedule?, prompt?, target_id? }    │
└───────────────────────────┬──────────────────────────────────────┘
                            │ calls Scheduler methods
                            ▼
┌─────────────────────────────────────────────────────────────────┐
│                     cron.Scheduler                                │
│                       cron/scheduler.go                           │
│                                                                  │
│  ┌──────────────┐    ┌──────────────┐    ┌──────────────────┐   │
│  │   JobStore    │    │   Timer Mgr  │    │  TriggerHandler  │   │
│  │  (persist)    │    │  (scheduling)│    │  (callback)      │   │
│  └──────┬───────┘    └──────┬───────┘    └────────┬─────────┘   │
│         │                   │                     │              │
│     crons.json         goroutines              channel          │
│    (~/.tachi/)        per active job           manager          │
└─────────────────────────────────────────────────────────────────┘
                                                    │
                                                    ▼
┌─────────────────────────────────────────────────────────────────┐
│                   Channel Manager                                 │
│                                                                  │
│  OnCronTrigger(ctx, job):                                        │
│    1. Find/create session for job.TargetThreadID                 │
│    2. Build agent with job.Prompt as user message                │
│    3. Run agent turn                                             │
│    4. Send response to channel thread                            │
└─────────────────────────────────────────────────────────────────┘
```

## 三、数据模型

### 3.1 Job 定义

```go
// cron/job.go
package cron

import "time"

type JobStatus string

const (
    JobStatusActive  JobStatus = "active"
    JobStatusPaused  JobStatus = "paused"
)

// Job represents a scheduled cron task.
type Job struct {
    // ID is a unique identifier (short UUID, e.g. "cr_a1b2c3").
    ID string `json:"id"`

    // Name is a human-readable label set by the LLM.
    Name string `json:"name"`

    // Schedule is a cron expression (supports standard 5-field + @every/@daily etc).
    // 例: "0 9 * * *" (每天9点), "*/30 * * * *" (每30分钟), "@every 2h"
    Schedule string `json:"schedule"`

    // Prompt is the message sent to the LLM when the cron fires.
    // This becomes the "user message" in the agent conversation.
    Prompt string `json:"prompt"`

    // TargetType identifies the consumer type. Currently only "channel".
    // Future: "webhook", "tui-notification", etc.
    TargetType string `json:"target_type"`

    // TargetThreadID is the channel thread to send the response to.
    // Required when TargetType == "channel".
    TargetThreadID string `json:"target_thread_id"`

    // Status controls whether the job is actively scheduled.
    Status JobStatus `json:"status"`

    // Timezone for schedule evaluation (default: system local).
    // IANA timezone string, e.g. "Asia/Shanghai", "UTC".
    Timezone string `json:"timezone,omitempty"`

    // MaxRetries is how many times to retry on execution failure (default: 0).
    MaxRetries int `json:"max_retries,omitempty"`

    // CreatedAt is when the job was created.
    CreatedAt time.Time `json:"created_at"`

    // UpdatedAt is when the job was last modified.
    UpdatedAt time.Time `json:"updated_at"`

    // LastRunAt is when the job last fired (zero if never).
    LastRunAt time.Time `json:"last_run_at,omitempty"`

    // LastRunStatus records the outcome of the last execution.
    LastRunStatus string `json:"last_run_status,omitempty"` // "success", "error"

    // LastRunError records the error message if LastRunStatus == "error".
    LastRunError string `json:"last_run_error,omitempty"`

    // CreatedBy records which thread/session created this job (for auditing).
    CreatedBy string `json:"created_by,omitempty"`
}
```

### 3.2 持久化

存储位置：`~/.tachi/crons.json`

```json
{
  "jobs": [
    {
      "id": "cr_a1b2c3",
      "name": "每日早报",
      "schedule": "0 9 * * 1-5",
      "prompt": "请总结一下今天的技术新闻热点，用简洁的中文回复",
      "target_type": "channel",
      "target_thread_id": "wxid_xxx",
      "status": "active",
      "timezone": "Asia/Shanghai",
      "created_at": "2026-05-11T10:00:00+08:00",
      "updated_at": "2026-05-11T10:00:00+08:00"
    }
  ]
}
```

选择 JSON 而非 YAML 的原因：
- cron 数据是程序管理的（通过 tool），不需要人工编辑
- JSON 的原子读写更简单可靠
- 与 session 的 `.jsonl` 风格一致

## 四、核心模块设计

### 4.1 Store（持久化层）

```go
// cron/store.go
package cron

import (
    "encoding/json"
    "os"
    "sync"
)

// Store handles cron job persistence.
// Thread-safe: all methods acquire a mutex.
type Store struct {
    mu   sync.RWMutex
    path string // ~/.tachi/crons.json
}

type storeData struct {
    Jobs []*Job `json:"jobs"`
}

func NewStore(path string) *Store

// CRUD operations — all return new copies, never internal pointers.
func (s *Store) List() ([]*Job, error)
func (s *Store) Get(id string) (*Job, error)
func (s *Store) Create(job *Job) error
func (s *Store) Update(job *Job) error
func (s *Store) Delete(id string) error

// Save writes atomically (write tmp → rename).
func (s *Store) save(data *storeData) error
```

### 4.2 Scheduler（调度器）

```go
// cron/scheduler.go
package cron

import (
    "context"
    "sync"
)

// TriggerHandler is the callback invoked when a cron job fires.
// It receives the Job and should execute the prompt against the target.
// Implementations must be safe for concurrent use.
type TriggerHandler func(ctx context.Context, job *Job) error

// Scheduler manages the lifecycle of cron jobs.
// It wraps a Store for persistence and maintains per-job goroutines
// for timing.
type Scheduler struct {
    store   *Store
    handler TriggerHandler
    logger  *debuglog.Logger

    mu      sync.Mutex
    timers  map[string]context.CancelFunc // jobID → cancel func for its goroutine
    ctx     context.Context
    cancel  context.CancelFunc
}

// NewScheduler creates a scheduler. handler is called on each trigger.
// Call Start() to begin scheduling existing jobs.
func NewScheduler(store *Store, handler TriggerHandler) *Scheduler

// Start loads all active jobs from store and starts their timers.
// Should be called once at startup after the handler is ready.
func (s *Scheduler) Start(ctx context.Context) error

// Stop cancels all active timers and waits for in-flight triggers.
func (s *Scheduler) Stop()

// --- CRUD (used by CronTool) ---
// Each method persists changes AND updates the in-memory timer state.

func (s *Scheduler) List() ([]*Job, error)
func (s *Scheduler) Get(id string) (*Job, error)
func (s *Scheduler) Create(job *Job) (*Job, error)   // assigns ID, starts timer
func (s *Scheduler) Update(id string, opts UpdateOpts) (*Job, error) // reschedules if needed
func (s *Scheduler) Delete(id string) error           // stops timer, removes from store
func (s *Scheduler) Pause(id string) (*Job, error)    // stops timer, sets status=paused
func (s *Scheduler) Resume(id string) (*Job, error)   // restarts timer, sets status=active

type UpdateOpts struct {
    Name     *string
    Schedule *string
    Prompt   *string
    Timezone *string
}
```

### 4.3 调度实现细节

```go
// 每个 active job 启动一个 goroutine，使用 time.Timer 等待下次触发时间。
// 选择 Timer 而非 Ticker 的原因：cron 表达式需要精确计算下次触发时间。
//
// 依赖: github.com/robfig/cron/v3 (仅用其 cron expression parser)
// 不使用 robfig/cron 的调度器本身 — 我们需要更细粒度的控制（per-job lifecycle）。

func (s *Scheduler) startJobTimer(job *Job) {
    // 1. Parse cron expression
    // 2. Calculate next fire time (with timezone)
    // 3. Launch goroutine:
    //    for {
    //      select {
    //      case <-timer.C:
    //          s.handler(ctx, job)
    //          update LastRunAt/Status in store
    //          recalculate next fire time, reset timer
    //      case <-jobCtx.Done():
    //          return
    //      }
    //    }
}
```

**为什么不直接用 `robfig/cron` 的 `Cron` 调度器：**
1. 我们需要 per-job 独立的 goroutine lifecycle（动态增删）
2. 需要在触发时更新 store 中的 `LastRunAt`
3. 需要支持 per-job timezone
4. robfig/cron v3 的 parser 仍然有价值（解析 cron 表达式、计算 next time）

**替代方案：直接用 robfig/cron**

实际上再想想，robfig/cron v3 本身就支持：
- 动态 `AddJob` / `Remove`
- Per-entry timezone
- Thread-safe

所以**更简单的方案**是直接用 `robfig/cron` 的调度器，封装一层：

```go
import "github.com/robfig/cron/v3"

type Scheduler struct {
    store   *Store
    handler TriggerHandler
    logger  *debuglog.Logger
    cron    *cron.Cron

    mu       sync.Mutex
    entryMap map[string]cron.EntryID // jobID → cron entry ID
}

func (s *Scheduler) Create(job *Job) (*Job, error) {
    // 1. Validate & persist to store
    // 2. entryID, _ := s.cron.AddFunc(job.Schedule, func() { s.trigger(job) })
    // 3. s.entryMap[job.ID] = entryID
}

func (s *Scheduler) Delete(id string) error {
    // 1. Remove from store
    // 2. s.cron.Remove(s.entryMap[id])
    // 3. delete(s.entryMap, id)
}
```

**最终选择：使用 robfig/cron v3 作为底层调度引擎**，封装 Scheduler 提供业务语义。这是更务实的选择，减少自己实现 timer 管理的 bug 风险。

## 五、CronTool 设计

### 5.1 Tool Schema

```go
// agent/tools/cron.go
package tools

const ToolNameCron = "Cron"

type CronTool struct {
    scheduler *cron.Scheduler
    // threadID provider: in channel mode, provides current thread ID for auto-fill
    threadIDFunc func() string
}

func (t *CronTool) Name() string        { return ToolNameCron }
func (t *CronTool) Parallel() bool      { return false } // mutations should be sequential
func (t *CronTool) Description() string {
    return `Manage scheduled cron jobs. Jobs automatically trigger at the specified schedule and send the prompt to the target thread.

Actions:
- list: List all cron jobs
- create: Create a new cron job (requires: name, schedule, prompt)
- get: Get details of a specific job (requires: id)
- update: Update an existing job (requires: id, plus fields to change)
- delete: Delete a job (requires: id)
- pause: Pause a job (requires: id)
- resume: Resume a paused job (requires: id)`
}

func (t *CronTool) Properties() map[string]PropertySchema {
    return map[string]PropertySchema{
        "action": {
            Type:        "string",
            Description: "The action to perform: list, create, get, update, delete, pause, resume",
        },
        "id": {
            Type:        "string",
            Description: "Job ID (required for get/update/delete/pause/resume)",
        },
        "name": {
            Type:        "string",
            Description: "Human-readable job name",
        },
        "schedule": {
            Type:        "string",
            Description: `Cron expression. Standard 5-field (minute hour day month weekday) or predefined: @yearly, @monthly, @weekly, @daily, @hourly, @every <duration>. Examples: "0 9 * * 1-5" (weekdays 9am), "*/30 * * * *" (every 30min), "@every 2h"`,
        },
        "prompt": {
            Type:        "string",
            Description: "The prompt to send to the LLM when the job triggers",
        },
        "timezone": {
            Type:        "string",
            Description: "IANA timezone for schedule evaluation (default: system local). Example: Asia/Shanghai, UTC",
        },
    }
}

func (t *CronTool) Required() []string {
    return []string{"action"}
}
```

### 5.2 执行逻辑

```go
func (t *CronTool) ExecuteContext(ctx context.Context, args string) (string, error) {
    var params struct {
        Action   string `json:"action"`
        ID       string `json:"id"`
        Name     string `json:"name"`
        Schedule string `json:"schedule"`
        Prompt   string `json:"prompt"`
        Timezone string `json:"timezone"`
    }
    json.Unmarshal([]byte(args), &params)

    switch params.Action {
    case "list":
        jobs, _ := t.scheduler.List()
        // Format as readable table/JSON for LLM
        return formatJobList(jobs), nil

    case "create":
        job := &cron.Job{
            Name:           params.Name,
            Schedule:       params.Schedule,
            Prompt:         params.Prompt,
            Timezone:       params.Timezone,
            TargetType:     "channel",
            TargetThreadID: t.threadIDFunc(), // auto-fill current thread
        }
        created, err := t.scheduler.Create(job)
        if err != nil {
            return "", err
        }
        return fmt.Sprintf("✅ Created cron job: %s (ID: %s)\nSchedule: %s\nNext run: %s",
            created.Name, created.ID, created.Schedule, nextRunTime(created)), nil

    case "get":
        job, err := t.scheduler.Get(params.ID)
        // Return detailed view

    case "update":
        opts := cron.UpdateOpts{}
        if params.Name != "" { opts.Name = &params.Name }
        if params.Schedule != "" { opts.Schedule = &params.Schedule }
        if params.Prompt != "" { opts.Prompt = &params.Prompt }
        if params.Timezone != "" { opts.Timezone = &params.Timezone }
        updated, err := t.scheduler.Update(params.ID, opts)
        // Return updated view

    case "delete":
        err := t.scheduler.Delete(params.ID)
        return "✅ Job deleted", err

    case "pause":
        job, err := t.scheduler.Pause(params.ID)
        // Return paused confirmation

    case "resume":
        job, err := t.scheduler.Resume(params.ID)
        // Return resumed confirmation + next run time
    }
}
```

### 5.3 Tool 注册时机

CronTool 仅在 cron scheduler 可用时注册（即 channel 模式下启动了 scheduler）。TUI 模式下不注册此 tool。

```go
// channel/manager.go — 在 process() 中
aiAgent.RegisterTool(tools.NewCronTool(m.scheduler, func() string {
    return msg.ThreadID
}))
```

### 5.4 LLM 交互示例

**用户**: "帮我设一个每周一早上9点的周报提醒"

**LLM 调用**:
```json
{
  "action": "create",
  "name": "周报提醒",
  "schedule": "0 9 * * 1",
  "prompt": "现在是周一早上9点，请提醒我写本周的周报。总结一下上周可能做过的事情，帮我列一个周报大纲。",
  "timezone": "Asia/Shanghai"
}
```

**Tool 返回**:
```
✅ Created cron job: 周报提醒 (ID: cr_x7k9m2)
Schedule: 0 9 * * 1 (每周一 09:00)
Timezone: Asia/Shanghai
Next run: 2026-05-18 09:00:00 CST
Target: current thread
```

## 六、集成设计

### 6.1 启动流程

```
main.go
  └── channel mode?
        ├── cronStore = cron.NewStore("~/.tachi/crons.json")
        ├── scheduler = cron.NewScheduler(cronStore, channelManager.OnCronTrigger)
        ├── scheduler.Start(ctx)
        ├── channelManager.SetScheduler(scheduler)  // for CronTool injection
        └── channelManager.Start(ctx)
```

### 6.2 Channel Manager 集成

```go
// channel/manager.go

type Manager struct {
    // ... existing fields ...
    scheduler *cron.Scheduler  // nil if cron not configured
}

// SetScheduler injects the cron scheduler. Must be called before Start().
func (m *Manager) SetScheduler(s *cron.Scheduler) {
    m.scheduler = s
}

// OnCronTrigger is the TriggerHandler callback.
// It simulates an incoming message from the cron system.
func (m *Manager) OnCronTrigger(ctx context.Context, job *cron.Job) error {
    m.logger.Log("cron: trigger job=%s thread=%s", job.ID, job.TargetThreadID)

    // Reuse the existing message processing pipeline.
    // The prompt becomes the "user message".
    msg := IncomingMessage{
        ThreadID:  job.TargetThreadID,
        MessageID: fmt.Sprintf("cron_%s_%d", job.ID, time.Now().Unix()),
        Content:   job.Prompt,
        ChannelID: "__cron__",  // special marker for logging
    }

    result, err := m.process(ctx, msg)
    if err != nil {
        m.logger.Log("cron: job %s failed: %v", job.ID, err)
        return err
    }

    // Deliver the response — find the channel that owns this thread
    // and send the outgoing message.
    m.deliverCronResponse(ctx, OutgoingMessage{
        ThreadID: job.TargetThreadID,
        Content:  result,
        ReplyTo:  msg.MessageID,
    })
    return nil
}

// deliverCronResponse sends a cron-triggered response to the appropriate channel.
func (m *Manager) deliverCronResponse(ctx context.Context, msg OutgoingMessage) {
    // For now, broadcast to all channels. Each channel implementation
    // should check if the ThreadID belongs to it and deliver or ignore.
    // Future: maintain a thread→channel mapping.
    m.mu.Lock()
    chans := make([]Channel, len(m.channels))
    copy(chans, m.channels)
    m.mu.Unlock()

    for _, ch := range chans {
        if sender, ok := ch.(MessageSender); ok {
            if err := sender.Send(ctx, msg); err != nil {
                m.logger.Log("cron: send to %s failed: %v", ch.Name(), err)
            }
        }
    }
}
```

### 6.3 Channel 接口扩展

```go
// channel/channel.go — 新增可选接口

// MessageSender is an optional interface for channels that support
// proactive message delivery (not just request-response).
// Required for cron and future push notification features.
type MessageSender interface {
    // Send delivers a message to the specified thread.
    // Returns error if the thread is unknown or delivery fails.
    Send(ctx context.Context, msg OutgoingMessage) error
}
```

### 6.4 process() 中注册 CronTool

```go
// channel/manager.go — 在 process() 中，agent 配置完成后

func (m *Manager) process(ctx context.Context, msg IncomingMessage) (string, error) {
    // ... existing setup ...

    // Register CronTool if scheduler is available.
    if m.scheduler != nil {
        aiAgent.RegisterTool(tools.NewCronTool(m.scheduler, func() string {
            return msg.ThreadID
        }))
    }

    // ... rest of process ...
}
```

## 七、配置

### 7.1 config.yaml 新增字段

```yaml
# Cron scheduler configuration (optional).
# Only active in channel mode.
cron:
  enabled: true           # default: true (when channel mode is active)
  store_path: ""          # default: ~/.tachi/crons.json
  max_concurrent: 3       # max concurrent cron job executions (default: 3)
  execution_timeout: 5m   # max time for a single cron job execution (default: 5m)
```

```go
// config/config.go

type CronConfig struct {
    Enabled          *bool         `yaml:"enabled" default:"true"`
    StorePath        string        `yaml:"store_path"`           // empty = default
    MaxConcurrent    int           `yaml:"max_concurrent" default:"3"`
    ExecutionTimeout time.Duration `yaml:"execution_timeout" default:"5m"`
}

type Config struct {
    // ... existing fields ...
    Cron CronConfig `yaml:"cron"`
}
```

## 八、动态加载机制

### 8.1 核心要求

"动态加载"意味着：
1. **CRUD 即时生效**：通过 CronTool 创建/修改/删除 job 后，调度器立即更新，无需重启
2. **持久化**：所有变更写入磁盘，进程重启后自动恢复
3. **多 thread 安全**：不同 channel thread 可能同时操作 cron

### 8.2 实现保证

```go
// Scheduler 的 CRUD 方法遵循 "persist-then-schedule" 模式：
//
//   Create(job):
//     1. Validate cron expression (fail fast)
//     2. store.Create(job)         ← 持久化
//     3. cron.AddFunc(...)         ← 内存调度
//     4. return job
//
//   Update(id, opts):
//     1. store.Get(id)             ← 读取当前状态
//     2. Apply opts
//     3. If schedule changed: validate new expression
//     4. store.Update(job)         ← 持久化
//     5. If schedule changed:
//        cron.Remove(old entry)
//        cron.AddFunc(new schedule) ← 重新调度
//     6. return job
//
//   Delete(id):
//     1. cron.Remove(entry)        ← 停止调度
//     2. store.Delete(id)          ← 持久化
//
// 错误处理: 如果持久化成功但内存调度失败（理论上不应该），
// 下次 Start() 时会从 store 恢复正确状态。
```

### 8.3 启动时恢复

```go
func (s *Scheduler) Start(ctx context.Context) error {
    jobs, err := s.store.List()
    if err != nil {
        return fmt.Errorf("load cron jobs: %w", err)
    }

    for _, job := range jobs {
        if job.Status != JobStatusActive {
            continue
        }
        if err := s.scheduleJob(job); err != nil {
            s.logger.Log("cron: failed to schedule job %s: %v", job.ID, err)
            // Log but don't fail — other jobs can still run.
        }
    }

    s.cron.Start()
    s.logger.Log("cron: started with %d active jobs", len(s.entryMap))
    return nil
}
```

## 九、文件结构

```
tachi/
├── cron/                          # 新增: 全局 cron 包
│   ├── job.go                     # Job 结构体定义
│   ├── store.go                   # JSON 持久化层
│   ├── scheduler.go               # 调度器 (wraps robfig/cron)
│   └── scheduler_test.go          # 调度器测试
├── agent/
│   └── tools/
│       └── cron.go                # 新增: CronTool (LLM 接口)
├── channel/
│   ├── channel.go                 # 扩展: MessageSender interface
│   └── manager.go                 # 扩展: SetScheduler, OnCronTrigger
├── config/
│   └── config.go                  # 扩展: CronConfig
└── main.go                        # 扩展: cron 初始化逻辑
```

## 十、并发与错误处理

### 10.1 并发控制

```go
// Scheduler 通过 semaphore 限制同时执行的 cron job 数量
type Scheduler struct {
    // ...
    sem chan struct{} // buffered channel, cap = MaxConcurrent
}

func (s *Scheduler) trigger(job *Job) {
    // Acquire semaphore (non-blocking: skip if at capacity)
    select {
    case s.sem <- struct{}{}:
        defer func() { <-s.sem }()
    default:
        s.logger.Log("cron: skipping job %s (max concurrent reached)", job.ID)
        return
    }

    ctx, cancel := context.WithTimeout(s.ctx, s.executionTimeout)
    defer cancel()

    err := s.handler(ctx, job)

    // Update last run status
    s.mu.Lock()
    job.LastRunAt = time.Now()
    if err != nil {
        job.LastRunStatus = "error"
        job.LastRunError = err.Error()
    } else {
        job.LastRunStatus = "success"
        job.LastRunError = ""
    }
    s.store.Update(job)
    s.mu.Unlock()
}
```

### 10.2 错误场景

| 场景 | 处理方式 |
|------|---------|
| cron expression 无效 | Create/Update 时立即返回错误给 LLM |
| 目标 thread 不存在 | handler 中 create new session |
| 执行超时 | context deadline exceeded，记录 error |
| channel 发送失败 | log error，job 本身仍标记 success (prompt 已处理) |
| 持久化写入失败 | 返回错误给调用方，内存状态不变 |
| 进程崩溃恢复 | Start() 从 store 重建所有 active jobs |

### 10.3 防抖/幂等

- 同一个 job 不会并发触发：robfig/cron 保证每个 entry 的 job func 串行执行
- 如果上一次执行还在运行（超过了间隔），新触发会等待（robfig/cron 的 `DelayIfStillRunning` wrapper）

## 十一、安全考虑

1. **Prompt 注入**：cron 的 prompt 由 LLM 自己设定，通过正常的 tool 调用路径，风险等同于正常对话
2. **资源耗尽**：通过 `max_concurrent` 和 `execution_timeout` 限制
3. **滥用**：可设置全局 job 数量上限（默认 50）
4. **审计**：每个 job 记录 `CreatedBy`（thread ID），可追踪来源

## 十二、未来扩展

1. **更多 TargetType**：
   - `webhook`: HTTP POST 到指定 URL
   - `tui-notification`: TUI 模式下弹出通知
   - `email`: 发送邮件

2. **条件触发**：
   - 除了时间触发，支持事件触发（如某个 MCP 数据源变化时）

3. **Job 模板**：
   - 预置常用 job（日报、周报、代码审查提醒等）

4. **执行历史**：
   - 记录每次执行的详细日志到独立文件
   - `/cron history <id>` 查看

5. **Web Dashboard**：
   - 通过 `/cron` slash command 在 channel 中管理
   - 列表、暂停、恢复、删除

## 十三、依赖

| 依赖 | 用途 | 备注 |
|------|------|------|
| `github.com/robfig/cron/v3` | cron expression 解析 + 调度 | 成熟稳定，Go 生态标准选择 |
| `github.com/google/uuid` | Job ID 生成 | 已在项目中使用 |

## 十四、实现路线图

### Phase 1: 基础能力 (MVP)
- [ ] `cron/` 包：Job, Store, Scheduler
- [ ] `agent/tools/cron.go`：CronTool (list/create/delete)
- [ ] Channel Manager 集成：SetScheduler, OnCronTrigger
- [ ] `MessageSender` 接口 + weixin 实现
- [ ] config 新增 CronConfig
- [ ] main.go 初始化

### Phase 2: 完善
- [ ] pause/resume/update 支持
- [ ] execution_timeout + max_concurrent
- [ ] LastRunAt/Status 跟踪
- [ ] `/cron` slash command (list/delete in channel)

### Phase 3: 增强
- [ ] 执行历史记录
- [ ] 更多 TargetType
- [ ] Job 数量上限
- [ ] 时区验证与友好提示
