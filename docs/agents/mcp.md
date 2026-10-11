# Tachi —— MCP 手册

服务端这一半在这份文件里：`agent/mcp/`、搜索工具（`agent/tools/mcp_tool_search.go`）、延迟工具提醒（`agent/systemreminder/deferred_tool.go`）。CLIENT 那一半——OAuth、授权按钮、面板、`roots`——在 [desktop.md](desktop.md)（桌面 MCP），这里只指过去，不重复。

## 目录

| 章节 | 什么时候读 |
| --- | --- |
| **池，以及替它做广告的提醒** | 你增删工具、开关某个 server，或改提醒的形状 |
| **搜索** | 你动 `MCPSearchTools`、任何一个 searcher，或 `server` 的收窄 |
| **模型读到的结果** | 你改一次搜索返回的东西 |
| **provider 看到的 schema** | 你动 MCP 工具怎么变成一个 `llm.Tool` |
| **加载的代价，以及那张预算卡片** | 你动成本门禁（`agent/mcp_budget.go`）、`mcp_tool_search.cache_budget`，或确认卡片的文案 |

## 池，以及替它做广告的提醒

- **工具进了池，只要提醒还没说出来，模型就看不见它：任何往池里加延迟工具的路径，都必须同时调用 `NotifyDeferredToolsAdded()`。** `DeferredToolReminder` 每个会话最多触发一次，所以会话中途加进来的东西（开起来一个 server、切 profile、一次刷新）只能靠那笔 dirty 标记到达模型——只往池里写，这些工具在剩下的整段对话里都够不着，而且是静默的。`agent.AddDeferredMCPTools` 是两件事一起做的入口；前端自己往池里加条目却忘了通知，就是那个 bug —— `desktop.SetMCPServerEnabled` 正是手加条目的例子，它靠 `applyToLiveAgents` 补上那次通知，所以它没踩这个坑。凡是要让提醒该重发的路径都走 `ensureDeferredReminder`，一次性注册也归它管——`Collector.AddReminder` 只做 append，若按通知注册，同一条提醒会在一条消息里跑 N 遍。
- **注册不能等着池非空。** 共享池（desktop、channel）是在后台填的，先建出来的 agent 看到的池就是空的；而空池这件事正是提醒要回答的，所以它无条件注册，在真有东西可提示之前保持沉默。`desktop/agent_mcp_test.go`（一个把测试二进制自己当 stdio server 跑起来的桩）和 `agent/agent_deferred_test.go` 钉住了启用、停用、迟到到达这三条路径。
- **`min_tools_for_search` 写不成 0** —— `defaults.Set` 跑在 YAML 之后，会把每个 ZERO 值按 tag 填回来，所以显式写的 `0` 会以默认值 `5` 回来。自动加载的判据是 `len(pool) > threshold`，所以「永不自动加载」那个值是个负数；需要一个工具**留在**池里的 fixture（冒烟场景 `perm-mcp-budget`，它讲的是「一次本来能加载点东西的搜索」面前那张成本卡）写的正是 `-1`。
- **开关一个 server 是**全局**的事实** —— 一个池、一条连接服务所有会话 —— 所以 desktop 要通知所有活着的 agent（`applyToLiveAgents()`），不只是当前显示的那一个；停用走的也是 `UnregisterMCPServer`（registry + 每个会话的 discovered 集合），不是裸地把它从池里删掉。
- **提醒的形状跟着 matcher 走**（`agent.mcpSearchUsesJev`）：Jev 门禁健康时是每 server 一行摘要，落到关键词时是逐工具目录（外加一行 server 名册）。目录不是退而求其次的审美——关键词搜索匹配的正是那些**名字**，而一行摘要会让那个 matcher 无处下嘴。
- **两种形状都以 `serverLegend` 开头**，它说明那些行是搜索工具 `server` 参数的值。工具描述为它指路（"named exactly as the deferred-tools block lists them"），而一份从不给自己打标签的清单，正是 `mcp__github — 30 个工具（…）` 被读成一个**工具**而不是一个 server 的路子。不要当冗余把这行 legend 删掉。
  名册那一行只在两个或更多 server 还有未发现的工具时才渲染：只有一项的名册不是选择，而且那个前缀在每个工具名里已经看得见。
- **各段之间用一个空行分隔**（`RenderPieces`），这也是为什么一段自己不能以空行开头。这个块的字节是一种 wire format——按发出时的样子存下来，由 `ConvertSessionToLLMMessages` 原样回放——所以「渲染改动不算结构性改动（只有此刻正在构建的那个块变了，而它在尾部）」这条规则住在 [prompt-cache.md](prompt-cache.md)。

## 搜索

