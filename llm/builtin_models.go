package llm

import (
	"strings"
	"time"

	"github.com/monsterxx03/tachi/config"
)

// === 内置模型注册表 ==========================================================
// 单一数据源：一条记录 = 一个模型/家族的完整内置能力（价格/上下文/视觉/
// 推理档位）。五个查询入口（GetBuiltinModelPriceAt / ModelContextWindow /
// ModelSupportsVision / ModelThinkingEfforts / ModelSupportsReasoningEffortNone）
// 共享同一条查找路径 lookup()——加内置模型 = 加一条记录，改能力 = 改一条记录，
// 不再需要在多个函数里同步修改匹配逻辑。
//
// 价格单位是 CNY / 1M token（ledger 的单位）。厂商以美元报价的记录置
// usdPrices：单价按 USD/M 写入，解析时按配置汇率换算（见 DefaultUSDToCNY）。
//
// 匹配语义（有序、首条命中）：
//   - match 是子串（小写）匹配，任一命中即可；require 里的条件须全部命中（"且"）。
//   - 变体记录必须排在它所属的家族记录之前（TestBuiltinModelOrder 固定此约束）。
//   - 记录自包含、无继承：命中即终态，所有字段在命中时必须已是最终值。
//
// 未知模型 = 未命中任何记录：无价格（nil）/ 上下文 0 / 视觉 false / 档位不过滤。

type builtinModel struct {
	match   []string              // 任一子串命中（小写）即匹配
	require []string              // 附加条件，须全部命中（nil = 无）
	context int64                 // 上下文窗口（0 = 未知）
	vision  bool                  // 视觉支持
	prices  []builtinPriceVersion // 价格版本（nil = 未定价）

	// usdPrices 表示 prices 的四个单价是 USD / 1M token，而不是本表的
	// CNY——厂商的价目表用什么货币，就照抄什么货币，换算留给解析时：
	// 汇率是用户可配的（config 的 usd_to_cny），在这里烤进数字等于把
	// 一个会变的数变成常量。
	usdPrices bool

	// thinkingEfforts 是该模型 API 接受的思考档位（厂商文档口径）。
	// nil = 不做声明，即全部档位可用——与引入本字段之前的行为一致，所以
	// 只有文档明确说明档位范围的模型才写。
	//
	// 为什么必须是「文档口径」而不是「我们这条线路的口径」：同一个网关可能
	// 在不同接口上映射得不同（实测某网关的 chat-completions 路径拒绝 max、
	// Responses 路径接受），而这张表描述的是模型本身。线路差异由 provider 配置
	// 的 spec.thinking_efforts 覆盖（见 llm.ResolvedProvider.ThinkingEfforts），
	// 显式配置优先——同 spec.vision 与 ModelSupportsVision 的关系。
	//
	// 一个被拒绝的档位不是被忽略，而是整单失败（400），所以这条事实同时决定
	// 前端让你选什么、以及 ModelSupportsReasoningEffortNone 的答案。
	thinkingEfforts []string
}

// gpt6EffortsWithoutNone 是 gpt-6 里那两个 API 拒绝 none/minimal 的变体
// （Astra / 6.1 Sol）接受的全部档位：官方文档给的是 low..max。没有 none——
// 这类模型无法关掉推理，送 none 是整单失败。luna / sol 没有这条限制，
// 记录里不写 thinkingEfforts（= 不声明 = 全部可用）。
//
// Source: https://developers.openai.com/api/docs/models/gpt-6-astra
//
//	https://developers.openai.com/api/docs/models/gpt-6.1-sol
var gpt6EffortsWithoutNone = []string{"low", "medium", "high", "xhigh", "max"}

