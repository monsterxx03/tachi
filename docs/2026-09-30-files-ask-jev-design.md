# 文件工具的 Jev 判定闸门：把"这次文件操作要不要人先看一眼"交给判定

状态：**设计 + 已实现**（2026-09-30）。上游是两层现成的东西：Bash 那套判定闸门
（`agent/agent_permission_jev.go` + `agent/permission/jevask`，见
`docs/2026-09-30-bash-ask-jev-design.md`）和 `agent/permission/policy.go` 的规则引擎。本文只写这半边
**不一样**的地方；判定的服务契约、降级路径、记账、阈值纪律照 `docs/agents/jev.md`，不重复。

`docs/agents/permission.md` 是必须一直为真的手册，本文是某一时刻的设计（允许过时）。

## 一、要解决的问题

Bash 的命令有规则可写、有判定可兜；**文件工具两样都没有**：

- `ReadFile` / `WriteFile` / `EditFile` 今天**完全不经过任何权限检查**——`NeedsConfirmation()`
  返回 false，设计文档里写过"不动它们"。于是"读一下 `~/.ssh/id_rsa`"和"读一下 `src/main.go`"
  在策略上没有任何区别，前者会把私钥交给模型、写进 transcript。
- 规则层同样是 Bash 专属：想写"永远别碰 `~/.ssh`"没有任何地方可写。
- 反过来，文件调用**频率远高于命令**（一个回合可能几十次 Read），而且 `Read`/`Edit` 是**并行**
  工具——这决定了实现上最麻烦的两件事（见 §三）。

## 二、边界

- **判定不产生 `deny`**：`deny` 必须确定性（jev.md："一个 99% 正确的 deny 是坏的 deny"），
  由规则层决定，判定物理上够不着 —— `checkFilePermission` 里 deny 分支在判定之前返回。
- **只在"人就在前台"的入口启用**：`TUI`（tui）、`External`（desktop、acp）走闸门；
  `Skip`（channel、`tachi -p`、subagent、dream）**判定完全关掉**，只由规则决定。那些入口本来就
  没有一个能回答"要不要看一眼"，让概率替它们放行文件访问是另一个产品决定。
- **拒绝只拒这一次调用，不中止回合**（与 Bash ask 的 `errCancelled` 不同）。文件调用是模型下一步就能
  换掉的步骤（换一个文件读、换一种写法、问用户），为它结束整段对话没有收益。
- 判定只回答"要不要人看一眼"，不回答"这个改动对不对"，不参与 allow/deny 语义。

## 三、插入点与并行

```mermaid
flowchart TD
  A["ReadFile / WriteFile / EditFile"] --> B["规则层：deny &gt; allow豁免 &gt; ask"]
  B -->|deny| C["拒绝（判定够不着）"]
  B -->|非 deny| D{"闸门可用？<br/>前台入口 + auto + jev 已配置"}
  D -->|否 / 没有回答| E["落回规则 = 今天的行为"]
  D -->|是| F["★ 判定：这次要不要人看一眼"]
  F -->|"p ≥ τ"| G["问人（卡：工具+路径+原因）"]
  F -->|"p &lt; τ"| H["放行（静默）"]
  E -->|"命中 ask 规则"| G
  E -->|"没命中"| H
  G -->|"允许"| H
  G -->|"拒绝"| I["只拒这次调用：工具错误喂回模型，回合继续"]
```

**两条执行路径都要接**，这是文件工具特有的麻烦：

- `executeToolGroup` 按 `Parallel()` 把一批调用分组，`Read`/`Edit` 是并行工具、`Write` 不是。
  并行组走 `executeToolCallsParallel`，那条路上**原先没有任何权限检查**（Bash 非并行，躲开了）。
  所以 `checkFilePermission` 在两条路径上各调一次，位置都在 hook 与 checkpoint 快照之前。
