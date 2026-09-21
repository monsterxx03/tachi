# Jev ToolSearch：用 System One 判定替代 BM25 关键词匹配

状态：**设计**（2026-09-20）。上游背景见 `docs/2026-05-19-mcp-tool-search-design.md`（渐进式工具发现那份），
本文只覆盖"匹配算法"这一层：把 `DeferredPool` 的 BM25 关键词打分换成 Jev 的逐工具判定。
`docs/agents/jev.md` 是接调用点前必读的手册，本文的信号形状、降级路径与阈值纪律都服从它。

## 一、要解决的问题

`agent/mcp/deferred_pool.go` 的搜索是三层：精确名 → `select:` 前缀 → BM25 关键词。第三层的分词是
`strings.Fields` + 下划线/CamelCase 切分 + 英文停用词表：

- **中文 query 没有空格**，整句变成一个 token，且 CJK token 不会与英文 token 匹配 → 中文查询基本必然 0 命中；
- 于是模型被迫先把意图"翻译"成英文关键词，再用 `+`（必须有）和 `select:`（我知道名字）这类语法去打补丁。

这两个语法都是**给分词缺陷打的补丁**，不是用户需要的能力。目标：query 用自然语言（中英不限），由模型做意图
判定，命中的工具按今天一样的出口返回（名字 + 完整 schema + 标记已发现）。

## 二、为什么是 A2（逐工具 noul），以及它不解决什么

Jev 是**判定器，不是检索器**：一次请求 = 一个共享 `state` + 一组 `questions`，每个 question 是 typed
judgment（`noul` / `choice` / `score`）。它只能判定**我们已枚举出来的候选**，不会检索，也不会生成文本。
所以候选枚举始终是我们自己的事——这就是为什么这里没有"把 query 丢进去就完事"的方案。

在三种接法里选了 A2：

| | 形状 | 中文 | 代价 |
| --- | --- | --- | --- |
| A1 | 一个 `choice`，选项 = 工具名，返回全分布取 top-k | ✅ | 强迫 argmax 语义；"选最好的"会压分布；N 大时分布是否还有区分度未知 |
| **A2（本文）** | **每个工具一个 `noul` 问题，共用 state = query** | ✅ | 每问自带 rubric，input token 约为 A1 的 1.4×；阈值要自己扫 |
| B | BM25 召回 → Jev 精排 | ❌ 召回段仍是 BM25，中文在第一段就断 | 仓库里也没有 embedding provider 可换 |

**A2 不解决**：池子大于一次请求能装下的总量时（见 §三的上限），还得分段或先召回；也不解决"工具列表变化导致
前缀缓存失效"——那是加载新工具时的既有事实，与本层无关（见 §七 非目标）。

## 三、请求解剖

一次搜索 = 一次 Jev 请求（`state` 对全池共享），问句按工具逐条展开：

```
POST /v1/systemone
{
  "state": "<模型给的 query 原文，不做任何加工>",
  "model": "<config.jev.model，空 = pkg/jev.DefaultModel（jev-1.13.0 这个具体版本）>",
  "questions": {
    "mcp__postgres__query": {
      "type": "noul",
      "instructions": "工具 mcp__postgres__query（server: postgres，相关词: query, sql, table, database）：
                       查询 PostgreSQL 数据库。这条工具能不能直接完成用户的请求？",
      "criteria": { "true": "...", "false": "..." }
    },
    ... 每个候选一条 ...
  }
}
```

要点：

1. **state 只放 query**，不塞会话历史。搜索是模型在回合里主动发起的，它自己能写出自足的请求；把历史接进来
   需要新的管线，收益不明（如果实测发现 query 普遍太短，再考虑把"本轮用户请求"一并带上）。
2. **每条问句自带工具文本**（不是把整个池子塞进 state 再用下标指代）。理由：这样评测的 case 文件与生产
   prompt 逐字一致，阈值才可移植（见 §六）；token 上贵 ~30%，相对它省下的整轮往返可以忽略。
3. 工具文本 = `DeferredTool` 已缓存的三样东西拼起来：`serverLower` + `nameParts`（`SearchHint` 就是这两者的
   摘要）+ **描述首行**（`strutil.FirstLineOrTruncate(desc, 100)`，和提醒块显示的一样）。**不放整段
   description**：MCP 描述动辄几百 token，进不了预算，且把散文塞进判定项会把语体噪声带进来（§八 风险）。
