# Tachi —— Jev 手册（TypeSafe System One）

这份文件管 `pkg/jev`、经过它的每一道语义门禁，以及**任何人——包括上一个会话——扫出来的那个阈值能不能信**。

## 目录

| 章节 | 什么时候读它 |
| --- | --- |
| **它是什么、处在哪个位置** | 你在判断某个调用点能不能迁到它上面时 |
| **什么时候该用——什么时候不该用** | 你正要伸手去用它时——**从这里开始** |
| **给调用点接线** | 你已经挑好调用点，正要写那道门禁时 |
| **测一个阈值** | 你需要一个数：一个截断点、一个上限、一个成本。**没有哪个数是事实** |
| **陷阱** | 某个东西表现得很怪，或者你正在评审一份 Jev 集成时 |

Related: [`.tachi.md`](../../.tachi.md)，以及 `pkg/jev` 的包文档——约束清单在那里，离必须遵守它的代码只有一屏。

## 它是什么、处在哪个位置

Jev 是一个**评估**端点，不是聊天补全。一个请求带一个 `state` 和一张类型化 `questions` 的表；响应为每个问题带一个类型化答案——一个概率（`noul`）、一个被选中的选项加它的完整分布（`choice`），或者有序档位上的一个位置（`score`）。**它从不生成文本**，也从不解释自己。

这就是全部的边界，也是 `pkg/jev` 有意**不**实现 `llm.Provider` 的原因：没有消息、没有工具、没有流式——把它折进 provider 接口会把两份互不相干的契约塞到同一个类型背后，还会招来那种「其实只需要其中一份」的调用点伸手去拿另一份。