- 并行组里的询问要**串行化**：一个 agent 只有一个 `ConfirmResp`（容量 1），两张卡同时等会抢同一次
  按键。用的是 `PermissionState.askMu`（MCP 费用卡先例）。
- `[permission]` 那行 note 走的是事件（transcript），并行路径原先没有这条通道，现在由
  `notes[i]` 从 goroutine 带出来（每个 goroutine 只写自己的下标）。

## 四、规则层：路径规则

配置形状：

```yaml
permissions:
  files:
    read:  {deny: [...], ask: [...], allow: [...]}   # 路径 glob
    write: {deny: [...], ask: [...], allow: [...]}
    edit:  {deny: [...], ask: [...], allow: [...]}
    jev: auto            # auto | off，一个门覆盖三个工具
    jev_threshold: 0.70  # 属于 pkg/jev/testdata/files-ask.json 这把问句
    jev_budget: 2s
```

三个工具各自一套规则，因为**同一个路径不是同一种风险**：读一个密钥是泄露，覆盖它是毁掉工作。
闸门则是一个：τ 属于问句，而三个工具问的是同一个问题（工具名是 state 里的一条事实）。

**路径有两种形态，这是本设计最需要想清楚的一处**（`agent/permission/paths.go` 的 `PathPattern`）：

- 匹配必须用 **canonical**（绝对、clean、符号链接解析到存在的那一段）。否则
  `ln -s ~/.ssh /tmp/x` 之后读 `/tmp/x/id_rsa` 就绕过了所有关于 `~/.ssh` 的规则。
- 但**回显给人的必须是用户写下的原文**。macOS 上 `/etc` 是 `/private/etc` 的符号链接：
  只匹配原文，`ask: ["/etc/*"]` 会静默永不命中（一个不触发的 deny 比没有更危险，因为读者以为锁在）；
  只回显规范化后的路径，卡片会告诉用户"命中 `/private/etc/*`"，而用户配置里没有这一行 ——
  那是 issue #40 同一类错误（让人去找一条自己没写过的规则）。

所以一条规则存两份：`Text`（回显）与 `Canonical`（匹配）；目标路径由
`permission.CanonicalizePath` 两侧共用同一个规范化函数解析。锚定规则：全局配置里的 `~`（以及相对
路径）锚到 home，项目 `.tachi/permissions.yaml` 里的相对路径锚到**它自己那棵树**（所以
`ask: ["secrets/**"]` 在项目文件里就是"这个仓库内"）。`allow` 只在全局生效，项目只能收紧 —— 与
Bash 半边同一条规矩，且**同一个 Policy 对象**承载两半：`SetPermissionTrees` 一次重建两半，
「本会话全部允许」一次静音两半（`AllowAllAsksForSession` 只压 ask，压不过 deny）。

## 五、问句与 state

一条 noul 问句（`agent/permission/jevfiles`），事实集是 state 里逐行列出的：

| 事实 | 为什么是它 |
| --- | --- |
| 工具名（ReadFile / WriteFile / EditFile） | 读和写不是同一种风险，判定要看得见是哪一个 |
| 目标文件的 **canonical 绝对路径** | 就是规则匹配过的那条字符串，判定与规则看同一个文件 |
| 是否存在 / 是目录 / 多少字节 | "覆盖一个 400KB 的文件"和"新建一个文件"是两件事 |
| 会话的工作目录 + 树列表（同样规范化） | 判据里那条边界（"在不在树内"）需要它，而比较是模型的事 |
| 命中的 ask 规则原文，或"没有任何规则命中" | "这次为什么会在闸门前"正是判定需要的输入 |

**判据不是"这次有没有写东西"**：读是最便宜、也最常见的泄密方式，`~/.ssh/id_rsa`、`~/.aws/credentials`、
`.env` 都在 needs-a-look 那一侧。判据依赖的那条边界是"在不在会话工作的树内"——但要**加一个限定**：
树外**且是用户会留下来的东西**（配置、凭据、别人的工作副本）才算，会话自己造的草稿文件（`/tmp`、
写给用户看的导出）算日常（这是用例集标签与判据一起定的，见 §六）。