4. **yes/no 判据逐问重复**（协议要求，`Question.MarshalJSON` 每个 question 自带 criteria）。必须写，且要短：
   不写 yes/no 时模型会自己猜"yes 覆盖什么"，而且倾向朝刚选的那一侧膨胀。
5. **catch-all 由阈值承担**：noul 只有是/否，"都不相关"就是全体概率低。所以阈值不只是一个取舍，它是
   "这轮压根不需要工具"这个真实情形的唯一出口——扫描时**必须**混入"无工具可满足"的 query 样本。

### 尺寸与分片

- 绑人的是 **state + 最长问句 ≤ ~32k**（state 是 query，问句 ~60-80 token，远远够）；**64k 总量由问题个数消耗**，
  正是"多个短问题"的形状。
- 估算：每问 ~70 token（工具文本 ~30 + 判据 ~40），100 个工具 ≈ 7k，200 个 ≈ 14k。
- 常量 `jevSearchChunkTokens = 8000`：按累计 token 估算切块，一次搜索**顺序**发若干请求（并发没必要，
  而且"大批量投机扇出"会占限流额度）；`jevSearchMaxQuestions = 400` 是硬上限，超出部分不判定并记日志。
- 池子大时**不做 BM25 预筛**：预筛会把中文 query 在第一步就掐死，那正是我们要修的东西。代价是大池子搜索变慢，
  可接受（个人配置几十到几百个工具）。

### 排序与返回

- 概率降序，**同分按工具名升序**（确定性：结果可复现，测试可 pin）。
- 取 `p ≥ threshold`，再截 `max_results`（沿用今天 1-20 的钳制）。
- **一条都没过阈值**：返回空 `matches`，并在输出里加一句提示（"没有匹配的工具，最接近的是 X，但置信度不足"），
  让模型自己决定换种说法还是放弃；概率数字不进结果（避免被当事实引用）。
- 输出结构 `mcpSearchOutput` 保持不变（模型侧行为、已有测试、已发现集标记都不动）。

### 确定性 fast path（保留，且不调 Jev）

今天的精确名 / 后缀名（`select:` 的语义）/ `mcp__server` 前缀**原样保留**，由系统自动识别 query 里的名字，
不再在工具描述里教语法。它是两个东西：模型明确知道名字时的确定性出口，以及 Jev 不可用时唯一可靠的路。
命中即返回，**不发 Jev 请求**。

## 四、降级路径（jev.md 的硬要求）

任何一条成立 → 走今天的 `pool.Search`（BM25），结果形状不变，WARN 记原因：

- `Config.Jev.Configured() == false`（未启用或没有 key）；
- `mcp_tool_search.matcher == "bm25"`（人工对账/强制关闭）；
- Jev 请求失败、4xx/5xx 重试耗尽、超时；
- 预算耗尽（`jev_budget`，默认 6s）：**搜索宁可降级，不许久等**——它在模型回合的关键路径上，而 `pkg/jev` 的
  重试循环本身可以跑 4×10s。
- 某条问句**没有答案**（`Answer.Noul == nil`）：该工具按"未通过"处理（不是 0 分，是没回答），记日志；
  其余有答案的照常参与阈值判定。

## 五、落地改动