// builtinModels 是内置模型注册表，按声明顺序匹配（变体在前、家族兜底在后）。
var builtinModels = []builtinModel{
	// ---- Anthropic Claude ----
	{
		match:   []string{"claude-sonnet-4-6", "claude-opus"},
		context: 1_000_000,
		vision:  true,
	},
	{
		match:   []string{"claude"},
		context: 200_000, // 未知 Claude 变体的保守值
		vision:  true,    // Claude 全家族支持图像输入
	},

	// ---- OpenAI ----
	// gpt-6 家族的变体记录必须排在家族（"gpt-6"）之前，且都在最下面的 "gpt"
	// 兜底之前——匹配是有序、首条命中的子串匹配。"gpt-6.1-sol" 不含子串
	// "gpt-6-sol"，反之亦然，但 6.1 的记录仍放在 gpt-6 家族记录之前，读起来
	// 才知道谁覆盖谁。
	//
	// Astra 与 6.1 Sol 是 gpt-6 里两个不接受 reasoning.effort = none 的模型
	// （官方文档：Astra 支持 low..max；6.1 Sol 明确不支持 none/minimal）——
	// 同一个事实写一次（thinkingEfforts），前端据此不给这个档位，
	// ModelSupportsReasoningEffortNone 也读它。
	{
		match:           []string{"gpt-6-astra"},
		context:         1_050_000,
		vision:          true,
		prices:          gpt6AstraPriceVersions,
		usdPrices:       true,
		thinkingEfforts: gpt6EffortsWithoutNone,
	},
	{
		match:           []string{"gpt-6.1-sol"},
		context:         1_050_000,
		vision:          true,
		prices:          gpt6Sol61PriceVersions,
		usdPrices:       true,
		thinkingEfforts: gpt6EffortsWithoutNone,
	},
	{
		match:     []string{"gpt-6-luna"},
		context:   1_050_000,
		vision:    true,
		prices:    gpt6LunaPriceVersions,
		usdPrices: true,
	},
	{
		match:     []string{"gpt-6-sol"},
		context:   1_050_000,
		vision:    true,
		prices:    gpt6SolPriceVersions,
		usdPrices: true,
	},
	{
		// 未知 gpt-6 变体：家族已知的上下文/视觉照给，价格不给——变体的价
		// 只有官方页面才能确定（Luna 与 Astra 差 100 倍），猜一个会把账
		// 算错而不是留白。
		match:   []string{"gpt-6"},
		context: 1_050_000,
		vision:  true,
	},
	{
		match:   []string{"gpt-5.4", "gpt-5.5"},
		context: 1_050_000,
		vision:  true,
	},
	{
		match:   []string{"gpt-4o", "gpt-4.1", "gpt-4-turbo", "gpt-4-vision", "gpt-5"},
		context: 400_000,
		vision:  true,
	},
	{
		match:  []string{"o4"},
		vision: true, // o4 系上下文未知 → 0（保持既有行为）
	},
	{
		match:   []string{"gpt"},
		context: 400_000, // 未知 GPT 变体的保守值
	},

	// ---- 阿里 Qwen ----
	{
		match:   []string{"qwen"},
		require: []string{"vl"},
		context: 1_000_000,
		vision:  true, // qwen-vl / qwen2.5-vl / qwen3-vl … 自动覆盖未来 vl 变体
	},
	{
		match:   []string{"qwen"},
		context: 1_000_000, // 纯文本 qwen
	},

	// ---- 智谱 GLM ----
	{
		// 变体必须在家族记录之前——"glm-5.3-flash" 也含子串 "glm-5.3"。
		// Source: https://bigmodel.cn/pricing
		match:   []string{"glm-5.3-flash"},
		context: 1_000_000,
		vision:  true, // 输入模态：图片、视频、文件、文本
		prices:  glm53FlashPriceVersions,
	},
	{
		match:   []string{"glm-5.3", "glm-5.2"},
		context: 1_000_000,
		prices:  glm52PriceVersions,
	},
	{
		match:   []string{"glm-4v", "glm-4.1v", "glm-4.5v"},
		context: 200_000,
		vision:  true,
	},
	{
		match:   []string{"glm"},
		context: 200_000, // 其余 GLM 系列保守值
	},

	// ---- MiniMax ----
	{
		match:   []string{"minimax-m3"},
		context: 1_000_000, // 1M 上下文 + 原生多模态(text/vision/video)
		vision:  true,
		prices:  minimaxM3PriceVersions,
	},
	{
		match:   []string{"minimax-m2.7-highspeed"},
		context: 204_800,
		vision:  true,
		prices:  minimaxM27HighspeedPriceVersions,
	},
	{
		match:   []string{"minimax"},
		context: 204_800,
		vision:  true,
		prices:  minimaxM27PriceVersions, // 未知变体兜底 = M2.7 价（保守默认）
	},

	// ---- 月之暗面 Kimi ----
	{
		match:   []string{"kimi-k3"},
		context: 1_000_000,
		vision:  true,
		prices:  kimiK3PriceVersions,
	},
	{
		match:   []string{"kimi"},
		context: 256_000,
		vision:  true, // 全家族支持图像输入
	},

	// ---- 小米 MiMo ----
	{
		match:   []string{"mimo-v2.5-pro"},
		context: 1_000_000, // 与 v2.5 同系列（官方文档未单列，按系列推断）
		vision:  true,
		prices:  mimoV25ProPriceVersions,
	},
	{
		match:   []string{"mimo-v2.5", "mimo-2.5"}, // "mimo-2.5" 为历史别名（旧代码漏 v 的写法）
		context: 1_000_000,
		vision:  true,
		prices:  mimoV25PriceVersions,
	},
	{
		match:  []string{"mimo"},
		vision: true, // 其他 MiMo 变体（vl/7b 等）支持图像输入
	},

	// ---- DeepSeek ----
	{
		match:   []string{"deepseek-v4-pro"},
		context: 1_000_000,
		prices:  deepseekProPriceVersions,
	},
	{
		match:   []string{"deepseek-v4-flash-vision-exp"},
		context: 1_000_000,
		vision:  true,
		prices:  deepseekFlashPriceVersions,
	},
	{
		// flash 的简写别名（连字符/下划线两种写法）：自建网关或配置里常见的
		// provider/model 名，价格与上下文同 deepseek-v4-flash，但支持图像输入
		// ——故必须排在 {"deepseek"} 兜底之前，否则拿到兜底的 vision:false。
		match:   []string{"deepseek-flash", "deepseek_flash"},
		context: 1_000_000,
		vision:  true,
		prices:  deepseekFlashPriceVersions,
	},
	{
		match:   []string{"deepseek"},
		context: 1_000_000,
		prices:  deepseekFlashPriceVersions, // 未知变体兜底 = flash 价（保守默认）
	},

	// ---- Gemini ----
	{
		match:  []string{"gemini"},
		vision: true, // 全家族支持图像输入
	},

	// ---- TypeSafe System One（Jev）----
	// Jev 不是 provider（不实现 llm.Provider），但它的调用会进同一个成本台账
	// （agent/mcp/jev_search.go 自己写行），所以价格必须在这里有一条记录。
	{
		match: []string{"jev"},
		// 一次请求 64k，其中 state + 最长问句 32k（pkg/jev 的包注释）。
		context: 64_000,
		// 输出不计费：计费只按 input（官方价目表：$0.042/M 输入，输出免费）。
		// 厂商按 USD 报价 → usdPrices，换算在解析时按汇率做（见 DefaultUSDToCNY）。
		prices:    []builtinPriceVersion{{Price: ModelPrice{InputPrice: 0.042}}},
		usdPrices: true,
	},
}

