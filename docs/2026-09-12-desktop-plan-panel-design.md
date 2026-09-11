# Desktop Plan 面板设计

> 版本: 0.2 | 日期: 2026-09-12 | 状态: P1（显示）/ P2（plan 模式）已实现；多份计划与清理已实现
> 关联: [desktop/plan.go](../desktop/plan.go)、[desktop/frontend/src/plan.tsx](../desktop/frontend/src/plan.tsx)、
>       [agent/tools/plan.go](../agent/tools/plan.go)、[agent/systemreminder/](../agent/systemreminder/)、
>       [2026-09-11-desktop-diff-review-design.md](./2026-09-11-desktop-diff-review-design.md)

## 本版修订（0.1 → 0.2：多份计划）

一个会话会攒下多份计划（每个计划一个文件），0.1 只显示最新的那份、其余缩成一句计数——等于"看不见"，
而重命名标题就会分叉出第二份。四处补齐：

1. **身份从标题改成 `plan_id`**（可选参数）：模型第一次保存时为这个计划起一个 id，之后每次更新都带上它，
   **改名也原地更新**。没有 id 的调用保持历史行为（按标题 slug 建文件）。文件名不变（改名不重命名文件：
   新名字可能撞上另一份计划的标题，而名字不是任何 UI 展示的东西——标题才是）。
   **id 要在三个地方告诉模型**，缺一个都会让它"看不见"这个字段：① 工具 schema 的 `plan_id` 描述；
   ② plan 模式的 system prompt；③ **plan-tracking reminder**——后者是 auto 模式下唯一的通道，
   它现在会带上当前计划的 id（没有 id 的老计划则提示"下一次更新顺便定一个"）。
   少了 ③ 的后果比原问题更糟：schema 要求给 id，而模型每次都可能新编一个 → **每次更新都生成新文件**。
2. **自动清理**（在保存后跑，作用域严格限定在本会话的文件）：本会话**其它已完成**的计划移到
   `<plans>/archive/`；本会话超过 **30 天**的计划（含归档区）删除。归档让"可以激进"成立——先收起来，
   很多天后才真删。计数写进工具结果，模型能看见发生了什么。
3. **面板列出全部计划**（`PlanVO.Plans`，仅当 >1 份时渲染）：标题 + N/M + 时间，点一行切换查看，
   每行一个删除入口。
4. **手动删除**：`DeletePlan(sessionID, path)`，删除前先校验 path **确实属于本会话**——
   这个绑定接收来自前端的路径，不校验就是一个任意文件删除原语。删除后列表刷新、显示回退到剩下最新的一份。

## 1. 背景

`SavePlan` 产出的计划只有 ACP 客户端看得到：`PlanToolEnabled` 全仓库只在 ACP 三处置 true（`agent/acp/agent.go`），
桌面上这个工具**根本没注册**，即便注册了，桌面也只会把它画成一张普通 tool card，展开是原始 JSON 参数。
仓库里已经躺着 80+ 份真实 plan 文件——它们全都写给编辑器看的。

## 2. 关键事实（决定了整个设计）

1. **一份 plan = 一个文件**：`<会话 cwd>/.tachi/plans/<title-slug>-<sessionID>.json`，
   内容 `{title, content(markdown), steps[{content,status}]}`；同题覆盖、**异题并存** → 一个会话可以有多份 plan。
2. **ACP 已有结构化 plan update**（`buildPlanUpdateFromArgs` → Zed 的 plan 卡）：桌面是同一份数据的第三个消费者，
   所以「args → 结构化 plan」的解析必须收口成一份，而不是写第三遍。
3. **桌面当初关掉 system reminder 的原因不是「不需要提醒」**，而是提醒层没有会话概念：
   project（`.tachi.md`）、git、plan 三个 reminder 全部读**进程 cwd**（`os.Getwd` / `config.FindProjectRoot()`），
   而桌面一个进程托管多个会话、GUI 进程 cwd 本身无意义（Finder 启动是 `/`）。
   直接打开开关的后果是：git 分支来自启动目录（与 prompt 里宣告的 working directory 互相矛盾）、
   `.tachi.md` 读不到或读错、plan 追踪**一条都不出**（`FindProjectRoot()` 解析不出会话的 `<root>/.tachi/plans`）。
4. **面板的数据取自文件，而不是 transcript**：文件就是那份文档（SavePlan 原地覆盖同一个文件），
   所以「重启后仍在」不需要新的持久化，也能显示**别的前端**存下的 plan。

## 3. 设计

### 3.1 提醒会话化（先决条件）

- `systemreminder.workDir(ctx)` = `wdctx.Dir(ctx)`。它自带降级链（ctx → 进程兜底 → `.`），
  所以 CLI/TUI 行为不变（它们的进程兜底就是启动目录），只有桌面从「错」变成「对」。
