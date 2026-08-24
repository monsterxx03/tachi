# 分时计价（Time-of-Use Pricing）设计

> 目标：让计费支持"单价随调用时刻变化"——厂商按时段定价（如 DeepSeek 2026-08-17
> 起峰谷定价）时，账本行按调用时刻的**当时**单价定格。
> 机制：`ModelPrice` 携带时段表（`Bands`）+ 时区基准，`PriceAt(t)` 取时刻快照；
> 内置价目表支持版本化（`EffectiveFrom`）自动切换；账本行新增 `band` 字段。
> 本文是 `docs/2026-08-05-usage-billing.md` 的扩展，与其不变量完全兼容。

---

## 一、背景

DeepSeek 官方宣布 2026-08-17 00:00（北京时间）起采用**峰谷定价**，空闲时段价格为
高峰时段的一半。高峰 = 北京时间 09:00-12:00、14:00-18:00（两个不连续区间）。

| 模型 | 时段 | 缓存命中 | 未命中输入 | 输出 |
|------|------|---------|-----------|------|
| deepseek-v4-flash | 空闲 | 0.05 | 1.5 | 4.5 |
| deepseek-v4-flash | 高峰 | 0.10 | 3.0 | 9.0 |
| deepseek-v4-pro | 空闲 | 0.15 | 4.5 | 13.5 |
| deepseek-v4-pro | 高峰 | 0.30 | 9.0 | 27.0 |

Source: https://api-docs.deepseek.com/zh-cn/quick_start/pricing/

> **8/24 更新**：官方 2026-08-24 00:00（北京时间）起高峰时段收窄为
> **周一至周五**（09:00-12:00、14:00-18:00），周六周日全天空闲（= 谷价）。
> 内置表新增第三版本（`EffectiveFrom: 2026-08-24`），peak band 带
> `Days: Mon-Fri` 过滤，周末回退平段价。8/17-8/23 的「每天峰谷」版本保留，
> 历史行快照不受影响。

这带来三个设计问题，本方案逐一解决：
1. **时段**：高峰是两个不连续区间 → 多个 band 条目，首个匹配生效。
2. **时区**：官方按北京时间分时，不是用户本地时区 → band 判定需要时区基准。
3. **生效日期**：8/17 之前仍是老价 → 内置价目表需要版本化，自动切换。

---

## 二、核心设计：价格 = 时刻的函数

现有账本的核心不变量（§8 of usage-billing.md）：**每行自带价格快照，成本 =
Σ(行内 token × 行内快照)，永不回头查当前 config**。分时计价正是这个语义的自然
延伸——每次调用落行时，快照取"该时刻生效的时段价"。**成本计算侧（`UsageRow.Cost`、
`ComputeSessionUsage`、`tachi usage`、三前端 `/usage`）零改动。**

改造全部集中在**价格解析层**：

```
llm.ModelPrice（平段四价 + Location 时区基准 + Bands 时段表）
        │ PriceAt(t) —— 首个匹配 band → 时段快照 + 时段名；未命中 → 平段快照
        ▼
ResolvedPrice{Price 定格快照, Band 时段名}  ← PriceResolver 的新返回类型
        │
        ▼
RecordingProvider.record → UsageRow（价格快照 + band 字段）
```

### llm 层（llm/pricing.go）

```go
type PriceBand struct {
    Name       string  // 时段名，写进账本行 band 字段（审计）
    Days       []time.Weekday // 星期过滤（空 = 每天；Monday=1 … Sunday=7 的 Go 约定）
    StartHour  int     // 0-23，含
    EndHour    int     // 0-24，不含；EndHour <= StartHour 跨午夜（EndHour==StartHour = 全天）
    InputPrice, OutputPrice, CacheReadInputPrice, CacheCreationInputPrice float64
}

type ModelPrice struct {
    // ...现有四价...
    Location *time.Location `json:"-"` // 时段判定基准时区；nil = 本地时区
    Bands    []PriceBand    `json:"-"` // 分时时段表；空 = 平段价（行为与旧版完全一致）
}

// PriceAt 返回 t 时刻生效的快照 + 命中的时段名（平段 = 空名）。
// 快照永远不带 Bands——定格后不可再解析（账本/TUI 只消费快照）。
func (p *ModelPrice) PriceAt(t time.Time) (ModelPrice, string)
```