// DefaultUSDToCNY is the rate used to convert a USD-quoted price record into
// this table's unit (CNY per 1M tokens) when the config sets none.
//
// The registry is written in the vendor's own currency: the vendors it prices
// mostly bill in CNY, and the ones that publish in USD (OpenAI's gpt-6 family,
// TypeSafe/Jev) say so with builtinModel.usdPrices rather than baking a rate
// into the number. 6.73 is a SNAPSHOT (2026-09) of a number that moves — it is
// the fallback so the ledger can show a cost at all, and the configured rate
// (config's usd_to_cny) is what a reader who cares should set.
const DefaultUSDToCNY = 6.73

// USDToCNYRate returns the conversion rate to price USD-quoted records with:
// the configured one, else DefaultUSDToCNY. A nil config or an unset/absurd
// rate (<= 0) falls back, so no caller has to decide what a zero rate means.
//
// It is read per price resolution, not captured — the desktop writes its
// config live, and a rate cached at startup would silently price the rest of
// the process with the launch-day one.
func USDToCNYRate(cfg *config.Config) float64 {
	if cfg == nil || cfg.USDToCNY == nil || *cfg.USDToCNY <= 0 {
		return DefaultUSDToCNY
	}
	return *cfg.USDToCNY
}

// lookup 返回首个匹配 model 的内置记录；nil = 未知模型。
func lookup(model string) *builtinModel {
	m := strings.ToLower(model)
	for i := range builtinModels {
		if builtinModels[i].matches(m) {
			return &builtinModels[i]
		}
	}
	return nil
}