- `config.FindProjectRootFrom(dir)`：把「从进程 cwd 往上找 git root」拆出显式目录版本，plan reminder 用它。
- 三个 reminder 改走 `workDir`：project（读 `<dir>/.tachi.md`）、git（在 `<dir>` 里跑 git）、plan（在 `<dir>` 的 git root 下找）。
- 桌面 `buildAgentForSession` 去掉 `DisableSystemReminders: true`。
  **意外收获**：桌面现在终于会给 agent `.tachi.md` 项目约定与 git 状态了（此前完全没有）。

### 3.2 P1 显示

- **解析收口**：`tools.PlanFromToolArgs` / `tools.PlanFromFile`（同一个 `SavePlanParams`，因为磁盘上的形状就是工具参数形状）；
  `agent/acp/stream.go` 改为消费它。
- **读取**：`desktop/plan.go` 的 `GetPlan(sessionID)`：
  在会话各 root 的 `.tachi/plans` 与全局兜底 `~/.tachi/plans` 里 glob `*-<sessionID>.json`，
  按 mtime 取最新，附 `others`（同会话别份数量）与 `note`。
  **刻意不复用** reminder 的「只挑未完成且 24h 内」逻辑：面板要能看到已完成的计划（那是「做完了」这个事实）。
- **实时**：SavePlan 成功 → `agent:plan` 事件，只喊「重读」，由前端调用 `GetPlan`。
  这样面板与文件不会各说各话（若把 args 直接当载荷，`Path`/`Others` 就缺了，而磁盘才是真源）。
- **UI**：footer 的 `计划 N/M` chip（**只有存在 plan 才渲染**）+ 锚在按钮上的 popover：
  步骤 ✅/🔄/○（完成项划掉、当前项加重）、正文 markdown 可展开、文件「预览/打开」、以及「还有 N 份」提示。

### 3.3 P2 plan 模式

- `AgentService.GetMode/SetMode`：`SetMode` 复用 `AIAgent.SetMode`（它自带 meta 持久化）；
  **会话在跑时拒绝**——模式的工具过滤每轮都跑，中途切换会把工具从正在跑的 agent 手里抽走。
- `prepareSession` 应用 `sess.Mode`：meta 是记录，静默按 auto 跑会让记录与运行时不一致。
- `systemPromptFor` 在 plan 模式追加 `agent.BuildPlanModePrompt()`，**并把 mode 加进 `promptKey`**：
  漏掉它就会把缓存的 auto prompt 发给 plan 模式的回合（「刚给你上了锁，却还告诉你可以改文件」）——
  与当年 roots 那个坑同型。
- UI：模式 select（自动/只读/计划，与模型选择器并排，因为两者一起描述接下来这一轮）；
  被拒绝时显示 ⚠（title 带原因），而不是静默弹回；plan 模式下面板里出现「开始执行（切到 auto 模式）」。

## 4. 顺带修掉的一个真 bug

用 SavePlan 记录本计划时踩到：`planSlug` 用**字节**切片截断 48，
中文长标题被切成半个字符 → 文件名不是合法 UTF-8 → 保存**直接失败**（`illegal byte sequence`）。
改用 `strutil.TruncatePlain`（按 rune 截断），并加 `TestPlanSlug_LongCJKTitleStaysWritable`
（断言不只是 slug 合法，而是整个保存路径能落盘——只测 slug 看不出「写不进去」）。

## 5. 验证

- **Go**：`agent/systemreminder/workdir_reminders_test.go`（5 个，改回进程 cwd 必红）、
  `desktop/plan_test.go`（5 个：取最新 / 不串会话 / 已完成仍可见 / 读不出来 / 无会话）、
  `desktop/agent_driver_test.go::TestSystemPromptForPlanMode`（plan 规则进 prompt + **mode 是缓存键的一部分**，
  切回 auto 立即生效）、`agent/tools/plan_test.go` 的 CJK slug 用例。
- **真机**（隔离 HOME + `TachiSmoke` + scripted mock，见 .tachi.md）：
  - chip 显示磁盘上的计划 `计划 2/4`（重启后仍在）；
  - 面板标题/计数/四个步骤的状态/正文 markdown 全对；
  - mock 直接返回一次 SavePlan 工具调用 → 计划真的落盘（`实时保存的计划-smoke-diff-0001.json`），
    chip **实时**从 `2/4` 变成新计划的 `1/3`（证明 `PlanToolEnabled` + `agent:plan` + `GetPlan` 整条链路）。
- **未验证**：从 UI 的 `<select>` 切换模式——driver 用原生 setter + `change` 事件没能被 React 接住，
  所以那一轮没有真正切换（prompt 里也就没有 Plan Mode）。该路径由上面的 Go 测试覆盖；
  UI 交互需要人工点一次确认。

## 6. 不做

- 步骤状态回写 / 「按计划继续推进」按钮：涉及「计划归谁所有」（agent 保存 vs 用户改动），
  且会引入新的写盘绑定，收益不明。
- 跨 session id 的归属：计划按 session ID 归属，对话被续接成新 id 后旧计划不再显示（见 §2 事实 5）。
  放宽到"同工作区的所有计划"会把别人的计划也拉进来，需要先想清楚权限/噪音，暂不做。
