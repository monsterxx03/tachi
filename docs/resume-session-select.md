# --resume session selection UI

## Problem

`tachi -r` / `tachi --resume` 自动恢复最近一个 session，用户无法选择其他 session。如果最近的不是想要的，必须先正常启动 tachi，再通过 `/sessions` 切换到目标 session — 多了一步，且冷启动时需要先等 TUI 完全就绪再执行 slash command，不够直观。

## Behavior

`tachi -r` 启动后直接进入 session 选择界面——与 `/sessions` 相同的 UI（`stateSelectingSession`）。列出所有历史 session，用户用 ↑↓ 选择，按 Enter 恢复目标 session，按 Esc 退出选择并回到正常 idle 状态。

- **启动即选择**：TUI 启动后不加载任何 session，直接展示 session 列表。
- **选择后恢复**：与现有 `/sessions` → Enter 行为完全一致 — `loadSession()` 加载消息历史、转换 LLM 格式、恢复 chatview、更新 statusbar。
- **Esc 退出**：不选择任何 session，退出到正常 idle 状态（等效于不加 `-r` 启动）。
- **空列表**：如果没有历史 session，显示 `No sessions found` 并回退到 idle。
- **非 TUI 模式不变**：`tachi run -r --prompt "..."` 保持原有行为（自动恢复最新 session），因为非交互模式没有 UI 来选择。

## Implementation notes

改动主要在两个文件：

### main.go (`runTUI`)

把原来 `--resume` 分支调用 `ResumeSession()` 的逻辑替换为：

```go
if cmd.Bool("resume") {
    // Create session manager early and pass session list to TUI
    sm, err := session.NewManager()
    if err != nil {
        return fmt.Errorf("session manager: %w", err)
    }
    sm.SetMaxKeep(cfg.SessionCleanupMaxCount)
    sm.CleanupOldSessions()
    aiAgent.SetSessionManager(sm)

    sessions, err := sm.List()
    if err != nil {
        fmt.Fprintf(os.Stderr, "Warning: failed to list sessions: %v\n", err)
    }
    // Pass session list to ModelConfig for initial selection UI
    initialSessionList = sessions
} else {
    // existing non-resume path
}
```

并在 `ModelConfig` 中传入 `InitialSessionList`。

### tui/model.go

- 给 `ModelConfig` 新增字段 `InitialSessionList []*session.Session`。
- `NewModel` 中：如果 `InitialSessionList` 非空，设置 `m.sessionList`、`m.sessionSelIdx = 0`、`m.clampSessionScroll()`，并将初始状态设为 `stateSelectingSession`。
- 如果列表为空，发一条 `No sessions to resume` 消息并保持 `stateIdle`。
- `loadSession` 无需修改 — 现有逻辑完全复用。

### 不改动的部分

- `agent.ResumeSession()` 方法保留不动 — `tachi run -r` 仍然使用它。
- `/sessions` 命令不受影响 — session 选择 UI 完全复用。
- `session.Manager` 和 `session.Store` 不变。