func (b *builtinModel) matches(m string) bool {
	hit := false
	for _, s := range b.match {
		if strings.Contains(m, s) {
			hit = true
			break
		}
	}
	if !hit {
		return false
	}
	for _, r := range b.require {
		if !strings.Contains(m, r) {
			return false
		}
	}
	return true
}

// === 价格版本数据 =============================================================

// OpenAI gpt-6 家族：官方按 USD 报价（usdPrices），单价照抄官方页面，换算交给
// 解析时的汇率。四个模型的价目表只差数字，形状相同（输入/输出/缓存命中/缓存
// 写入），故各一条记录、无版本。
//
// 共同的两条约定：
//   - 输入 > 272K token 的请求，OpenAI 按整单 2x 输入/缓存价、1.5x 输出价计
//     （官方价目表原文）。本表不建「按输入 token 量分档」的机制，超长请求按
//     标准档计入 = 低估，与 MiniMax-M3（>512k 翻倍）的处理一致。
//   - 缓存命中价各不相同：Luna/Sol/Astra 是输入的 10%，6.1 Sol 是 5%。缓存
//     写入一律是输入的 1.25x，照抄官方给的数。
//
// Source: https://developers.openai.com/api/docs/models/gpt-6-luna
//
//	https://developers.openai.com/api/docs/models/gpt-6-sol
//	https://developers.openai.com/api/docs/models/gpt-6-astra
//	https://developers.openai.com/api/docs/models/gpt-6.1-sol
var gpt6LunaPriceVersions = []builtinPriceVersion{{Price: ModelPrice{
	InputPrice:              0.10,
	OutputPrice:             0.50,
	CacheReadInputPrice:     0.01,
	CacheCreationInputPrice: 0.125,
}}}

var gpt6SolPriceVersions = []builtinPriceVersion{{Price: ModelPrice{
	InputPrice:              2.00,
	OutputPrice:             10.00,
	CacheReadInputPrice:     0.20,
	CacheCreationInputPrice: 2.50,
}}}

var gpt6AstraPriceVersions = []builtinPriceVersion{{Price: ModelPrice{
	InputPrice:              10.00,
	OutputPrice:             50.00,
	CacheReadInputPrice:     1.00,
	CacheCreationInputPrice: 12.50,
}}}

// gpt6Sol61PriceVersions: 6.1 Sol 与 6.0 Sol 同价，只有缓存命中价更低（5% vs
// 10%）——不是笔误，是官方页面上的差别。
var gpt6Sol61PriceVersions = []builtinPriceVersion{{Price: ModelPrice{
	InputPrice:              2.00,
	OutputPrice:             10.00,
	CacheReadInputPrice:     0.10,
	CacheCreationInputPrice: 2.50,
}}}

// tzAsiaShanghai anchors DeepSeek's peak-pricing bands: the official
// schedule is defined in 北京时间. China has no DST since 1991, so a fixed
// +08:00 zone is exact and needs no tzdata.
var tzAsiaShanghai = time.FixedZone("Asia/Shanghai", 8*3600)