**有四个生产调用方。** MCP 工具搜索（`agent/mcp/jev_search.go`，matcher 为 `mcp_tool_search.matcher: auto` 且带一个可用的 `jev` 块）把一段散文式请求变成每个候选工具一个 `noul` 问题，然后把过截断点的那些提供出去；问题文本放在 `agent/mcp/jevsearch` 里，因为产生那个截断点的 eval case 文件是从它生成的，所以这次测量不可能描述一个门禁已经不再问的问题。cron 通知门禁（`channel/manager/notify_gate.go`，matcher 为 `cron.notify_matcher: auto` 且带同一个块）判断一次已经结束的 `notify=when_relevant` 运行，问题与 state 在 `channel/manager/notifygate`；与那个文件配对的是 `cron.jev_threshold`。Bash 判断门禁（`agent/agent_permission_jev.go`，`permissions.bash.jev: auto`）问的是：一条规则没有拒绝的命令，是不是该先拿给人看——问题与 state 在 `agent/permission/jevask`，阈值在 `permissions.bash.jev_threshold`。**文件**门禁（`agent/agent_permission_files.go`，`permissions.files.jev: auto`）对一次 `ReadFile` / `WriteFile` / `EditFile` 调用问同一个问题，问题与 state 在 `agent/permission/jevfiles`，阈值在 `permissions.files.jev_threshold`，还有自己的一套 case（`pkg/jev/testdata/files-ask.json`）。后两个是「答案只可能升级到人、永远不会阻断」的调用方——拒绝不是它们该做的决定——而这两个问题包是**兄弟**而不是一个共享包，是刻意的：共享渲染器会让一句改写同时废掉两个阈值。哪些事实该进哪一个 state、以及为什么，见 [`permission.md`](permission.md)。
**没有哪个截断点是事实，它所属的那道门禁也不是**：工具搜索的 τ = 0.30 属于一个**池**，notify 门禁的、bash 门禁的和文件门禁的属于它们各自的 case set——而且 bash 和文件那两个数彼此独立，尽管两者都是对着 `jev-1.13.0` 扫出来的：它们回答的是不同的问题。
把其中任何一个复制到别处之前，先读[测一个阈值](#测一个阈值)；当它当初被测量的那个对象发生实质变化时，重新扫。仓库里其余的一切仍然问生成式模型然后解析回复。

## 什么时候该用——什么时候不该用

**一个问题就能决定：调用方是读模型的输出，还是展示它？**

问那个答案接下来会怎样。如果紧接着就是一条 `if`、一次阈值比较或一个 `switch`——那模型被当作**判断**在用，这就是 Jev 的形状。如果那个答案会被存起来、渲染出来、记给人类看，或者流到 UI——那是**生成**，它留在 `llm.Provider` 上，不管那段文本多短、多无聊。

第二个更弱的检验能把剩下的绝大部分筛掉：**你能事先把答案枚举出来吗？** Jev 从你提供的选项里挑，或者回答一个你能说清的 yes/no。它无法凭空造出一个标签、一个关键词、一个文件路径或一段摘要——所以任何开放式的东西都是生成，哪怕输出很小。

**Jev 形状**（代码消费的判断）：

- 一个标记协议：调用方让模型吐一小段自由文本，然后对它做字符串匹配。今天在 Tachi 里，那就是 GitHub channel 的 `[NO_REPLY]` / `[READY_FOR_PR]` 控制标记（`channel/github/security.go`），以及该 channel 用于环境轮次的 `[SILENT]` 静默标记——cron 通知，也就是另一个 `[SILENT]` 站点，已经挪到下面的 notify 门禁（在那里这个标记作为兜底活了下来，这与「它是那个决定」不是一回事）。以上每一个都是穿着判断外衣的生成式调用——该挪它的理由是：那个标记可以被输入伪造、被模型打错字，或者落进最终会被发布出去的文本里。
- 提示词已经用散文描述过的二元或小集合门禁：「这事值不值得打断用户」、「这条评论需要回复吗」、「这归哪个团队」。
- 对**代码**产出的候选列表做重排——MCP 延迟加载工具搜索（对工具元数据做 BM25）就是常驻例子。代码保证候选覆盖；判断从中挑一个。`agent/extractor.go` 的搜索关键词抽取器是同一形状但缺了一半：关键词是开放式的，所以 Jev 产不出来，它只有在代码提供一个候选池供其选择之后才变成一个候选。在那之前它仍是生成。

**不是 Jev 形状**（别碰它）：

- **任何输出会被展示或存储的东西。** 会话标题、压缩摘要、提交信息、评审文字、dream 的记忆巩固、deep-research 报告。全是生成。
- **图像与音频描述。** `agent/vision_fallback.go` 必须留在多模态模型上——见下面那个非文本陷阱；这是唯一一个「伸手去拿 Jev 会得到自信的垃圾而不是报错」的地方。
- **硬安全决定。** `agent/permission/policy.go` 的 Bash `deny` 是确定性的，而且必须保持确定性。概率不是保证，一个 99% 情况下正确的 `deny` 就是个坏掉的 `deny`。加权分数与否决规则是两回事，绝不能共用一个数。
- **调用方已经握有完整上下文时还去挑一个东西。** 比如自动技能选择，是由主模型读目录做的；把它绕经 Jev 会丢掉上下文且一无所获。

## 给调用点接线

每道门禁都需要五件事，按重要程度排序：

1. **一条降级路径。** Jev 不可达、超出限额，或者回了个错时，必须回退到调用点原有的行为，而且回退要偏向该门禁目的的保守一侧——而不是偏向「悄悄丢掉用户本该看到的东西」。配置门控了整件事：`config.JevConfig.Configured()` 是 enabled-AND-key，所以一个填了一半的配置段落会让所有地方都保持旧路径，而不是每次调用各自失败。桌面设置页已经提供这个块（`desktop/settings.go` 里单独一行 `jev`，在模型下）。**门禁在哪里读配置，就决定那一行的模式**：在判断时读，那一行保持 `applyLazy`；从配置里构建并持有一个客户端，那一行就是 `applyPush` + 一个 `Apply`，且要在同一个提交里——被缓存的客户端让 key 变成一个 push 形状的事实，否则页面会保存一个直到 agent 重建才生效的值。工具搜索门禁在它触及的两行上都是第二种情况：它的客户端和搜索器按 agent 构建（`agent.mcpSearcher`、`agent/agent_jev.go`），所以 `jev` 行和 `mcp_tool_search` 行每次保存都各自调用 `a.RefreshMCPSearch()`——没有它，`matcher` / `jev_threshold` / `jev_budget` / 一个 key 就会看起来是活设置，而实际上只有新 agent 才看得到。账本那一半是反方向的例外：一行的价格和积分汇率是在写那一行的地方现读的，所以定价不需要 push。这个上界通过 `WithHTTPClient(httpx.NewClient(cfg.Jev.Timeout, cfg.Jev.Proxy))` 抵达客户端，做法跟 `web_search` 一样——`pkg/jev` 没有 `WithTimeout` 选项。
2. **一个钉住的模型。** `pkg/jev` 的 `DefaultModel` 是一个具体版本；让空的配置字段保持为空，好让包默认值生效。服务的 `jev-latest` 别名会移动，一个对着旧权重扫出来的阈值会在它移动时悄悄失效。
   **一个只暴露别名的端点无法满足这一条，而且这必须在记录这个数的地方说出来。** 挡在服务前面的网关接受一个模型名、拒绝钉住的版本（`typesafe/jev` 有响应，`typesafe/jev-1.13.0` 回 400 `unsupported_model`），而响应里回显的是别名而不是实际应答的东西——所以指向这种网关的配置，`model` 里是个别名，它的 τ 值又变回假设，而这次移动从这里看不见。优先选接受版本的端点；实在没有的，那些阈值是重新扫出来的，不是信来的。
3. **每个 `choice` 上都要有一个兜底选项。** 它不是给你忘了的输入准备的保险网——它校准的是那个分布。没有它，模型被迫挑一个，并为了提高把握而抬高置信度，于是你之后扫出来的阈值系统性地偏高，门禁随后恰好漏掉它存在的意义就是要抓的那些 case。
4. **用量记账。** `llm` 账本是一个 `Provider` 装饰器，而 Jev 不是 provider，所以它的调用不会出现在里面。决定它们怎么出现，是接第一道门禁的一部分，而不是之后靠抄一个现成模式补上去的东西。工具搜索当时是怎么定的：门禁自己在 `llm.UsageKindJev` 下写那一行（session、model、输入/输出 token，一条显式的 0 积分——Jev 不挣积分，但也不是一行预授信），价格来自内置模型表，Jev 在那里像任何别的模型一样有一条记录（`llm/builtin_models.go`：厂商按 USD 报价，所以那条记录报 USD——`usdPrices`——解析器按配置的汇率 `usd_to_cny` 换算，默认 `llm.DefaultUSDToCNY`）。一道跳过价格的闸门会把它的调用记成免费，一条侧信道就是这样在 /usage 里悄悄变得看不见。
   同一个接缝在配置侧的另一半是 `config.JevConfig.Pricing` / `CreditRate` 和 `llm.ResolveModelPriceFrom(schedule, usdToCNY, source, model, at)`：`Record(UsageRow)` 接受一行普通记录（不需要 `Provider`），而一个原始的 `*config.PricingConfig` 就能给它定价，因为价格表并不总是某个 provider 的。工具搜索解析所走的就是这条——配置块设了覆盖就用覆盖，否则用内置表，也就是 provider 自己那行记录所走的同样顺序——而它的积分是 `ComputeCredit` 按配置汇率对这次调用的 token 算出来的，即使为 0 也会写（nil Credit 意为「预授信行」，不是「零成本」）。
   **不要为了承载 Jev 的定价而加一个假 provider 条目。** `ProviderConfig` 不是惰性的——`BuildProvider` 会遍历它，于是一个 `typesafe` 条目会变成一个真正的 LLM provider，标题、视觉和兜底解析都可能选中它。
5. **一条能看见它决定了什么的途径。** 答案是门禁为什么放过或漏掉某个东西的唯一记录——一个概率在别处根本不存在，事后也无法重建。每个请求一行 **Info**，元数据在前、分数在后（一个按字段打印 100 项 map 的处理器会把它们交错在一起）：query、实际应答的模型、chunk i/n、阈值、token，然后是按概率排序的 `answers="name=0.912, name=0.044"`。服务没有回答的问题写成 `name=-`，绝不写 `0.000`：它没有被判断过，这与「被判成低」不是一回事。`agent/mcp/jev_search.go` 的 `logAnswers` 就是要照抄的形状；`TestJevSearcher_LogsTheAnswers` 钉住那一行，`TestAnswerList_OrderAndUnanswered` 钉住排序。
   **那个概率现在也抵达了模型**，因为 `MCPSearchTools` 把它在每个结果上渲染成 `score` + `matched_by`（`judgment` / `keywords` / `name`）——这一对，从不单独给一个数，因为一个 noul 概率和一个 BM25 分数是不同的量。这让那段框定话变成承重的而不是装饰性的：一个结果之所以被返回，只因为它过了截断点，所以低分意味着匹配弱而不是工具错，而工具描述用一句话把这点说清（`tools.ScoreAdvice`）——读到它却把它当成判决的模型，会恰好丢掉 τ 当初扫出来就是为了纳入的那个边缘工具，然后再花一轮重新搜索。

**输入的形状属于代码，不属于模型。** 把 state 写成判断真正需要的那些字段、来自有名字的来源，而不是把身边恰好躺着的一段散文递过去——为什么这是一条正确性要求而不是整洁偏好，见下面那个语域陷阱。

## 测一个阈值

**这个仓库里没有哪个阈值是事实，也没有哪个可以从别处抄——不从这里这些文档、不从本地的 `jev` 技能、也不从另一道门禁。** 一个置信度数的意思是「分布在你这个调用方所提供的这些选项上有多集中」；增删或改写一个选项就会重新标定它，模型版本变化也一样。每一个截断点都必须对着调用方自己的选项和它自己的 case 扫出来。

`pkg/jev` 为此提供了 harness：

```sh
# one case set (the usual way — start with the file you are working on)
JEV_API_KEY=... JEV_EVAL_FILE=notify-gate.json go test ./pkg/jev/ -run TestJevEval -v

# everything under pkg/jev/testdata/
JEV_API_KEY=... go test ./pkg/jev/ -run TestJevEval -v

# without a key this test skips, so `go test ./...` stays offline
JEV_EVAL_REPEATS=10        # default 5; below 5 the stability numbers mean nothing
JEV_EVAL_MODEL=jev-x.y.z   # re-measure a calibration against a new version
```

它逐 case 报告：答了什么、模型对正确答案有多确定，以及动作概率在重复之间的**范围**；然后是阈值扫描、范围横跨某个候选截断点的那些 case，以及一个 `pair` 的各种形式之间的对比。它对那些数不断言任何东西——一个因为校准差就失败的 harness 会教人忽略它——只在请求错误和畸形 case 文件上失败。

**使用它的两条规则：**

- **要分开 N 个维度就需要完整网格，重复不是替代品。** 两条共享了同一个混淆变量的臂会一致，而且无论你重跑多少次都会继续一致——可复现性度量的是你固定住的那个条件，不是你想变动的那个。一个混淆设计下的两臂对比，产出一个又紧又自信又错的数。
- **稳定性是 run 内性质；门禁暴露在 run 间的那个性质下。** 同一个请求的重复可以完全一致，而整个文件重跑一次就能让某个聚合量移动超过漂移容差。优先取分离区间**中间**的截断点，而不是它的边缘——边缘正是下次 run 会把某个 case 走过去的地方。

`TestEvalCaseFilesAreWellFormed` 离线运行，在任何人花掉调用之前检查 case 文件：一个标签里的错字本来会表现为「门禁什么都没分开」，而这读起来跟模型失败一模一样。

**扫出来的阈值记在哪里**：和门禁在一起，作为它变成的那个配置字段上的一条注释——τ、扫它时用的池（或 case 文件）、以及模型版本，因为这三点里没有哪一个可以在不使这个数失效的情况下变动。工具搜索的是 `mcp_tool_search.jev_threshold`，而那道门禁是按它**运行时**的形状扫的，不是通过 case 文件 harness：

```sh
# 1. dump what this machine actually has (production path: whitelists, search hints, summaries)
TOOL_SEARCH_POOL_OUT=$PWD/pkg/jev/testdata/tool-search/local/corpus.json \
  go test ./agent/mcp/ -run TestDumpDeferredPool -v
# 2. author the intents beside it (local/queries.json), then sweep every candidate for every request
JEV_API_KEY=... TOOL_SEARCH_CORPUS=$PWD/pkg/jev/testdata/tool-search/local/corpus.json \
  TOOL_SEARCH_QUERIES=$PWD/pkg/jev/testdata/tool-search/local/queries.json \
  TOOL_SEARCH_SWEEP_RAW=/tmp/raw.json \
  go test ./pkg/jev/ -run TestToolSearchPoolSweep -v
# 3. argue with the stored run for free — no calls, any cut-off, any corrected label
TOOL_SEARCH_SWEEP_RAW=/tmp/raw.json go test ./pkg/jev/ -run TestToolSearchRawAnalysis -v
```

`testdata/tool-search/local/` 被 gitignore：一个池会点出内部服务器和它们的描述，所以语料留在本地，只记录它产出的那个截断点。问题文本逐 case 不同的，走 case 级的 `instructions` 覆盖，而生成器必须调用生产函数来构造那段文本——否则文件测量的是一个门禁从不发送的提示词。

**channel 的 cron 通知门禁：它的 case 文件是生成的，它的截断点是 `cron.jev_threshold`。** 问题、state 以及 state 被允许采用的两种形状都在 `channel/manager/notifygate` 里；决定规则（服务失败意味着什么、一份只有那个标记的报告意味着什么、一次失败的运行意味着什么）在 `channel/manager/notify_gate.go` 里。case 文件之所以是生成的，是因为手写的那份测量的是一个没人会发的 state：`pkg/jev/testdata/notify/source.json` 里的一个手写场景经过那些生产组装器渲染三遍，所以三份共享 id、标签和表头——

- `notify-gate-prompt.json` —— **candidate**：任务自己的原话随 state 一起走（任务原话），用 `Question()` 发问。判据是逐任务的（「开市了就总结」vs「有到期单就说」），这正是要点：没有哪一种通用的「值不值得打断」读法能同时服务两者。
- `notify-gate.json` —— **control**：结果 + 报告以散文形式给出，用 `GenericQuestion()` 发问，也就是一个从没见过任务原话的裁判会答的东西。它给那个判据块定价。
- `notify-gate-template.json` —— 一个 **variant**：报告被切成两个钉住的字段，留给「纯散文本身最后发现扫不动」的那种情况。是后续跟进，不是同级对等物。

candidate 和 control 在 state **和**问题上同时不同，这是刻意的：一个问题没告诉模型去读的判据就是死重，所以这一对回答的是「这个设计是不是更好」，而不是「这个块值不值 30 个 token」：

```sh
NOTIFY_CASES_SRC=$PWD/pkg/jev/testdata/notify/source.json \
NOTIFY_CASES_OUT=$PWD/pkg/jev/testdata \
go test ./channel/manager/notifygate/ -run TestGenerateNotifyCases -v
```

编辑生成出来的文件是白费功夫。问题、标签和 state 文本是中文的，因为这次部署的任务就是中文的——一道翻译过的门禁需要它自己的扫描，而不是这里的阈值。它的 `existence-*` 组是这次部署自己的任务（扫 A 股 / 查菜单 / 查到期单 / 盯 blog 更新），与其余的是不同的判断：「到底有没有东西回来」，这里一次抓取失败和一次真正为空的结果绝不能只靠散文来区分。一次通过网关抵达服务的部署必须设 `JEV_BASE_URL`（需要的话还有 `JEV_PROXY`），否则每个 case 都会对着一个它看不见的 host 失败。

**任务自己的原话在 state 里，才是这道门禁能工作的原因**：一个说「开市了就总结」的任务和一个说「有到期单就说」的任务，从同一份报告里想要的东西不同，所以没有判据的 state 让判断没有规则可套，那条走廊就消失了（control 臂——结果加报告、通用问题——根本没有走廊）。部署出去的截断点是 **`cron.jev_threshold` = 0.45**，取自对着 `pkg/jev/testdata/notify-gate-prompt.json` 量出来的那条走廊的中间；这个 case 集的标签是一个人的判断，所以动它之前重新扫。

**工具搜索的截断点是那个**池**的工具文本的性质——184 个工具的池上 τ = 0.30，而不是那两个大家容易误怪的别的原因：**

- **不是请求形状。** 同一个 (query, tool) 单独问、对着自己的服务器问、放进一个 184 问的请求里问、以及在生产的分块里问，得分彼此相差在 0.02 以内（`TestToolSearchShapeProbe`）。一个用固定夹具扫出来的阈值是形状可移植的；门禁自己的批处理从来不是某个数移动的原因。**把一次搜索收窄到一个服务器是同一情况，不是重扫**：搜索工具的 `server` 参数移除候选，而不是改写问题——上面那句「against its own server」字面上就是被对比的四种形式之一——所以它是一根成本杠杆（一个候选就是一个问题，而一整个池每一百个候选一次请求），而不是另一个问题。这也是收窄后的搜索会记录自己范围的原因（`logAnswers` 带着 `server`），并且不从它外面回答任何东西。
  一个请求里塞多少个问题是同一类杠杆，而它属于端点而不是任何一道门禁：一个服务——或者挡在它前面的一个网关——会拒绝一个带着太多问题的请求（在一个网关上实测：HTTP 400 `at most 20 questions per call`，只超一个就报，而且这个状态不会被重试，于是整次搜索降级成关键词路径）。`jev.max_questions_per_request` 设置页大小，`config.JevConfig.QuestionsPerRequest` 为未设置的值从 `base_url` 解析出来：已知网关取它自己实测的上限（`config` 里的一张表），其余一律取参考服务的 100。改它动的是往返次数（以及 `jev_budget` 必须覆盖的墙钟时间），不是一个概率：一次搜索的 state 就是那个 query 本身，而问题文本两种情况下都一样。
- **它确实是那些工具文本。** 同一批意图对着一个小型手写夹具能干净地分开，对着真实池就分不开：真实的描述是写给一个已经了解产品的人看的——前提条件（「使用前提：先调用 …」）、必需的服务器模式、产品黑话——而 `No` 判据的那句「需要请求没提供的输入」读的正是这些。一个描述被重写过的池，就是一个截断点重新变回假设的池。

决定那个数的是按 REQUEST 测量，不是按 (query, tool) 对——一个点名了两三个合法答案的意图，被其中任何一个服务到都算数，所以对级命中率会把一次其实有效的搜索低估。在那个 184 工具的池上（jev-1.13.0，每次搜索约 15k 输入 token），这个截断点换的是什么：

| τ | recall | tools offered / search | precision | no-tool leak |
| --- | --- | --- | --- | --- |
| 0.15 | 0.89 | 3.3 | 0.29 | 3/10 |
| 0.20 | 0.81 | 2.2 | 0.40 | 3/10 |
| 0.25 | 0.70 | 1.6 | 0.47 | 2/10 |
| **0.30** | **0.60** | **1.1** | **0.55** | **0/10** |
| 0.35 | 0.51 | 0.9 | 0.63 | 0/10 |

*recall* = 有任何一条预期过了 τ 的搜索所占比例；*offers* 是成本那一侧（每个被提供的工具都是一份注册的 schema 和一次工具列表变化，而后者会使提示词前缀失效）；*no-tool leak* = 池里没有任何东西能服务、却仍然被答了点什么出来的请求。仓库默认值落在 **0.30**：每次搜索大约提供一个工具，没有未被标注的答案漏过去，代价是五次搜索里有两次空手而归。往下一行用两倍的提供量和 3/10 的泄漏买 21 个点的 recall；往上一行再用另 9 个点的 recall 换工具成本降到五分之一。

## 陷阱

**非文本的 state 会悄悄且自信地失败。** 以 base64 发出的图像会返回 HTTP 200、一个看似合理的答案，以及比同一个请求带噪声时*更高*的置信度——那个答案是从空无里读出来的。先把图像和音频用 OCR/CV 流水线转成文本。只有 4xx/5xx 是可信任的信号；一个高置信度的 200 不是一个答案正确的证据。

**两个 token 上限里更紧的那个才是约束。** 服务文档里写了一个按请求的预算，和一个更小的「`state` 加最长单个问题」预算，而一个大请求先撞上的是更小的那个——远在总量接近它自己的上限之前。让 `state` 加最长的单个问题落在它之下，并且宁可要很多短问题也不要一个巨长的问题；读 `Response.Usage` 看一次请求实际花了多少，而不是估算。Tachi 的门禁每个请求发几百个 token，所以这只有在 `state` 长成真正的文档文本时才会咬人。

**语域比语言影响大得多。** 同一个判断，写成一份任务报告和写成一条同事的便条，对概率的推动远大于把它翻译一遍——语言差异留在漂移预算之内，而语域差异会翻转决定，且两种语言对称。所以一道喂着不受控散文的门禁根本不可扫：它的输入是一个没人固定住的变量。这就是「输入的形状属于代码」的理由：传一份从有名字的字段组装出来的固定摘要，绝不传语气随上一轮心情而定的原始模型散文。证据和 case 集在 `pkg/jev/testdata/notify-phrasing.json`，是语言 × 语域的 2×2——那几个 plain 臂是随意的*并且*轻度弱化的，所以要把它读成「语域加语气」，不是单看语域。

**空选项列表不是你一定会注意到的那个失败模式。** 把某个输入标错的门禁，和把所有东西都标错的门禁，在摘要里长得一样；去看逐 case 的表，并检查那个兜底选项是不是真的在吸收垃圾，而不是模型把那些输入硬塞进一个真实类别。

**关于一对形式的判决是关于 argmax 的。** 同一个判断的两种形式落在不同标签上，意味着它们默认不一致，而不是没有门禁能服务它们：一个高于较高那个形式的截断点仍然把两者判成一样。在断定一对形式不可服务之前，先看扫描结果。