| 位置 | 改动 |
| --- | --- |
| `config/config.go` `MCPToolSearchConfig` | 新增 `matcher`（`auto`/`bm25`/`jev`，默认 auto）、`jev_threshold`（浮点，默认 **0.35** —— 实测值，出处见 §六）、`jev_budget`（`Duration`，默认 6s）。三个叶子都落在既有的 `mcp_tool_search` 行下，桌面设置页自动收下，无需前端改动 |
| `agent/mcp/jev_search.go`（新） | `JevSearcher`：实现 `tools.MCPSearcher`；包含 fast path 复用、问句构造、分片、阈值排序、降级。`Judge` 小接口（`Evaluate(ctx, state, questions)`）包住 `*jev.Client`，单测用假实现 |
| `agent/tools/mcp_tool_search.go` | `MCPSearcher.Search` **加 `context.Context`**（Jev 请求必须能被回合取消；现在是 `Search(query, maxResults)`，没有取消路径）；`Description`/`Properties` 改写：删语法教学，改成"用自然语言描述你要做什么，中英文都可以；会话里的请求本身就是最好的 query"；输出加可选 `note` |
| `agent/agent_configure.go` | 两处 `NewMCPSearchToolsTool(...)`（`:99`、`:328`）改走一个 helper：`auto` + `Jev.Configured()` 时包一层 `JevSearcher`，否则原样传 pool |
| `llm/usage_recorder.go` | 新增 `UsageKindJev`（照 `UsageKindVision` 的先例），在 searcher 里用 `llm.WithUsageKind` 记账。jev.md 把"用量记账"列为接 gate 时必须一起决定的事，不能后补 |
| `agent/systemreminder/deferred_tool.go` | 结尾那句「使用 MCPSearchTools 搜索并加载」改成"直接描述你要做的事即可（中英文都可以）"。目录本身（名字 + 一句话）不变——它仍是模型最省事的路径 |
| 测试 | `agent/mcp/jev_search_test.go`：fast path 不触发 Jev、低于阈值返回空、错误降级到 BM25、分片、缺答案按未通过、同分按名字稳定排序；`mcp_tool_search_test.go` 跟着 ctx 调整；桌面 config 守卫测试自动通过（新叶子在既有行下） |

## 六、阈值必须自己扫（这一节决定它能不能进主干）

jev.md 的纪律：**本仓库没有任何一个阈值是事实**，`confidence` 只表示"在你给的这组选项上有多集中"，换一组
选项就重新标定。所以默认值不是猜的，是扫出来的，并且要写清出处。

工具：仓库现成的 `pkg/jev` eval harness（`JEV_API_KEY` + `JEV_EVAL_FILE`，`evalThresholds` 已经扫
0.50→0.99，报告里带 per-case 延迟与 token）。

要做四件事：

1. **case 文件生成**：`pkg/jev/testdata/tool-search-*.json` 由一段生成逻辑产出，输入是（query → 期望工具）
   的手写清单 + 池子里真实的工具文本。中文臂和英文臂都要有，`pair` 字段串起同一意图的两种语言。
2. **harness 支持按 case 覆盖 `instructions`**：现在 question 是文件级的，而生产的每条问句都带自己的工具
   文本。加一个可选的 case 级 `instructions`（覆盖文件级模板）即可，向后兼容，~十几行。**不做这一步，
   测出来的就是另一个 prompt 的阈值**。
3. **扫描**：threshold × (zh/en) 的 hit@k 与误报（"无工具可满足"的 query 里返回了什么），选一个
   分离走廊中间的切点。
4. **合批等价性抽查**：eval 一次只发一条问句，生产一次发 N 条。同一批 (query, tool) 对子在"单发"和
   "合批（塞进 30 条问句里）"两种形态下各测一遍，概率若有实质差异，则阈值必须以**合批**形态扫定。
   另外值得顺手做一次廉价 A/B：同一批 case 用 `score`（不相关/可能相关/直接完成三档）再跑一遍，看分离度
   是不是比二值 noul 好——成本是多一次 sweep，换来一个可能的形状改进。

### 已实测（本机，2026-09-20，jev-1.13.0，`testdata/tool-search-noul.json`）

94 个 case × 5 次重复 = 470 次调用，input 184,130 token（mean 391/call）：

| 量 | 值 |
| --- | --- |
| 阈值走廊 | 不该触发 **最大 0.215** ＜ τ ≤ 该触发 **最小 0.470** |
| **取 τ = 0.35** | 走廊中偏**下**（理由见下面第 2 条） |
| no-tool 臂 | 最大 **0.060** —— 一个数量级的余量 |
| 重复间标签翻转 | 1/94（`pg-select-en`，位于 0.48–0.50 → 在 0.35 处是稳的） |
| 同时跨候选阈值的 case | 13 个，全在 ≥0.50 处 → 0.35 处没有一个会抖 |
| zh / en 同一判断 | 偏移最大 **0.37**、多数 ≤0.19 —— **比走廊还宽** |
| 合批 vs 单发 | max Δ **0.040**、mean **0.002** → 单发扫的阈值可移植，不必给 harness 加批模式 |
| 延迟 | 单问句 420ms 均值；一请求 22 问句 **632ms** 均值（≈ 不随 N 线性增长） |
| 一次搜索的成本 | 22 个候选 = **2,945 input token**（≈134/问 + 固定）；N=100 约 13–15k |