- **半开区间** `[StartHour, EndHour)`：09:00-12:00 含 9 不含 12；跨午夜 23:00-07:00
  覆盖 23、0-6 点。
- **首个匹配生效**：多条目按配置顺序，命中即返回（DeepSeek 两个高峰区间就是两条）。
- **时区**：`PriceAt` 先把 t 转到 `Location`（nil → t 自己的时区）再取**星期+小时**
  ——days 过滤与小时判定共用同一时区基准（周末判定同样锚定厂商时区）。内置
  DeepSeek 表锚定 `time.FixedZone("Asia/Shanghai", 8*3600)`（中国无 DST，零依赖）。

### 版本化内置表

```go
type builtinPriceVersion struct {
    EffectiveFrom time.Time // 生效起点；zero = 一直生效（初始版本）
    Price         ModelPrice
}

// 取 at 时刻生效的版本：最后一个 EffectiveFrom <= at 的版本。
func GetBuiltinModelPriceAt(model string, at time.Time) *ModelPrice
```

DeepSeek flash/pro 各三个版本：老价（无 EffectiveFrom）+ 8/17 起峰谷价
（每天）+ 8/24 起工作日峰谷价（peak band 带 `Days: Mon-Fri`，周末回退平段
= 谷价）。各版本同写进代码，`at` 决定取哪版——**发布不受厂商定价日绑架**，
历史行快照天然正确（8/17 前记老价、8/17-8/23 记每天峰谷、8/24 起记工作日峰谷）。

`GetBuiltinModelPrice(model)` 保留为 `GetBuiltinModelPriceAt(model, time.Now())`
的兼容壳——**生产路径请用 At 版**（账本/TUI 都解析"调用时刻"）。

### 采集层（llm/usage_recorder.go）

```go
type ResolvedPrice struct {
    Price ModelPrice // 已定格快照（时段已应用，Bands 已消费）
    Band  string     // 命中的时段名；"" = 平段
}

type PriceResolver func(provider Provider, model string) ResolvedPrice

// UsageRow 新增：
Band string `json:"band,omitempty"` // 当时命中的时段名；空 = 平段价
```

`record()` 把 `rp.Band` 原样写入行——行自包含："为什么这行是 3 元/1M"直接可审计。
`ResolvedPrice.HasPrice()` 区分"有价/无价"（四价全 0 = 无价，`/usage` 的
"no pricing data" 判断用）。

### 解析层（llm/pricing.go，依赖倒置）

```go
// llm 包拥有全部定价语义；config 只做"用户配置结构 → llm 原始结构"的翻译。
type PriceScheduleSource interface {
    // config.Config 实现（config → llm 依赖已存在，无循环）：
    PricingSchedule(providerName string) (flat PriceOverride, bands []PriceBandSpec, timezone string, ok bool)
}

// 分时核心：at 时刻解析（内置表版本选择 + band 继承合并 + 定格）。
// 账本 resolver（agent.go wrapForUsage）与 TUI statusbar 共用。
func ResolveModelPriceAt(src PriceScheduleSource, providerName, model string, at time.Time) ResolvedPrice
```

收敛动机：`cmds.ResolveModelPriceAt` 原住在 commands 包（因为它接收
`*config.Config`，而 `config → llm` 已存在——若搬进 llm 直接形成
`llm → config → llm` 循环）。解法是**依赖倒置**：llm 定义窄接口
`PriceScheduleSource`，config 实现它——更进一步，**定价配置类型
（`llm.PricingConfig`/`PriceBandSpec`，带 yaml tag）直接定义在 llm**，
`ModelSpec.Pricing` 字段就是 `*llm.PricingConfig`：schema 单一来源，
`Config.PricingSchedule` 零搬运（原样返回），config 只负责 YAML 装载。
`commands` 只剩 `ResolveModelPrice` 一行转发（TUI 入口）。