**state 里没有 payload，这是刻意的**：写入的内容与 edit 的 old/new 都是模型生成的文本，而判定要回答的是
"**这条路径**要不要人先看一眼"。放进 payload 有两个代价：state 随所写内容变长（一次粘贴、一个生成的 bundle），
而"读自由文本的判定"正是 jev.md 的 register trap —— 同一段内容会让一次调用显得更安全或更危险，却没有改变
被碰的是哪个文件。第一轮扫描里 `/tmp` 那条用例正好演示了这件事（内容是一段部署脚本时分数升到 0.54），
于是这一版把 payload 整个拿掉，`/tmp` 这类路径回到草稿该有的位置。
用户的请求同样**不放**进 state（Bash 那侧的配对实验已经证明它不改变判断）。

## 六、阈值纪律

- 用例集：`pkg/jev/testdata/files-ask/source.json`（作者手写：调用的字段 + 标签），生成文件
  `pkg/jev/testdata/files-ask.json`（state 由生产函数渲染，64 条，read/write/edit 各半）。
  生成器：`agent/permission/jevfiles/generate_cases_test.go`。
- 重扫：
  ```bash
  FILES_ASK_CASES_SRC=$PWD/pkg/jev/testdata/files-ask/source.json \
  FILES_ASK_CASES_OUT=$PWD/pkg/jev/testdata \
  go test ./agent/permission/jevfiles/ -run TestGenerateFilesAskCases -v
  JEV_API_KEY=... JEV_EVAL_FILE=files-ask.json go test ./pkg/jev/ -run TestJevEval -v
  ```
- **一处刻意的重复**：`jevfiles` 不 import `jevask` 的渲染函数（treesLine / 占位符），而是各写一份。
  共享一个渲染函数意味着改一句文案会同时作废**两个** τ；两个问题、两套用例、两次测量。

### 扫出来的结果（第三轮，即当前这一版）

三轮扫描的教训本身就是这段设计史的一部分：

| 轮次 | 改了什么 | 结果 |
| --- | --- | --- |
| 1 | 初版（state 含 payload） | 两组**重叠**（该跑最高 0.56 / 该问最低 0.48），无 τ 可分离 |
| 2 | 判据写明"即使在树内改助手自身配置也不是日常"、点明"树是全部那几棵"，改一条标签 | 仍重叠，但该跑组上限降到 0.52 |
| 3 | **去掉 state 里的 payload**（只看目标路径） | 该跑组上限 0.49、下一档该问 0.62，走廊 **(0.49, 0.62)**；0.50 与 0.60 两档都测得"155 次日常调用 0 次被抬起" |

τ = **0.55**，取走廊中部：距日常侧 0.06、距下一档该问 0.07，两侧都有余量（STABILITY 提示要的
正是这个余量，阈值贴着一个日常用例 0.01 会翻脸）。

**它刻意漏掉的三条**：写/编辑 `.tachi/permissions.yaml`（0.43、0.48）与整份替换一棵树内 200KB 数据文件
（0.30）。前两条是重点：**判定不该是阻止 agent 改自己规则的那道闸**，而且在只给路径的 state 里，
`.tachi/permissions.yaml` 这个路径本身并不显得危险（带上内容时它曾得 0.87，内容是我们主动拿掉的）。
确定性答案是规则，值得写一行：

```yaml
permissions:
  files:
    write: {deny: ["**/.tachi/permissions.yaml"]}
    edit:  {deny: ["**/.tachi/permissions.yaml"]}
```

其他实测：平均 709 input token / 次（最长 723，state 加最长问句的上限 ~32k）、平均延迟约 0.6s、
一轮 320 次调用约 4 分钟。


## 七、落地改动