- **两半，只有一半会被收窄。** 一个点名要什么的 query（`select:`、精确或后缀工具名、`mcp__server` 前缀）是拿**每一个** server 来解析的：名字是身份，一个记错的 server 不能把一次命中变成落空。被 `server` 收窄的是散文——**空 query 的那份清单也一样**，它是一页池子而不是一个身份，所以从所有 server 去回答它，会把另一个 server 的 schema 递给下一个请求（一次工具列表变化，也就是整段前缀重读）。两条规则都住在 `DeferredPool.SearchExact`，每个调用方那一半都从这唯一入口过——包括 Jev 门禁，它自己取那一半而不经 `SearchIn`；一个 searcher 自己留一份清单规则的副本，就是同一个输入能拿到两个答案的路子。搜索工具拒绝纯空白的 query（`query is required`），所以清单是代码走到的地方，不是模型忘了填字段走到的。
- **`server` 活着，是为了让 Jev 门禁付得起** —— 一个候选就是一个问题，所以一个满池就是每百个工具一次请求。`tools.NormalizeMCPServer` 把模型写的东西规范化（`mcp__GITHUB`、` github ` → `github`），searcher 收到的是 `tools.MCPSearchOptions`。未知的 `server` 是一个 ERROR，里面点名存在的那些 server（`tools.MCPServerLister`，两个真 searcher 都实现了）——绝不是一次静默的空结果被读成「没有这个工具」。
- **不要试图说服模型传 `server`。** 实践中它不传——收窄只会缩小它自己的搜索空间，它没理由想那样——而 `select:` 是常规用法，因为 `server` 只是限制模型自己的搜索空间，收益（Jev 那笔账单）又是它感知不到的。更窄的搜索必须在**代码**里推断出来，不能在散文里要求。
- **收窄不会让清理出来的 cut-off 失效**（它移除候选，不改写问题）：这套推理在 [jev.md](jev.md) 的「测一个阈值」一节，满池搜索的代价和降级规则（预算、失败、未作答的问题）也在那里。
- **每个判定请求留下一行日志**（`jev_search.go` 的 `logAnswers`）：query、作答的模型、chunk i/n、作用域、阈值、token 数，然后是概率序的 `answers="name=0.912, …"`——未作答的问题写成 `name=-`，永远不是 `0.000`（没被判定不等于被判定为低）。
- **没有 session ID 的一次运行记不下它找到的东西，这是已知缺口，不是一条受支持的路径。** `Manager.TrackerFor("")` 正是为此返回 nil（那个空 ID 检查是为了避免把一个类型化 nil 装箱），而同一个 ID 键又用来构建 NEXT 请求的工具列表（`filterActiveSchemas`）——所以一次旁路运行（dream、`/review`、一次 ambient 轮次）永远拿不到找到的工具进它的工具列表，而 executor 仍会在首次使用时从池里注册它（`lazyRegisterMCPTool`），也就是没有 schema 可以拿来校验参数。`TrackerFor` 上的注释带着同样的警告。

## 模型读到的结果

- **结果是一份菜单：`{"matches": [{"name", "matched_by", "score"}]}`，别的什么都没有。** 没有 description，没有参数 schema，没有 `tool_definitions` 块，没有回显的 `query`，没有计数。这些对**调用**一个工具一个都不需要：被找到的工具带着池子里的完整 schema 进入**下一个**请求的工具列表（`filterActiveSchemas`），一次调用也从同一个池子注册它（`lazyRegisterMCPTool`）。那些定义曾经占 payload 的一半，而 payload 会在会话剩下的时间里一直躺在会话全文里——所以 `TestMCPSearchToolsTool_Execute_Success` 钉住的是它**不在那里**。把其中任何一点加回来看着无害，却让每次搜索的负载翻倍，而且是永久性的。
- **`matched_by` 永远和 `score` 一起出现**（`name` | `judgment` | `keywords`）：noul 概率和 BM25 分数是不同的量，一个裸数字会招来跨搜索的、毫无意义的比较。名字匹配压根不带分数（查表不排名）。分数四舍五入到三位小数，与判定自己那行日志用的精度一致。
- **给分数定性的那个子句（`tools.ScoreAdvice`）是承重的，不能和其他 schema 文字一起被裁掉**：每个结果都已经过了 cut-off，所以一个把低分读成判决的模型，丢掉的恰好是阈值扫出来要纳入的那个边缘工具，然后花一整轮再搜一次。**它当前还没接上线**：`MCPSearchToolsTool.Description()` 只拼 `advice()` 与 `ExactNameAdvice`，这个常量在全仓库没有引用——这句话还没到模型眼前。

## provider 看到的 schema