解析顺序：
1. **平段价**：source `PriceOverride` 四价任一非 nil → 现有 partial-override 语义
   （未设字段 = 0/免费）；四价全 nil 但有 `Bands` → 平段回退内置表（bands-only
   覆盖继承内置平段价）；无 override → 内置 `GetBuiltinModelPriceAt(model, at)`。
2. **时段合并**：source `Bands` **完全替代**内置 bands（override 语义）；每条 band
   未设的价字段**继承平段价**（显式 0 = 免费）；时间解析失败（非整点）或 days 越界
   （非 1-7）→ 跳过该 band（回退平段）并 warn；`timezone` 非空且 IANA 可解析 →
   覆盖判定时区。
3. **定格**：`PriceAt(at)` → `ResolvedPrice{快照, band名}`。

### config（config/config.go）

```yaml
providers:
  - name: deepseek-v4-flash
    spec:
      pricing:
        timezone: "Asia/Shanghai"        # 可选；空 = 本地时区
        input_price: 1.5                 # 平段价 = 谷价（空闲价）
        output_price: 4.5
        cache_read_input_price: 0.05
        bands:                           # 可选；替代内置时段表
          - name: peak
            days: [1,2,3,4,5]            # 可选；1=周一 … 7=周日；空 = 每天。
                                         # 未列出的星期回退平段价（= 周末全天谷价）
            start: "09:00"               # HH:MM，含
            end: "12:00"                 # HH:MM，不含；end<=start 跨午夜
            input_price: 3.0             # 未设字段继承平段价；显式 0 = 免费
            output_price: 9.0
            cache_read_input_price: 0.10
          - name: peak
            days: [1,2,3,4,5]
            start: "14:00"
            end: "18:00"
            input_price: 3.0
            output_price: 9.0
            cache_read_input_price: 0.10
```

---

## 三、语义决策（拍板记录）

| 决策 | 选择 | 理由 |
|------|------|------|
| 时段粒度 | 小时（HH:MM 整点） | 厂商时段均整点；非整点条目解析失败跳过 |
| band 未设字段 | 继承平段价（显式 0 = 免费） | 折扣场景只改想改的价；与平段"partial override 其余=免费"语义**有意不同**（两处注释写明） |
| 不连续时段 | 多条 band，首个匹配 | 无需新机制 |
| 时区 | `pricing.timezone`（IANA），内置 DeepSeek 锚定 Asia/Shanghai | 厂商按官方时区分时 |
| 生效日期 | `EffectiveFrom` 版本化内置表 | 价格切换自动、历史行正确、发布不被厂商日绑架 |
| 跨午夜 | `EndHour <= StartHour` 回绕；End==Start = 全天 | 区间逻辑天然处理 |
| 未命中时段 | 回退平段价，band 名空 | 与"平段 = 默认价"模型一致 |
| 账本行 band 字段 | 做（`json:"band,omitempty"`） | 审计"为什么这行这个价"，为时段小计留口 |
| 星期过滤 | 做：band 级 `days`（数字 1-7，1=周一；空 = 每天） | 厂商按工作日/周末分时（DeepSeek 8/24 起周末全天谷价）；未命中星期回退平段 |

---

## 四、不变量（review 检查清单）

1. **成本侧零改动**：`UsageRow.Cost` / `ComputeSessionUsage` / `summarizeUsage` /
   `tachi usage` / 三前端 `/usage` 全部不动——行内快照计价与"价从哪来"无关。
2. **TUI statusbar 零改动**：`costForUsage` 走 `cmds.ResolveModelPrice`（签名保留），
   内部 `time.Now()` 定格，估算与账本口径一致。
3. **无时段配置的模型行为逐字节不变**：`PriceAt` 无 Bands 时返回平段快照（连
   Location/Bands 都不带）。
4. **快照不追溯**：改 config / 内置表只影响之后的新行（既有不变量 §8）。
5. **Bands 永不序列化**：`json:"-"`；账本行只存定格快照 + band 名。
6. **config bands 完全替代内置 bands**：override 语义，不叠加。
7. **解析失败不破坏调用**：bad band / bad days / bad timezone 跳过，回退平段
   （埋点零风险精神）。