| 位置 | 改动 |
| --- | --- |
| `config/config.go` | `permissions.files`（三个工具的 `PathPermissions` + `FilesPermissions.Jev/JevThreshold/JevBudget`）；τ = 0.55（§六） |
| `agent/permission/paths.go`（新） | `FileTool`、`PathPattern`、`PathRules`/`FileRules`、`CheckPath`、`ExpandPathRules`、`CanonicalizePath`；`Policy` 多三个字段（`WithFileRules` 注入） |
| `agent/permission/policy.go` | `Rules` 文档改为两半共用、`mergeLayer` 抽出、`Empty()` 把路径规则算进去、`sessionApproved()` 抽出 |
| `pkg/fileutil/fileutil.go` | `CanonicalPath`（解析存在的那段前缀，供两条路径共用） |
| `agent/permission/jevfiles`（新包） | 问句 + state 组装（**只有目标，没有 payload**）+ 生成器 + 单测 |
| `agent/agent_permission_files.go`（新） | `checkFilePermission` / `resolveFileAsk` / `judgeFileAsk` / `fileRuleDecision` / 路径与 stat 组装 / 两种拒绝措辞 |
| `agent/agent_ask.go`（新） | `askFrontend`（bash / 文件 / MCP 三处共用的"弹卡等人"）、`fileToolNames` |
| `agent/agent_permission.go` | `bashAskCause` → `askCause`、`bashAskPreview` → `askPreview`；文件规则的构建与树锚定 |
| `agent/tool_executor.go` | 两条路径各接一次 `checkFilePermission`；并行路径带出 note |
| `agent/acp/permission.go` | 卡片的 ToolKind 改用与流一致的 `mapToolKind`（Read 不再被说成 edit） |
| `tui/model_events.go`、`tui/model.go` | 文件 ask 的标题与按键文案 |
| `desktop/permission.go`、前端 `types.ts`/`components.tsx` | 新增 `files` kind 与文案（否则读文件的卡片写"需要你确认这条命令"） |
| `desktop/settings.go` | `permissions.files` 设置行（与 bash 同行：`SetPermissionTrees` 重建策略） |
| `itest/desktop/` | 新场景 `perm-files` + driver |
| `docs/agents/permission.md` | 文件工具那一节（必须一直为真的部分） |

## 八、非目标（明确不做）

- **不给 `Grep`/`Glob`/`WebFetch` 加门**：它们按模式扫，敏感度低一个数量级；要加是同一套机制换个
  事实集，属于下一步。
- **不做内置路径 deny**：Bash 有 `BuiltinDenyRules`（写磁盘、关机），文件侧对应的"不许改自己的规则
  文件"放进判定与用例集（`write-permissions` 标 true），不做成不可解除的硬拒绝 —— 那是用户的决定。
- **不做按类外推的缓存**：只做 (工具, canonical 路径, 命中规则) 的会话内记忆。
- **不改 Bash 半边的语义**（规则顺序、ask 三成因、`[permission]` 行）。

## 九、风险与未知

- **打扰的方向**：判定覆盖每一次文件调用（同路径同规则只判一次，靠记忆），判得偏保守会让人关掉开关，
  那是另一种失败。τ 取在走廊的安全侧，不是正中。
- **payload 不在 state 里**：判定只看目标路径。代价写在 §六（写 `.tachi/permissions.yaml` 这类
  "内容才是信号"的调用落在阈值以下），收益是判定不读模型生成的自由文本，state 也不会随所写内容变长。
- **`Skip` 入口的行为变了**：文件 ask 规则在无人值守入口是**拒绝该次调用**（与 Bash 的 ask 规则一致）。
  默认配置里没有任何文件规则，所以默认行为不变；写下规则的人应当知道这一点。
- **符号链接与规范化**：规则与目标都走 `CanonicalizePath`，两侧同源；但规范化只到"存在的那一段"，
  一个还没创建的目录下的路径按字面比较。
- **τ 会漂移**：模型版本、判据文案、用例集变化都会移动它。日志里两件事是信号：自动放行了什么、
  用户事后为哪次自动放行写了 deny 规则。