// deepSeekPeakEffectiveFrom is 2026-08-17 00:00 北京时间 — when DeepSeek's
// 峰谷定价 (peak/off-peak) takes effect per the official pricing page.
// 高峰时段 = 北京时间 09:00-12:00、14:00-18:00；空闲时段 = 高峰的一半。
// Source: https://api-docs.deepseek.com/zh-cn/quick_start/pricing/
var deepSeekPeakEffectiveFrom = time.Date(2026, 8, 17, 0, 0, 0, 0, tzAsiaShanghai)

// deepSeekWeekdayPeakEffectiveFrom is 2026-08-24 00:00 北京时间 — when the
// peak schedule narrows to WORKDAYS only: 周一至周五 09:00-12:00、14:00-18:00
// 高峰；周六周日全天空闲（= 谷价，回退平段）。官方 8/17 首版峰谷为每天
// 生效，8/24 起调整为工作日峰谷、周末全天谷价。
// Source: https://api-docs.deepseek.com/zh-cn/quick_start/pricing/
var deepSeekWeekdayPeakEffectiveFrom = time.Date(2026, 8, 24, 0, 0, 0, 0, tzAsiaShanghai)

// deepSeekFlashPriceAdjustEffectiveFrom is 2026-09-10 12:00 北京时间 — when
// DeepSeek adjusts the flash 系列（含 deepseek-v4-flash-vision-exp）定价：
// 空闲时段 缓存命中 ¥0.02 / 缓存未命中输入 ¥1 / 输出 ¥4；高峰时段 = 空闲 ×2
// （¥0.04 / ¥2 / ¥8），时段表不变（周一至周五 09:00-12:00、14:00-18:00 高峰，
// 周末全天空闲）。本次仅 flash 调价，deepseek-v4-pro 维持 8/24 版价格。
// 生效时刻为 12:00 而非 00:00：9/10 当天 09:00-12:00 的高峰时段仍按旧价计。
// Source: https://api-docs.deepseek.com/zh-cn/quick_start/pricing/
var deepSeekFlashPriceAdjustEffectiveFrom = time.Date(2026, 9, 10, 12, 0, 0, 0, tzAsiaShanghai)

// DeepSeek 的上下文硬盘缓存（kv_cache）没有"缓存写入费"这一计费项：每个请求
// 自动触发缓存构建（落盘），官方文档只区分命中/未命中两类输入计费。未列写入费
// = 免费 → CacheCreationInputPrice = 0（各版本均适用）。
// Source: https://api-docs.deepseek.com/zh-cn/quick_start/pricing/