三条从这次测量本身学到的、会改变实现的东西：

1. **标签的定义就是语料**。第一版把"能力重叠"的工具标成 false（`postgres__query` 对"有哪些表"），
   模型给 0.79 —— **模型是对的，标签是错的**：一条 SQL 查询确实能列出表。改判为 true 之后走廊才出现。
   §三 的问句问的是"能不能直接完成"，所以能力重叠必然是 true，而 gate 同时给出两条可用的工具是**好结果**。
   规则写在 `testdata/tool-search/queries.json` 的 notes 里。
2. **语言偏移比走廊宽**，所以 τ 取走廊偏低处而不是中值：漏判的代价是模型重搜一轮（一整次带长历史的往返），
   误判的代价是多给一个同域候选（模型自己会挑）。在 0.35 处误报侧仍有一个数量级余量，在 0.42+ 处最低的那批
   真目标开始掉出去。
3. **阈值有出处**：τ、case 文件、模型版本三者绑在一起写进 `docs/agents/jev.md`。换权重、改语料、改问句
   文本，都会让它作废——重跑一条命令即可（`JEV_EVAL_FILE=tool-search-noul.json`）。


## 七、非目标（明确不做）

- **不做"每轮自动预取"**。那会让工具列表每轮变化，而 tools 在缓存前缀最前面，等于整段历史重读；上游设计
  的卖点正是"活跃工具集稳定增长，每会话只失效 1-3 次"（`docs/2026-05-19-mcp-tool-search-design.md` §二、§三）。
  `DeferredToolReminder` 因此**保留**：它管的是"模型知不知道有这批工具"，与匹配算法无关。A2 也不改变
  工具列表的行为——只改变返回哪几个名字。
- **不做 BM25 预筛 + Jev 精排**（中文在第一段就断，等于没修）。
- **不引入 embedding / 向量检索**（新依赖，另一个工程）。
- **不动 `select:` 的解析**：语法从描述里删掉，解析保留（模型偶尔还会写，且它是零成本的确定性路径）。

## 八、风险与未知

1. **阈值不稳**：工具搜索的 query 是自由散文，而 `notify-phrasing.json` 的 2×2 实验已经证明"自由散文在
   语体之间不可阈值化"（同一对句子因正式/口语偏移 0.46、标签翻转）。缓解：判定项里的工具文本是结构化的
   （名字 + server + 关键词 + 描述首行），把散文面收小；case 里同时放正式与口语两臂。
2. **延迟随 N 增长？** 一次请求带 100-400 条问句的服务端表现未知，必须实测 P50/P95；若随 N 显著增长，
   分片策略要改（更多并发、更小片）。
3. **成本**：计费按 input token，一次搜索 ~7-14k（N=100-200）。相对它想省下的"整轮往返"是便宜的，
   但要做成可观测的（`UsageKindJev`），别变成一个看不见的常项。
4. **误报比漏报便宜**：结果是**候选**不是决定，模型还有一次判断；漏报要模型再搜一轮（一整次带长历史的往返）。
   所以阈值偏向"多给几个"，而不是只放高置信。
5. **缺答案**必须读作"未通过"，绝不能读成 0 分或 1 分——客户端已经警告过 200 带高置信度也可能是错的，
   这里唯一能控制的是"没答案就别选"。
6. **上限**：池子超过 `jevSearchMaxQuestions` 的部分不会被判定。这目前只是个日志，将来要么分段跑完，
   要么引入召回（那时中文问题会回来）。

## 九、实施顺序

1. **先量，不改生产**：harness 的 case 级 instructions + case 文件生成 + 扫描 + 合批抽查。
   （这一步的产物是"阈值 + 候选文本的选择 + A2 vs score 的结论"，与生产代码解耦。）
2. **骨架 + 降级**：`MCPSearcher` 加 ctx；`JevSearcher` + `Judge` 接口 + 假实现单测；配置三个叶子；
   接进两处 wiring（此时 `matcher` 默认 `bm25`）。
3. **接阈值**：把第 1 步扫出来的值写进默认配置，`matcher` 默认改 `auto`，同一次提交里更新
   `docs/agents/jev.md`（第一个生产调用点 + 阈值出处 + case 文件名）。
4. **收尾**：描述与提醒文案改写；`UsageKindJev` 记账；必要时按实测延迟调分片。