8. **days 空 = 每天**：无 `days` 的 band 行为与加 days 前逐字节一致；8/17-8/23
   内置版本无 Days 过滤，8/24 起才有。

---

## 五、测试要点

- **llm/pricing_test.go**：`PriceAt`（无 bands 恒等 / 首个匹配 / 跨午夜 / 未命中回
  平段 / 快照不带 Bands / days 过滤：工作日命中、周末回平段、无 days = 每天）；
  `GetBuiltinModelPriceAt` 版本切换（8/16 老价 vs 8/17 峰谷价 vs 8/24 工作日峰谷
  ——8/22 周六仍峰谷验证历史快照、8/29 周六回谷价）；峰谷时段选择（含 UTC 同一时刻
  命中北京时段 = 时区锚定验证）。测试全部用**固定时刻**，不依赖 time.Now()。
- **agent/commands/pricing_test.go**：config bands 命中/未命中；继承平段；显式 0 =
  免费；非整点解析失败跳过；bands-only 继承内置平段；timezone（UTC 时刻 vs 北京）；
  config bands 替代内置（8/17 后 10:00 内置是 peak，自定义 bands 时回到平段）。
- **llm/pricing_resolve_test.go**：config days 命中/未命中（工作日 vs 周末）；
  days 越界（0/8）→ 跳过 band 回平段；`parseBandDays`（1-7 映射、7→周日、去重、
  越界报错）。
- **config/pricing_yaml_test.go**：YAML `days` 字段装载。
- **llm/usage_recorder_test.go**：resolver 返回的 band 名写进账本行 + JSON
  `"band"` 字段。
- **回归**：全量 `go test ./...` 37 包绿；`make lint` 过。

---

## 六、改动清单（已实施）

| 文件 | 改动 |
|------|------|
| `llm/pricing.go` | `PriceBand` / `ModelPrice.Location+Bands` / `PriceAt` / `builtinPriceVersion`+`GetBuiltinModelPriceAt` / DeepSeek 峰谷价（flash+pro，8/17 起，Asia/Shanghai） |
| `llm/pricing.go`（8/24） | `PriceBand.Days` 星期过滤 + `matches(weekday, hour)` / `buildPriceBand`+`parseBandDays`（1-7，空 = 每天，越界跳过） |
| `llm/builtin_models.go`（8/24） | DeepSeek 第三价格版本：8/24 起工作日峰谷（peak band 带 Days Mon-Fri），周末全天谷价 |
| `config/provider_defs.go`（8/24） | `PriceBandSpec.Days []int`（yaml `days,omitempty`，1-7） |
| `llm/usage_recorder.go` | `ResolvedPrice`+`HasPrice` / `PriceResolver` 返回类型 / `UsageRow.Band` / record 写 band |
| `config/config.go` | `ModelSpec.Pricing` 类型改为 `*llm.PricingConfig`（config 的 `ModelPricing`/`PriceBandConfig` 删除——schema 单源） |
| `config/resolve.go` | `Config.PricingSchedule`（实现 `llm.PriceScheduleSource`，原样返回、零搬运） |
| `agent/commands/pricing.go` | `ResolveModelPrice` 薄壳（转发 `llm.ResolveModelPriceAt`，TUI 入口） |
| `agent/agent.go` | `wrapForUsage` 闭包改用 `llm.ResolveModelPriceAt(cfg, ..., time.Now())` |
| 测试 | llm（fake source 解析语义 + 星期机制 + 8/24 版本切换）+ config（PricingSchedule 接线 + YAML 装载 days）+ commands（接线；`pricesEqual` 改 `reflect.DeepEqual` 因 PriceBand 含 slice） |

## 后续（未实现，已知待办）

- 账本 `band` 字段当前是**只写不读**：`/usage` / `tachi usage` 尚未按
  时段聚合/展示。行内已携带 band，随时可加"时段小计"（按 `row.Band` 分组），
  一期不做的理由：分时计价的正确性不依赖展示，先保证记账对。
- `GetBuiltinModelPriceAt` 的版本切片必须保持 `EffectiveFrom` 升序（代码注释
  已写明不变量，DeepSeek 版本测试兜底；未加 init 断言，避免过度防御）。