- **一个 MCP 工具的参数类型，必须以一个纯字符串到达 provider；一个没点名任何类型的属性，必须不带那个关键字到达。** server 把可空参数声明成 union（`["null","array"]`），或者声明成一个完全没有 `type` 的组合（pydantic 对 `Optional[int]` 发出 `anyOf:[{type:integer},{type:null}]`），所以 `agent/mcp/tool.go` 的 `propertyType` 把两者都解析到 union 的非 null 成员；只有 `$ref` 属性保持无类型，而 `llm.ToolParameterProperty.Type` 是 `omitempty`，因为 `"type": ""` 是非法 JSON Schema——一个这样的属性会让 provider 拒掉**整个**请求，列表里每个工具都跟着它一起死，不只是出问题的那一个（`TestBuildLLMTools_EmptyTypeNeverReachesTheProvider`、`TestMCPToolProperties_ComposedSchemaType`）。

## 加载的代价，以及那张预算卡片

- **加载一个工具，是唯一一种会把整段提示词重读一次的变化。** 工具是前缀的第一段（tools → system → messages），所以一个不在上一个请求里的工具，会把它位置之后的一切都变成 miss——而消息在工具后面。`mcp_tool_search.cache_budget`（人民币元/次搜索，0 = 关闭）是用户接受的数：超过它，这次调用先要一次确认，TUI、ACP、desktop 三条路径都一样（`agent/mcp_budget.go`，被两条 executor 路径调用）。
- **估算值是 `tokens × (input + cache-creation − cache-read)` 每 1M**，其中 `tokens` 来自 `convState.contextEstimate()`——那个锚在真实值上的数字，不是原始的字符估算，因为预算决策正是那个估算已知的偏差会变成错误金额的地方——价格用 `ResolveModelPriceAt(now)` 钉住（含分时电价段）。未定价的模型，或 delta ≤ 0（缓存读至少和 miss 一样贵），都不弹卡：卡上没有数字可放，而编一个出来比不问更糟。
- **只有当一次搜索真有可能挪动工具列表时，它才会问**：池中未发现的工具为零（池减去该会话的 discovered 集合，键是构建下一个请求列表用的同一个 session ID，`tools.SessionIDFromCtx`）意味着一次搜索只能返回已经在里面的名字。某个特定 query 会不会真加载东西，是搜索自己的答案，所以卡片说的是「可能加载」。
- **它是一道**成本**门禁，不是安全门禁，由此有三条性质。** `PermissionModeSkip` 从不拦——在 channel / `tachi -p` / 一个 subagent 里没人在控制台前，而一个无人可答的花钱问题不是问题，是挂起。拒绝会返回一个工具**结果**，这一轮继续（bash 的询问则是拿 `errCancelled` 中止），因为模型才是那个必须知道工具为什么没了的读者。而且这次拒绝会被本会话记住（`PermissionState.MCPSearchBlocked`）：一次下次调用再问一次就能撤销的拒绝，是一颗兑现不了的按钮。「本会话允许」是它的对偶——TUI 以 `ConfirmAllowSession` 到达 agent，ACP/desktop 的处理方只回一个 bool，自己调 `AllowMCPSearchForSession()`。**两者都由 `ClearSession`（`/new`）清掉，就在 bash 策略自己的会话批准旁边**：一个答案属于给出它的那段对话，一次活得比它那段对话还久的拒绝，会让改了主意的用户除了重启没有回头路。desktop 每个会话建一个 agent，所以不调那次也拿到全新的门禁。
- **询问按 agent 串行化（`PermissionState.askMu`）。** bash 从不需要这个（它的工具非并行，询问本来就一次一个）；MCPSearchTools 是并行的，所以同一组里的两次搜索可以同时停在那里——而 TUI 通过唯一那个 channel 回答每一张卡（`Channels.ConfirmResp`，容量 1），否则第二张卡会吃掉第一张的答案。
- **每次调用都留下一行日志，有卡没卡都留**：`Agent: MCP search cost gate`，带 `decision`（proceed/ask/refuse）+ `reason` + `tokens` + `cost` + `budget` + `undiscovered` + `priced`；一次询问再留第二行（`... gate answered`），带着用户的选择。所以它需要的事实都在提前返回之前先收集好。这正是这道门禁可审计的原因：「它没问」和「没东西可加载」和「这个模型没有价表」从外面看本来是同一件没发生的事，而那个估算——整个特性所讲的那个数字——别处都不存在。
- **卡片的 chrome 按种类分，正文从不分。** `desktop.PermissionEvent.Kind`（`bash` / `mcp_search`，由工具名推得）决定标题和按钮提示；preview 仍然是 agent 自己的渲染，原样展示——与让 `askPreview` 成为它那个格式唯一写入者的同一条规则。TUI 按工具名分叉出同样的两个字符串。整张卡（文案、上面的数字、一次跨第二次调用的拒绝）由 desktop 冒烟场景 `perm-mcp-budget` 端到端钉住。

Related: [`.tachi.md`](../../.tachi.md), [desktop.md](desktop.md)（MCP client、OAuth、面板）、[jev.md](jev.md)（那道门禁及其阈值纪律）、[prompt-cache.md](prompt-cache.md)（提醒块的接缝）。