// deepseekFlashPriceVersions: 老价（¥1/2/0.02）+ 2026-08-17 起峰谷价
// （空闲 ¥1.5/4.5/0.05，高峰 ¥3/9/0.10）+ 2026-08-24 起工作日峰谷
// （周一至周五高峰，周末全天谷价 = 平段 ¥1.5/4.5/0.05）+ 2026-09-10 12:00 起
// flash 系列调价（空闲 ¥1/4/0.02，高峰 = 空闲 ×2）。
// 本表同时服务 deepseek-v4-flash-vision-exp 与未知 deepseek 变体兜底。
var deepseekFlashPriceVersions = []builtinPriceVersion{
	{
		// 8/16 及以前：flat，无时段。
		Price: ModelPrice{InputPrice: 1.0, OutputPrice: 2.0, CacheReadInputPrice: 0.02},
	},
	{
		EffectiveFrom: deepSeekPeakEffectiveFrom,
		Price: ModelPrice{
			// 平段 = 空闲时段价。
			InputPrice: 1.5, OutputPrice: 4.5, CacheReadInputPrice: 0.05,
			Location: tzAsiaShanghai,
			Bands: []PriceBand{
				// 高峰：09:00-12:00、14:00-18:00（北京时间），空闲 = 高峰一半。
				{Name: "peak", StartHour: 9, EndHour: 12, InputPrice: 3.0, OutputPrice: 9.0, CacheReadInputPrice: 0.10},
				{Name: "peak", StartHour: 14, EndHour: 18, InputPrice: 3.0, OutputPrice: 9.0, CacheReadInputPrice: 0.10},
			},
		},
	},
	{
		EffectiveFrom: deepSeekWeekdayPeakEffectiveFrom,
		Price: ModelPrice{
			// 平段 = 谷价（空闲价）；周末不命中任何 band → 全天谷价。
			InputPrice: 1.5, OutputPrice: 4.5, CacheReadInputPrice: 0.05,
			Location: tzAsiaShanghai,
			Bands: []PriceBand{
				// 高峰仅周一至周五：09:00-12:00、14:00-18:00（北京时间）。
				{Name: "peak", Days: []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday}, StartHour: 9, EndHour: 12, InputPrice: 3.0, OutputPrice: 9.0, CacheReadInputPrice: 0.10},
				{Name: "peak", Days: []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday}, StartHour: 14, EndHour: 18, InputPrice: 3.0, OutputPrice: 9.0, CacheReadInputPrice: 0.10},
			},
		},
	},
	{
		EffectiveFrom: deepSeekFlashPriceAdjustEffectiveFrom,
		Price: ModelPrice{
			// 平段 = 空闲时段价；周末不命中任何 band → 全天空闲价。
			InputPrice: 1.0, OutputPrice: 4.0, CacheReadInputPrice: 0.02,
			Location: tzAsiaShanghai,
			Bands: []PriceBand{
				// 高峰仅周一至周五：09:00-12:00、14:00-18:00（北京时间），
				// 单价 = 空闲时段 ×2。
				{Name: "peak", Days: []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday}, StartHour: 9, EndHour: 12, InputPrice: 2.0, OutputPrice: 8.0, CacheReadInputPrice: 0.04},
				{Name: "peak", Days: []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday}, StartHour: 14, EndHour: 18, InputPrice: 2.0, OutputPrice: 8.0, CacheReadInputPrice: 0.04},
			},
		},
	},
}

// deepseekProPriceVersions: 老价（¥3/6/0.025）+ 2026-08-17 起峰谷价
// （空闲 ¥4.5/13.5/0.15，高峰 ¥9/27/0.30）+ 2026-08-24 起工作日峰谷
// （周一至周五高峰，周末全天谷价 = 平段 ¥4.5/13.5/0.15）。
var deepseekProPriceVersions = []builtinPriceVersion{
	{
		Price: ModelPrice{InputPrice: 3.0, OutputPrice: 6.0, CacheReadInputPrice: 0.025},
	},
	{
		EffectiveFrom: deepSeekPeakEffectiveFrom,
		Price: ModelPrice{
			InputPrice: 4.5, OutputPrice: 13.5, CacheReadInputPrice: 0.15,
			Location: tzAsiaShanghai,
			Bands: []PriceBand{
				{Name: "peak", StartHour: 9, EndHour: 12, InputPrice: 9.0, OutputPrice: 27.0, CacheReadInputPrice: 0.30},
				{Name: "peak", StartHour: 14, EndHour: 18, InputPrice: 9.0, OutputPrice: 27.0, CacheReadInputPrice: 0.30},
			},
		},
	},
	{
		EffectiveFrom: deepSeekWeekdayPeakEffectiveFrom,
		Price: ModelPrice{
			// 平段 = 谷价；周末不命中任何 band → 全天谷价。
			InputPrice: 4.5, OutputPrice: 13.5, CacheReadInputPrice: 0.15,
			Location: tzAsiaShanghai,
			Bands: []PriceBand{
				// 高峰仅周一至周五（北京时间）。
				{Name: "peak", Days: []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday}, StartHour: 9, EndHour: 12, InputPrice: 9.0, OutputPrice: 27.0, CacheReadInputPrice: 0.30},
				{Name: "peak", Days: []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday}, StartHour: 14, EndHour: 18, InputPrice: 9.0, OutputPrice: 27.0, CacheReadInputPrice: 0.30},
			},
		},
	},
}

