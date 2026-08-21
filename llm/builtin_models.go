package llm

import (
	"strings"
	"time"
)

// === 内置模型注册表 ==========================================================
// 单一数据源：一条记录 = 一个模型/家族的完整内置能力（价格/上下文/视觉）。
// 三个查询入口（GetBuiltinModelPriceAt / ModelContextWindow / ModelSupportsVision）
// 共享同一条查找路径 lookup()——加内置模型 = 加一条记录，改能力 = 改一条记录，
// 不再需要在多个函数里同步修改匹配逻辑。
//
// 匹配语义（有序、首条命中）：
//   - match 是子串（小写）匹配，任一命中即可；require 里的条件须全部命中（"且"）。
//   - 变体记录必须排在它所属的家族记录之前（TestBuiltinModelOrder 固定此约束）。
//   - 记录自包含、无继承：命中即终态，所有字段在命中时必须已是最终值。
//
// 未知模型 = 未命中任何记录：无价格（nil）/ 上下文 0 / 视觉 false。

type builtinModel struct {
	match   []string              // 任一子串命中（小写）即匹配
	require []string              // 附加条件，须全部命中（nil = 无）
	context int64                 // 上下文窗口（0 = 未知）
	vision  bool                  // 视觉支持
	prices  []builtinPriceVersion // 价格版本（nil = 未定价）
}

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
		match:   []string{"deepseek"},
		context: 1_000_000,
		prices:  deepseekFlashPriceVersions, // 未知变体兜底 = flash 价（保守默认）
	},

	// ---- Gemini ----
	{
		match:  []string{"gemini"},
		vision: true, // 全家族支持图像输入
	},
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

// tzAsiaShanghai anchors DeepSeek's peak-pricing bands: the official
// schedule is defined in 北京时间. China has no DST since 1991, so a fixed
// +08:00 zone is exact and needs no tzdata.
var tzAsiaShanghai = time.FixedZone("Asia/Shanghai", 8*3600)

// deepSeekPeakEffectiveFrom is 2026-08-17 00:00 北京时间 — when DeepSeek's
// 峰谷定价 (peak/off-peak) takes effect per the official pricing page.
// 高峰时段 = 北京时间 09:00-12:00、14:00-18:00；空闲时段 = 高峰的一半。
// Source: https://api-docs.deepseek.com/zh-cn/quick_start/pricing/
var deepSeekPeakEffectiveFrom = time.Date(2026, 8, 17, 0, 0, 0, 0, tzAsiaShanghai)

// DeepSeek 的上下文硬盘缓存（kv_cache）没有"缓存写入费"这一计费项：每个请求
// 自动触发缓存构建（落盘），官方文档只区分命中/未命中两类输入计费。未列写入费
// = 免费 → CacheCreationInputPrice = 0（各版本均适用）。
// Source: https://api-docs.deepseek.com/zh-cn/quick_start/pricing/

// deepseekFlashPriceVersions: 老价（¥1/2/0.02）+ 2026-08-17 起峰谷价
// （空闲 ¥1.5/4.5/0.05，高峰 ¥3/9/0.10）。
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
}

// deepseekProPriceVersions: 老价（¥3/6/0.025）+ 2026-08-17 起峰谷价
// （空闲 ¥4.5/13.5/0.15，高峰 ¥9/27/0.30）。
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
}

// glm52PriceVersions: 智谱 GLM-5.3 / GLM-5.2（国内版）同价。
// 缓存存储（写入）限时免费 → CacheCreationInputPrice = 0。
// Source: https://open.bigmodel.cn/pricing
var glm52PriceVersions = []builtinPriceVersion{{Price: ModelPrice{
	InputPrice:          8.0,
	OutputPrice:         28.0,
	CacheReadInputPrice: 2.0,
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