// glm52PriceVersions: 智谱 GLM-5.3 / GLM-5.2（国内版）同价。
// 缓存存储（写入）限时免费 → CacheCreationInputPrice = 0。
// Source: https://open.bigmodel.cn/pricing
var glm52PriceVersions = []builtinPriceVersion{{Price: ModelPrice{
	InputPrice:          8.0,
	OutputPrice:         28.0,
	CacheReadInputPrice: 2.0,
}}}

// glm53FlashPriceVersions: 智谱 GLM-5.3-Flash（国内版）5 折限时两周促销价。
// 输入 0.4 / 输出 1.4 / cache 命中 0.115；缓存存储限时免费 → CacheCreationInputPrice = 0。
// 促销结束后将切换到常规价 0.8/2.8/0.23，届时再加一条 EffectiveFrom 版本。
// Source: https://bigmodel.cn/pricing
var glm53FlashPriceVersions = []builtinPriceVersion{{Price: ModelPrice{
	InputPrice:          0.4,
	OutputPrice:         1.4,
	CacheReadInputPrice: 0.115,
}}}

// kimiK3PriceVersions: 月之暗面 Kimi K3（国内版）。
// 官方价格表只有三价（输入/输出/缓存命中），未列缓存写入费 = 免费。
// Source: https://platform.kimi.com
var kimiK3PriceVersions = []builtinPriceVersion{{Price: ModelPrice{
	InputPrice:          20.0,
	OutputPrice:         100.0,
	CacheReadInputPrice: 2.0,
}}}

// mimoV25ProPriceVersions: 小米 MiMo-V2.5 Pro（国内版）。
// 缓存写入限时免费 → CacheCreationInputPrice = 0。
// Source: https://mimo.mi.com/docs/zh-CN/price/pay-as-you-go
var mimoV25ProPriceVersions = []builtinPriceVersion{{Price: ModelPrice{
	InputPrice:          3.0,
	OutputPrice:         6.0,
	CacheReadInputPrice: 0.025,
}}}

// mimoV25PriceVersions: 小米 MiMo-V2.5（国内版）。
// 缓存写入限时免费 → CacheCreationInputPrice = 0。
// Source: https://mimo.mi.com/docs/zh-CN/price/pay-as-you-go
var mimoV25PriceVersions = []builtinPriceVersion{{Price: ModelPrice{
	InputPrice:          1.0,
	OutputPrice:         2.0,
	CacheReadInputPrice: 0.02,
}}}

// minimaxM3PriceVersions: MiniMax-M3 paygo 标准价（≤512k 输入永久五折）。
// M3 价格表未列缓存写入费 → 按"未列即不计费" = 0。
// M3 在 > 512k 输入时价格翻倍，但当前按 ≤512k 一档计入（避免按输入 token 量
// 分档的复杂机制；超长请求将按本档低估）。
// Source: https://platform.minimaxi.com/docs/guides/pricing-paygo
var minimaxM3PriceVersions = []builtinPriceVersion{{Price: ModelPrice{
	InputPrice:          2.10,
	OutputPrice:         8.40,
	CacheReadInputPrice: 0.42,
}}}

// minimaxM27HighspeedPriceVersions / minimaxM27PriceVersions: M2.7 系列明码
// 标价缓存写入费 ¥2.625/M（M3 价格表未列，M2.7 是当前主力通用模型）。
var minimaxM27HighspeedPriceVersions = []builtinPriceVersion{{Price: ModelPrice{
	InputPrice:              4.2,
	OutputPrice:             16.8,
	CacheReadInputPrice:     0.42,
	CacheCreationInputPrice: 2.625,
}}}

var minimaxM27PriceVersions = []builtinPriceVersion{{Price: ModelPrice{
	InputPrice:              2.1,
	OutputPrice:             8.4,
	CacheReadInputPrice:     0.42,
	CacheCreationInputPrice: 2.625,
}}}
